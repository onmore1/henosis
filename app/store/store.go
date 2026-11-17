// Package store is the server's data layer: users, projects, members, locks,
// blobs and snapshots, all kept as plain files under one data folder.
//
//	<dir>/users.json
//	<dir>/projects/<id>/project.json      settings, members, head, lock, invites
//	<dir>/projects/<id>/snapshots/<id>.json
//	<dir>/projects/<id>/blobs/<sha256>    file contents and manifests
//
// Every change is made under one mutex and written with an atomic rename, so
// the folder is always consistent and can be backed up by copying it.
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"henosis/core"
)

var (
	ErrNotFound      = core.ErrNotFound
	ErrLockLost      = core.ErrLockLost
	ErrNotAtHead     = core.ErrNotAtHead
	ErrForbidden     = errors.New("forbidden")
	ErrInviteInvalid = errors.New("the invite code is used, expired or unknown")
	ErrBadRequest    = errors.New("bad request")
	ErrBlobMismatch  = errors.New("the uploaded bytes do not match the hash")
)

// LockHeldError is returned when another member holds a live lock.
type LockHeldError struct {
	Holder string
}

func (e *LockHeldError) Error() string { return "the project is locked by " + e.Holder }

// Is makes errors.Is(err, core.ErrLockHeld) true for this error.
func (e *LockHeldError) Is(target error) bool { return target == core.ErrLockHeld }

// User is an account. Only the hash of its token is stored.
type User struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	TokenSHA256 string `json:"token_sha256"`
	CreatedAt   string `json:"created_at"`
}

type member struct {
	UserID   string `json:"user_id"`
	Role     string `json:"role"` // owner | editor
	JoinedAt string `json:"joined_at"`
}

type lock struct {
	UserID      string    `json:"user_id"`
	Device      string    `json:"device"`
	AcquiredAt  time.Time `json:"acquired_at"`
	HeartbeatAt time.Time `json:"heartbeat_at"`
	RequestedBy string    `json:"requested_by,omitempty"` // name of a member asking for the lock
}

type invite struct {
	CodeSHA256 string    `json:"code_sha256"`
	ExpiresAt  time.Time `json:"expires_at"`
	Used       bool      `json:"used"`
}

type project struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	OwnerID       string         `json:"owner_id"`
	Members       []member       `json:"members"`
	RetentionDays int            `json:"retention_days"`
	Head          string         `json:"head_snapshot_id"`
	NextSeq       int            `json:"next_seq"`
	Lock          *lock          `json:"lock"`
	Invites       []invite       `json:"invites"`
	CreatedAt     string         `json:"created_at"`
	Comments      []core.Comment `json:"comments,omitempty"`

	snapshots map[string]core.Snapshot
}

// The API view types are defined in core so the client can share them.
type (
	UserView    = core.UserView
	LockView    = core.LockView
	MemberView  = core.MemberView
	ProjectView = core.ProjectView
)

// settings is server.json: choices that apply to the whole server.
type settings struct {
	// ShareAll makes every account a member of every project. The server
	// built into the app runs this way: a session is a group of friends.
	ShareAll bool `json:"share_all"`
	// JoinSHA256 is the hash of the session code's secret. Whoever knows the
	// secret may create an account. Empty means joining is off.
	JoinSHA256 string `json:"join_sha256,omitempty"`
}

// Store holds all server state. Create one with Open.
type Store struct {
	dir string
	mu  sync.Mutex

	users    map[string]*User // by id
	byToken  map[string]*User // by token hash
	projects map[string]*project

	usersStamp time.Time // modification time of users.json when it was last read
	settings   settings

	// Now is the clock; tests replace it.
	Now func() time.Time
	// StaleAfter is how long a lock survives without a heartbeat.
	StaleAfter time.Duration
	// BlobGrace is the minimum age of an unreferenced blob before cleanup
	// deletes it. It protects uploads whose commit has not happened yet.
	BlobGrace time.Duration
}

const (
	defaultRetentionDays = 3
	inviteLifetime       = 7 * 24 * time.Hour
)

// Open loads the data folder, creating it if needed.
func Open(dir string) (*Store, error) {
	s := &Store{
		dir: dir, users: map[string]*User{}, byToken: map[string]*User{}, projects: map[string]*project{},
		Now: time.Now, StaleAfter: 10 * time.Minute, BlobGrace: 24 * time.Hour,
	}
	if err := os.MkdirAll(filepath.Join(dir, "projects"), 0o755); err != nil {
		return nil, err
	}
	if err := s.loadUsers(); err != nil {
		return nil, fmt.Errorf("users.json: %w", err)
	}
	if err := readJSON(filepath.Join(dir, "server.json"), &s.settings); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("server.json: %w", err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "projects"))
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		p := &project{snapshots: map[string]core.Snapshot{}}
		pdir := filepath.Join(dir, "projects", entry.Name())
		if err := readJSON(filepath.Join(pdir, "project.json"), p); err != nil {
			return nil, fmt.Errorf("project %s: %w", entry.Name(), err)
		}
		files, _ := os.ReadDir(filepath.Join(pdir, "snapshots"))
		for _, f := range files {
			var snap core.Snapshot
			if strings.HasSuffix(f.Name(), ".json") && readJSON(filepath.Join(pdir, "snapshots", f.Name()), &snap) == nil {
				p.snapshots[snap.ID] = snap
			}
		}
		s.projects[p.ID] = p
	}
	return s, nil
}

// --- helpers ---------------------------------------------------------------

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// writeJSON writes to a temporary file and renames it into place.
func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, data)
}

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".new"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the system random source is unavailable
	}
	return hex.EncodeToString(b)
}

func newID() string {
	h := randomHex(16)
	return h[0:8] + "-" + h[8:12] + "-4" + h[13:16] + "-a" + h[17:20] + "-" + h[20:32]
}

func hashHex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func (s *Store) stamp() string { return s.Now().UTC().Format(time.RFC3339) }

func (s *Store) projectDir(id string) string { return filepath.Join(s.dir, "projects", id) }

func (s *Store) blobPath(projectID, sha string) string {
	return filepath.Join(s.projectDir(projectID), "blobs", sha)
}

// loadUsers reads users.json. A missing file means no users yet.
func (s *Store) loadUsers() error {
	path := filepath.Join(s.dir, "users.json")
	var users []*User
	if err := readJSON(path, &users); err != nil && !os.IsNotExist(err) {
		return err
	}
	s.users, s.byToken = map[string]*User{}, map[string]*User{}
	for _, u := range users {
		s.users[u.ID] = u
		s.byToken[u.TokenSHA256] = u
	}
	if info, err := os.Stat(path); err == nil {
		s.usersStamp = info.ModTime()
	}
	return nil
}

// usersChangedOnDisk reports whether another process (the adduser command)
// has rewritten users.json since it was last read.
func (s *Store) usersChangedOnDisk() bool {
	info, err := os.Stat(filepath.Join(s.dir, "users.json"))
	return err == nil && !info.ModTime().Equal(s.usersStamp)
}

func (s *Store) saveUsers() error {
	list := make([]*User, 0, len(s.users))
	for _, u := range s.users {
		list = append(list, u)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt+list[i].ID < list[j].CreatedAt+list[j].ID })
	path := filepath.Join(s.dir, "users.json")
	if err := writeJSON(path, list); err != nil {
		return err
	}
	if info, err := os.Stat(path); err == nil {
		s.usersStamp = info.ModTime()
	}
	return nil
}

func (s *Store) saveProject(p *project) error {
	return writeJSON(filepath.Join(s.projectDir(p.ID), "project.json"), p)
}

func (p *project) member(userID string) *member {
	for i := range p.Members {
		if p.Members[i].UserID == userID {
			return &p.Members[i]
		}
	}
	return nil
}

func (p *project) shared() bool { return len(p.Members) > 1 }

// memberProject returns the project if the user belongs to it. Callers that
// are not members get ErrNotFound, so they cannot tell that it exists.
func (s *Store) memberProject(userID, projectID string) (*project, *member, error) {
	p := s.projects[projectID]
	if p == nil {
		return nil, nil, ErrNotFound
	}
	m := p.member(userID)
	if m == nil {
		return nil, nil, ErrNotFound
	}
	return p, m, nil
}

func (s *Store) userView(id string) UserView {
	if u := s.users[id]; u != nil {
		return UserView{ID: u.ID, Name: u.Name}
	}
	return UserView{ID: id, Name: "unknown"}
}

func (s *Store) lockStale(l *lock) bool { return s.Now().Sub(l.HeartbeatAt) > s.StaleAfter }

func (s *Store) lockView(l *lock) *LockView {
	if l == nil {
		return nil
	}
	return &LockView{
		User: s.userView(l.UserID), Device: l.Device,
		AcquiredAt:  l.AcquiredAt.UTC().Format(time.RFC3339),
		HeartbeatAt: l.HeartbeatAt.UTC().Format(time.RFC3339),
		Stale:       s.lockStale(l),
		RequestedBy: l.RequestedBy,
	}
}

func (s *Store) view(p *project, m *member, detail bool) ProjectView {
	v := ProjectView{
		ID: p.ID, Name: p.Name, OwnerID: p.OwnerID, Role: m.Role, MemberCount: len(p.Members),
		RetentionDays: p.RetentionDays, Head: p.Head, Lock: s.lockView(p.Lock), CreatedAt: p.CreatedAt,
	}
	if detail {
		for _, mm := range p.Members {
			v.Members = append(v.Members, MemberView{User: s.userView(mm.UserID), Role: mm.Role, JoinedAt: mm.JoinedAt})
		}
	}
	return v
}

// --- users -----------------------------------------------------------------

func validName(name string) bool {
	if len(name) == 0 || len(name) > 32 {
		return false
	}
	for _, c := range []byte(name) {
		if !(c >= 'a' && c <= 'z') && !(c >= 'A' && c <= 'Z') && !(c >= '0' && c <= '9') && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

func (s *Store) saveSettings() error {
	return writeJSON(filepath.Join(s.dir, "server.json"), s.settings)
}

// SetShareAll turns "everyone is a member of every project" on or off.
// Turning it on adds every account to every existing project.
func (s *Store) SetShareAll(on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settings.ShareAll = on
	if err := s.saveSettings(); err != nil {
		return err
	}
	if !on {
		return nil
	}
	for _, p := range s.projects {
		changed := false
		for id := range s.users {
			if p.member(id) == nil {
				p.Members = append(p.Members, member{UserID: id, Role: "editor", JoinedAt: s.stamp()})
				changed = true
			}
		}
		if changed {
			if err := s.saveProject(p); err != nil {
				return err
			}
		}
	}
	return nil
}

// SetJoinSecret sets the secret of the session code; "" turns joining off.
// Accounts made with an earlier secret stay.
func (s *Store) SetJoinSecret(secret string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settings.JoinSHA256 = ""
	if secret != "" {
		s.settings.JoinSHA256 = hashHex(secret)
	}
	return s.saveSettings()
}

// Join creates an account for someone who holds the session code's secret
// and returns its token.
func (s *Store) Join(secret, name string) (UserView, string, error) {
	s.mu.Lock()
	ok := s.settings.JoinSHA256 != "" && secret != "" && hashHex(secret) == s.settings.JoinSHA256
	s.mu.Unlock()
	if !ok {
		return UserView{}, "", core.ErrJoinCode
	}
	return s.AddUser(name)
}

// AddUser creates an account and returns its token. The token is shown once;
// only its hash is kept.
func (s *Store) AddUser(name string) (UserView, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !validName(name) {
		return UserView{}, "", fmt.Errorf("%w: a user name is 1 to 32 letters, digits, '-' or '_'", ErrBadRequest)
	}
	if s.usersChangedOnDisk() {
		if err := s.loadUsers(); err != nil {
			return UserView{}, "", err
		}
	}
	for _, u := range s.users {
		if strings.EqualFold(u.Name, name) {
			return UserView{}, "", fmt.Errorf("%w: %w", ErrBadRequest, core.ErrNameTaken)
		}
	}
	token := "hn_" + randomHex(32)
	u := &User{ID: newID(), Name: name, TokenSHA256: hashHex(token), CreatedAt: s.stamp()}
	s.users[u.ID] = u
	s.byToken[u.TokenSHA256] = u
	if err := s.saveUsers(); err != nil {
		return UserView{}, "", err
	}
	if s.settings.ShareAll {
		for _, p := range s.projects {
			p.Members = append(p.Members, member{UserID: u.ID, Role: "editor", JoinedAt: s.stamp()})
			if err := s.saveProject(p); err != nil {
				return UserView{}, "", err
			}
		}
	}
	return UserView{ID: u.ID, Name: u.Name}, token, nil
}

// ReissueToken gives an existing account a new token and returns it. It is
// for the computer that hosts the session: whoever runs it already has the
// data folder, so this hands back an account whose token was lost.
func (s *Store) ReissueToken(name string) (UserView, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.usersChangedOnDisk() {
		if err := s.loadUsers(); err != nil {
			return UserView{}, "", err
		}
	}
	for _, u := range s.users {
		if strings.EqualFold(u.Name, name) {
			token := "hn_" + randomHex(32)
			delete(s.byToken, u.TokenSHA256)
			u.TokenSHA256 = hashHex(token)
			s.byToken[u.TokenSHA256] = u
			if err := s.saveUsers(); err != nil {
				return UserView{}, "", err
			}
			return UserView{ID: u.ID, Name: u.Name}, token, nil
		}
	}
	return UserView{}, "", core.ErrNotFound
}

// UserByToken finds the account a token belongs to.
func (s *Store) UserByToken(token string) (UserView, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.byToken[hashHex(token)]
	if u == nil && s.usersChangedOnDisk() {
		// An account added with "henosis-server adduser" while this server runs.
		if err := s.loadUsers(); err == nil {
			u = s.byToken[hashHex(token)]
		}
	}
	if u == nil {
		return UserView{}, false
	}
	return UserView{ID: u.ID, Name: u.Name}, true
}

// Users lists all accounts.
func (s *Store) Users() []UserView {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]UserView, 0, len(s.users))
	for _, u := range s.users {
		out = append(out, UserView{ID: u.ID, Name: u.Name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// --- projects --------------------------------------------------------------

// CreateProject makes a project owned by the caller.
func (s *Store) CreateProject(userID, name string) (ProjectView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return ProjectView{}, fmt.Errorf("%w: a project name is 1 to 100 characters", ErrBadRequest)
	}
	p := &project{
		ID: newID(), Name: name, OwnerID: userID, RetentionDays: defaultRetentionDays, NextSeq: 1,
		Members:   []member{{UserID: userID, Role: "owner", JoinedAt: s.stamp()}},
		CreatedAt: s.stamp(), snapshots: map[string]core.Snapshot{},
	}
	if s.settings.ShareAll {
		for id := range s.users {
			if id != userID {
				p.Members = append(p.Members, member{UserID: id, Role: "editor", JoinedAt: s.stamp()})
			}
		}
		sort.Slice(p.Members[1:], func(i, j int) bool { return s.users[p.Members[1+i].UserID].Name < s.users[p.Members[1+j].UserID].Name })
	}
	for _, sub := range []string{"blobs", "snapshots"} {
		if err := os.MkdirAll(filepath.Join(s.projectDir(p.ID), sub), 0o755); err != nil {
			return ProjectView{}, err
		}
	}
	if err := s.saveProject(p); err != nil {
		return ProjectView{}, err
	}
	s.projects[p.ID] = p
	return s.view(p, &p.Members[0], true), nil
}

// Projects lists the caller's projects by name.
func (s *Store) Projects(userID string) []ProjectView {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []ProjectView{}
	for _, p := range s.projects {
		if m := p.member(userID); m != nil {
			out = append(out, s.view(p, m, false))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name+out[i].ID < out[j].Name+out[j].ID })
	return out
}

// Project returns one project with its members.
func (s *Store) Project(userID, projectID string) (ProjectView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, m, err := s.memberProject(userID, projectID)
	if err != nil {
		return ProjectView{}, err
	}
	return s.view(p, m, true), nil
}

// UpdateProject changes the name or the version lifetime. Owner only.
func (s *Store) UpdateProject(userID, projectID string, name *string, retentionDays *int) (ProjectView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, m, err := s.memberProject(userID, projectID)
	if err != nil {
		return ProjectView{}, err
	}
	if m.Role != "owner" {
		return ProjectView{}, ErrForbidden
	}
	if name != nil {
		trimmed := strings.TrimSpace(*name)
		if trimmed == "" || len(trimmed) > 100 {
			return ProjectView{}, fmt.Errorf("%w: a project name is 1 to 100 characters", ErrBadRequest)
		}
		p.Name = trimmed
	}
	if retentionDays != nil {
		if *retentionDays < 1 || *retentionDays > 365 {
			return ProjectView{}, fmt.Errorf("%w: retention_days must be between 1 and 365", ErrBadRequest)
		}
		p.RetentionDays = *retentionDays
	}
	if err := s.saveProject(p); err != nil {
		return ProjectView{}, err
	}
	return s.view(p, m, true), nil
}

const inviteAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

func normalizeInvite(code string) string { return strings.ToUpper(strings.TrimSpace(code)) }

// CreateInvite returns a single-use code that lets one person join. Owner only.
func (s *Store) CreateInvite(userID, projectID string) (string, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, m, err := s.memberProject(userID, projectID)
	if err != nil {
		return "", time.Time{}, err
	}
	if m.Role != "owner" {
		return "", time.Time{}, ErrForbidden
	}
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, err
	}
	code := "HN"
	for i, b := range raw {
		if i%4 == 0 {
			code += "-"
		}
		code += string(inviteAlphabet[int(b)%len(inviteAlphabet)])
	}
	expires := s.Now().Add(inviteLifetime)
	p.Invites = append(p.Invites, invite{CodeSHA256: hashHex(code), ExpiresAt: expires})
	if err := s.saveProject(p); err != nil {
		return "", time.Time{}, err
	}
	return code, expires, nil
}

// AcceptInvite adds the caller to the project the code belongs to.
func (s *Store) AcceptInvite(userID, code string) (ProjectView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := hashHex(normalizeInvite(code))
	for _, p := range s.projects {
		for i := range p.Invites {
			inv := &p.Invites[i]
			if inv.CodeSHA256 != want {
				continue
			}
			if inv.Used || s.Now().After(inv.ExpiresAt) {
				return ProjectView{}, ErrInviteInvalid
			}
			inv.Used = true
			if p.member(userID) == nil {
				p.Members = append(p.Members, member{UserID: userID, Role: "editor", JoinedAt: s.stamp()})
			}
			if err := s.saveProject(p); err != nil {
				return ProjectView{}, err
			}
			return s.view(p, p.member(userID), true), nil
		}
	}
	return ProjectView{}, ErrInviteInvalid
}

// DeleteProject removes a project, its versions and its files from the
// server. Owner only. The folders on people's computers are untouched.
func (s *Store) DeleteProject(userID, projectID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, m, err := s.memberProject(userID, projectID)
	if err != nil {
		return err
	}
	if m.Role != "owner" {
		return ErrForbidden
	}
	dir := s.projectDir(p.ID)
	gone := dir + ".deleted-" + randomHex(4)
	// Renaming first means a half-deleted folder is never loaded again.
	if err := os.Rename(dir, gone); err != nil {
		return err
	}
	delete(s.projects, p.ID)
	return os.RemoveAll(gone)
}

// RemoveMember removes someone from a project. The owner can remove anyone
// but themself; an editor can only leave.
func (s *Store) RemoveMember(userID, projectID, targetID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, m, err := s.memberProject(userID, projectID)
	if err != nil {
		return err
	}
	if p.member(targetID) == nil {
		return ErrNotFound
	}
	if targetID == p.OwnerID || (m.Role != "owner" && targetID != userID) {
		return ErrForbidden
	}
	kept := p.Members[:0]
	for _, mm := range p.Members {
		if mm.UserID != targetID {
			kept = append(kept, mm)
		}
	}
	p.Members = kept
	if p.Lock != nil && (p.Lock.UserID == targetID || !p.shared()) {
		p.Lock = nil // a removed holder loses the lock; a solo project has none
	}
	return s.saveProject(p)
}

// --- locks -----------------------------------------------------------------

// AcquireLock takes the project's lock for one device. On a solo project it
// succeeds without storing anything. takenOver is true when a stale lock of
// someone else was replaced.
func (s *Store) AcquireLock(userID, projectID, device, baseSnapshotID string) (view *LockView, takenOver bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, _, err := s.memberProject(userID, projectID)
	if err != nil {
		return nil, false, err
	}
	if device == "" {
		return nil, false, fmt.Errorf("%w: a device name is required", ErrBadRequest)
	}
	now := s.Now()
	mine := &lock{UserID: userID, Device: device, AcquiredAt: now, HeartbeatAt: now}
	if !p.shared() {
		return s.lockView(mine), false, nil
	}
	if p.Lock != nil {
		if p.Lock.UserID == userID && p.Lock.Device == device {
			p.Lock.HeartbeatAt = now
			return s.lockView(p.Lock), false, s.saveProject(p)
		}
		if !s.lockStale(p.Lock) {
			return nil, false, &LockHeldError{Holder: s.userView(p.Lock.UserID).Name}
		}
		takenOver = true
	}
	if baseSnapshotID != p.Head {
		return nil, false, ErrNotAtHead
	}
	p.Lock = mine
	return s.lockView(mine), takenOver, s.saveProject(p)
}

// Heartbeat keeps a lock alive. ErrLockLost means the lock now belongs to
// someone else or was released.
func (s *Store) Heartbeat(userID, projectID, device string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, _, err := s.memberProject(userID, projectID)
	if err != nil {
		return err
	}
	if !p.shared() {
		return nil
	}
	if p.Lock == nil || p.Lock.UserID != userID || p.Lock.Device != device {
		return ErrLockLost
	}
	p.Lock.HeartbeatAt = s.Now()
	return s.saveProject(p)
}

// ReleaseLock frees the lock. The holder can always release; the owner can
// release anyone's lock with force.
func (s *Store) ReleaseLock(userID, projectID, device string, force bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, m, err := s.memberProject(userID, projectID)
	if err != nil {
		return err
	}
	if p.Lock == nil {
		return nil
	}
	holder := p.Lock.UserID == userID && p.Lock.Device == device
	if !holder && !(force && m.Role == "owner") {
		return ErrForbidden
	}
	p.Lock = nil
	return s.saveProject(p)
}

// RequestLock records that the caller would like the lock. The holder's app
// sees the name the next time it looks at the project.
func (s *Store) RequestLock(userID, projectID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, _, err := s.memberProject(userID, projectID)
	if err != nil {
		return err
	}
	if p.Lock == nil || p.Lock.UserID == userID {
		return nil
	}
	p.Lock.RequestedBy = s.userView(userID).Name
	return s.saveProject(p)
}

// --- blobs and snapshots ---------------------------------------------------

// MissingBlobs returns which of the hashes the project does not hold yet.
func (s *Store) MissingBlobs(userID, projectID string, hashes []string) ([]string, error) {
	s.mu.Lock()
	_, _, err := s.memberProject(userID, projectID)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	missing := []string{}
	for _, h := range hashes {
		if !core.IsSHA256Hex(h) {
			return nil, fmt.Errorf("%w: not a SHA-256 hash: %s", ErrBadRequest, h)
		}
		if _, err := os.Stat(s.blobPath(projectID, h)); err != nil {
			missing = append(missing, h)
		}
	}
	return missing, nil
}

// PutBlob stores content under its hash. Bytes that do not match the hash are
// refused and leave nothing behind.
func (s *Store) PutBlob(userID, projectID, sha string, r io.Reader) error {
	s.mu.Lock()
	_, _, err := s.memberProject(userID, projectID)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if !core.IsSHA256Hex(sha) {
		return fmt.Errorf("%w: not a SHA-256 hash", ErrBadRequest)
	}
	final := s.blobPath(projectID, sha)
	if _, err := os.Stat(final); err == nil {
		_, _ = io.Copy(io.Discard, r)
		return nil // already stored
	}
	tmp, err := os.CreateTemp(filepath.Dir(final), ".upload-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once the rename has happened
	hasher := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(tmp, hasher), r)
	closeErr := tmp.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if hex.EncodeToString(hasher.Sum(nil)) != sha {
		return ErrBlobMismatch
	}
	return os.Rename(tmp.Name(), final)
}

// BlobPath returns where a blob is stored, for members only.
func (s *Store) BlobPath(userID, projectID, sha string) (string, error) {
	s.mu.Lock()
	_, _, err := s.memberProject(userID, projectID)
	s.mu.Unlock()
	if err != nil {
		return "", err
	}
	if !core.IsSHA256Hex(sha) {
		return "", ErrNotFound
	}
	path := s.blobPath(projectID, sha)
	if _, err := os.Stat(path); err != nil {
		return "", ErrNotFound
	}
	return path, nil
}

// Commit adds a snapshot and moves the head, or explains why it cannot.
func (s *Store) Commit(userID, projectID, device string, req core.CommitRequest) (core.CommitResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, _, err := s.memberProject(userID, projectID)
	if err != nil {
		return core.CommitResult{}, err
	}
	manifest := req.Manifest
	manifest.Sort()
	if err := manifest.Validate(); err != nil {
		return core.CommitResult{Status: core.InvalidManifest, Reason: err.Error()}, nil
	}
	manifestJSON := manifest.JSON()
	manifestHash := hashBytes(manifestJSON)
	author := s.userView(userID).Name

	if p.Head != req.ParentID {
		// A retry whose first attempt succeeded: answer with what exists.
		if head, ok := p.snapshots[p.Head]; ok && head.ParentID == req.ParentID &&
			head.ManifestSHA256 == manifestHash && head.Author == author && head.Device == device {
			return core.CommitResult{Status: core.AlreadyCommitted, Snapshot: head}, nil
		}
	}
	if p.shared() && (p.Lock == nil || p.Lock.UserID != userID || p.Lock.Device != device) {
		return core.CommitResult{Status: core.LockNotHeld}, nil
	}
	if p.Head != req.ParentID {
		return core.CommitResult{Status: core.HeadMoved, HeadID: p.Head}, nil
	}

	var missing []string
	for _, entry := range manifest.Files {
		info, err := os.Stat(s.blobPath(projectID, entry.SHA256))
		if err != nil || uint64(info.Size()) != entry.Size {
			missing = append(missing, entry.SHA256)
		}
	}
	if len(missing) > 0 {
		return core.CommitResult{Status: core.BlobsMissing, Missing: missing}, nil
	}

	if _, err := os.Stat(s.blobPath(projectID, manifestHash)); err != nil {
		if err := writeFileAtomic(s.blobPath(projectID, manifestHash), manifestJSON); err != nil {
			return core.CommitResult{}, err
		}
	}
	snap := core.Snapshot{
		ID: newID(), Seq: p.NextSeq, ParentID: req.ParentID, ManifestSHA256: manifestHash,
		Author: author, Device: device, Message: req.Message, FLVersion: req.FLVersion,
		Plugins: req.Plugins, IsConflict: req.IsConflict, CreatedAt: s.stamp(),
	}
	// The record is written first; the head moves last.
	if err := writeJSON(filepath.Join(s.projectDir(projectID), "snapshots", snap.ID+".json"), snap); err != nil {
		return core.CommitResult{}, err
	}
	previousHead, previousSeq := p.Head, p.NextSeq
	p.Head, p.NextSeq = snap.ID, p.NextSeq+1
	if err := s.saveProject(p); err != nil {
		p.Head, p.NextSeq = previousHead, previousSeq
		return core.CommitResult{}, err
	}
	p.snapshots[snap.ID] = snap
	return core.CommitResult{Status: core.Committed, Snapshot: snap}, nil
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Snapshots returns the project's history, newest first.
func (s *Store) Snapshots(userID, projectID string) ([]core.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, _, err := s.memberProject(userID, projectID)
	if err != nil {
		return nil, err
	}
	out := make([]core.Snapshot, 0, len(p.snapshots))
	for _, snap := range p.snapshots {
		out = append(out, snap)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq > out[j].Seq })
	return out, nil
}

func (s *Store) readManifest(projectID, manifestHash string) (core.Manifest, error) {
	data, err := os.ReadFile(s.blobPath(projectID, manifestHash))
	if err != nil {
		return core.Manifest{}, fmt.Errorf("the manifest is missing: %w", ErrNotFound)
	}
	if hashBytes(data) != manifestHash {
		return core.Manifest{}, errors.New("the stored manifest is damaged")
	}
	return core.ParseManifest(data)
}

// Manifest returns the file list of one snapshot.
func (s *Store) Manifest(userID, projectID, snapshotID string) (core.Manifest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, _, err := s.memberProject(userID, projectID)
	if err != nil {
		return core.Manifest{}, err
	}
	snap, ok := p.snapshots[snapshotID]
	if !ok {
		return core.Manifest{}, ErrNotFound
	}
	return s.readManifest(projectID, snap.ManifestSHA256)
}

// AddComment leaves a remark on one version.
func (s *Store) AddComment(userID, projectID, snapshotID, text string) (core.Comment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, _, err := s.memberProject(userID, projectID)
	if err != nil {
		return core.Comment{}, err
	}
	text = strings.TrimSpace(text)
	if text == "" || len(text) > 2000 {
		return core.Comment{}, fmt.Errorf("%w: a comment is 1 to 2000 characters", ErrBadRequest)
	}
	if _, ok := p.snapshots[snapshotID]; !ok {
		return core.Comment{}, ErrNotFound
	}
	c := core.Comment{ID: newID(), SnapshotID: snapshotID, Author: s.userView(userID).Name, Text: text, CreatedAt: s.stamp()}
	p.Comments = append(p.Comments, c)
	return c, s.saveProject(p)
}

// Comments lists a project's comments, oldest first.
func (s *Store) Comments(userID, projectID string) ([]core.Comment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, _, err := s.memberProject(userID, projectID)
	if err != nil {
		return nil, err
	}
	return append([]core.Comment{}, p.Comments...), nil
}

// Cleanup deletes snapshots older than each project's version lifetime (never
// the head) and then blobs no remaining snapshot uses, once they are older
// than BlobGrace. It returns how many of each were removed.
func (s *Store) Cleanup() (snapshotsDeleted, blobsDeleted int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.Now()
	for _, p := range s.projects {
		cutoff := now.Add(-time.Duration(p.RetentionDays) * 24 * time.Hour)
		for id, snap := range p.snapshots {
			created, err := time.Parse(time.RFC3339, snap.CreatedAt)
			if id == p.Head || err != nil || !created.Before(cutoff) {
				continue
			}
			if os.Remove(filepath.Join(s.projectDir(p.ID), "snapshots", id+".json")) == nil {
				delete(p.snapshots, id)
				snapshotsDeleted++
			}
		}
		// Comments go with their version.
		kept := p.Comments[:0]
		for _, c := range p.Comments {
			if _, ok := p.snapshots[c.SnapshotID]; ok {
				kept = append(kept, c)
			}
		}
		if len(kept) != len(p.Comments) {
			p.Comments = kept
			_ = s.saveProject(p)
		}

		referenced := map[string]bool{}
		complete := true
		for _, snap := range p.snapshots {
			referenced[snap.ManifestSHA256] = true
			manifest, err := s.readManifest(p.ID, snap.ManifestSHA256)
			if err != nil {
				complete = false // never delete blobs on incomplete knowledge
				break
			}
			for _, entry := range manifest.Files {
				referenced[entry.SHA256] = true
			}
		}
		if !complete {
			continue
		}
		blobs, _ := os.ReadDir(filepath.Join(s.projectDir(p.ID), "blobs"))
		for _, blob := range blobs {
			info, err := blob.Info()
			if err != nil || referenced[blob.Name()] || now.Sub(info.ModTime()) < s.BlobGrace {
				continue
			}
			if os.Remove(filepath.Join(s.projectDir(p.ID), "blobs", blob.Name())) == nil {
				blobsDeleted++
			}
		}
	}
	return snapshotsDeleted, blobsDeleted
}
