package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"henosis/core"
)

// world is a store with a controllable clock and two users.
type world struct {
	t        *testing.T
	s        *Store
	dir      string
	now      time.Time
	ann, bob UserView
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t, dir: t.TempDir(), now: time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)}
	w.reopen()
	var err error
	if w.ann, _, err = w.s.AddUser("ann"); err != nil {
		t.Fatal(err)
	}
	if w.bob, _, err = w.s.AddUser("bob"); err != nil {
		t.Fatal(err)
	}
	return w
}

func (w *world) reopen() {
	s, err := Open(w.dir)
	if err != nil {
		w.t.Fatal(err)
	}
	s.Now = func() time.Time { return w.now }
	w.s = s
}

func (w *world) put(projectID, content string) core.FileEntry {
	w.t.Helper()
	sha := hashHex(content)
	if err := w.s.PutBlob(w.ann.ID, projectID, sha, strings.NewReader(content)); err != nil {
		w.t.Fatal(err)
	}
	// Files get the real time; the tests run on a fake clock, so stamp them with it.
	blob := filepath.Join(w.dir, "projects", projectID, "blobs", sha)
	if err := os.Chtimes(blob, w.now, w.now); err != nil {
		w.t.Fatal(err)
	}
	return core.FileEntry{SHA256: sha, Size: uint64(len(content))}
}

func manifestOf(entries map[string]core.FileEntry) core.Manifest {
	var m core.Manifest
	for path, e := range entries {
		e.Path = path
		m.Files = append(m.Files, e)
	}
	m.Sort()
	return m
}

// shared returns a project with ann (owner) and bob (editor).
func (w *world) shared() ProjectView {
	w.t.Helper()
	p, err := w.s.CreateProject(w.ann.ID, "My Song")
	if err != nil {
		w.t.Fatal(err)
	}
	code, _, err := w.s.CreateInvite(w.ann.ID, p.ID)
	if err != nil {
		w.t.Fatal(err)
	}
	if _, err := w.s.AcceptInvite(w.bob.ID, code); err != nil {
		w.t.Fatal(err)
	}
	return p
}

func TestUsersAndTokens(t *testing.T) {
	w := newWorld(t)
	carl, token, err := w.s.AddUser("carl")
	if err != nil || !strings.HasPrefix(token, "hn_") || len(token) != 67 {
		t.Fatalf("AddUser: %v %q", err, token)
	}
	if got, ok := w.s.UserByToken(token); !ok || got != carl {
		t.Error("the token does not find its user")
	}
	if _, ok := w.s.UserByToken("hn_wrong"); ok {
		t.Error("a wrong token was accepted")
	}
	for _, bad := range []string{"", "CARL", "has space", strings.Repeat("x", 33), "sl/ash"} {
		if _, _, err := w.s.AddUser(bad); !errors.Is(err, ErrBadRequest) {
			t.Errorf("AddUser(%q) = %v", bad, err)
		}
	}
	// The token is not stored anywhere in clear text, and survives a restart.
	data, _ := os.ReadFile(filepath.Join(w.dir, "users.json"))
	if strings.Contains(string(data), token) {
		t.Error("users.json contains the token itself")
	}
	w.reopen()
	if got, ok := w.s.UserByToken(token); !ok || got != carl || len(w.s.Users()) != 3 {
		t.Error("users were not reloaded")
	}
}

func TestProjectsInvitesMembers(t *testing.T) {
	w := newWorld(t)
	p, err := w.s.CreateProject(w.ann.ID, "  My Song ")
	if err != nil || p.Name != "My Song" || p.Role != "owner" || p.MemberCount != 1 || p.RetentionDays != 3 {
		t.Fatalf("CreateProject: %v %+v", err, p)
	}
	// Someone who is not a member cannot tell the project exists.
	if _, err := w.s.Project(w.bob.ID, p.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("non-member got %v", err)
	}
	if len(w.s.Projects(w.bob.ID)) != 0 || len(w.s.Projects(w.ann.ID)) != 1 {
		t.Error("project lists are wrong")
	}

	code, _, err := w.s.CreateInvite(w.ann.ID, p.ID)
	if err != nil || !strings.HasPrefix(code, "HN-") || len(code) != 17 {
		t.Fatalf("CreateInvite: %v %q", err, code)
	}
	joined, err := w.s.AcceptInvite(w.bob.ID, " "+strings.ToLower(code)+" ") // typed sloppily
	if err != nil || joined.Role != "editor" || joined.MemberCount != 2 {
		t.Fatalf("AcceptInvite: %v %+v", err, joined)
	}
	if _, err := w.s.AcceptInvite(w.bob.ID, code); !errors.Is(err, ErrInviteInvalid) {
		t.Error("an invite worked twice")
	}
	if _, err := w.s.AcceptInvite(w.bob.ID, "HN-AAAA-BBBB-CCCC"); !errors.Is(err, ErrInviteInvalid) {
		t.Error("an unknown invite worked")
	}
	expired, _, _ := w.s.CreateInvite(w.ann.ID, p.ID)
	w.now = w.now.Add(8 * 24 * time.Hour)
	carl, _, _ := w.s.AddUser("carl")
	if _, err := w.s.AcceptInvite(carl.ID, expired); !errors.Is(err, ErrInviteInvalid) {
		t.Error("an expired invite worked")
	}

	// Only the owner changes settings and invites.
	seven := 7
	if _, err := w.s.UpdateProject(w.bob.ID, p.ID, nil, &seven); !errors.Is(err, ErrForbidden) {
		t.Error("an editor changed the settings")
	}
	if _, _, err := w.s.CreateInvite(w.bob.ID, p.ID); !errors.Is(err, ErrForbidden) {
		t.Error("an editor created an invite")
	}
	name := "Renamed"
	if v, err := w.s.UpdateProject(w.ann.ID, p.ID, &name, &seven); err != nil || v.Name != "Renamed" || v.RetentionDays != 7 {
		t.Errorf("UpdateProject: %v %+v", err, v)
	}
	zero := 0
	if _, err := w.s.UpdateProject(w.ann.ID, p.ID, nil, &zero); !errors.Is(err, ErrBadRequest) {
		t.Error("retention 0 was accepted")
	}

	// The owner cannot be removed; an editor can leave.
	if err := w.s.RemoveMember(w.bob.ID, p.ID, w.ann.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("removing the owner: %v", err)
	}
	if err := w.s.RemoveMember(w.bob.ID, p.ID, w.bob.ID); err != nil {
		t.Errorf("leaving: %v", err)
	}
	if _, err := w.s.Project(w.bob.ID, p.ID); !errors.Is(err, ErrNotFound) {
		t.Error("a removed member still sees the project")
	}
	w.reopen()
	if v, err := w.s.Project(w.ann.ID, p.ID); err != nil || v.Name != "Renamed" || v.MemberCount != 1 {
		t.Errorf("after restart: %v %+v", err, v)
	}
}

func TestLocks(t *testing.T) {
	w := newWorld(t)

	// A solo project has no lock: the calls succeed and store nothing.
	solo, _ := w.s.CreateProject(w.ann.ID, "Solo")
	if _, _, err := w.s.AcquireLock(w.ann.ID, solo.ID, "pc1", ""); err != nil {
		t.Fatal(err)
	}
	if v, _ := w.s.Project(w.ann.ID, solo.ID); v.Lock != nil {
		t.Error("a solo project stored a lock")
	}
	if err := w.s.Heartbeat(w.ann.ID, solo.ID, "pc1"); err != nil {
		t.Error(err)
	}

	p := w.shared()
	if _, taken, err := w.s.AcquireLock(w.ann.ID, p.ID, "ann-pc", ""); err != nil || taken {
		t.Fatalf("acquire: %v", err)
	}
	if _, _, err := w.s.AcquireLock(w.ann.ID, p.ID, "ann-pc", "anything"); err != nil {
		t.Errorf("re-acquire by the holder: %v", err)
	}
	var held *LockHeldError
	if _, _, err := w.s.AcquireLock(w.bob.ID, p.ID, "bob-pc", ""); !errors.As(err, &held) || held.Holder != "ann" {
		t.Fatalf("second acquire: %v", err)
	}
	// The same user on another device does not hold the lock either.
	if _, _, err := w.s.AcquireLock(w.ann.ID, p.ID, "ann-laptop", ""); !errors.As(err, &held) {
		t.Errorf("other device of the holder: %v", err)
	}
	if err := w.s.Heartbeat(w.bob.ID, p.ID, "bob-pc"); !errors.Is(err, ErrLockLost) {
		t.Errorf("heartbeat by a non-holder: %v", err)
	}
	if err := w.s.ReleaseLock(w.bob.ID, p.ID, "bob-pc", true); !errors.Is(err, ErrForbidden) {
		t.Errorf("an editor forced a release: %v", err)
	}

	// Heartbeats keep the lock alive past the stale limit.
	w.now = w.now.Add(9 * time.Minute)
	if err := w.s.Heartbeat(w.ann.ID, p.ID, "ann-pc"); err != nil {
		t.Fatal(err)
	}
	w.now = w.now.Add(9 * time.Minute)
	if _, _, err := w.s.AcquireLock(w.bob.ID, p.ID, "bob-pc", ""); !errors.As(err, &held) {
		t.Errorf("lock taken while the holder was alive: %v", err)
	}
	// Without a heartbeat for more than ten minutes it goes stale and can be taken over.
	w.now = w.now.Add(2 * time.Minute)
	if v, _ := w.s.Project(w.bob.ID, p.ID); v.Lock == nil || !v.Lock.Stale {
		t.Error("the lock is not reported stale")
	}
	if _, _, err := w.s.AcquireLock(w.bob.ID, p.ID, "bob-pc", "stale-base"); !errors.Is(err, ErrNotAtHead) {
		t.Errorf("takeover with an old base: %v", err)
	}
	if _, taken, err := w.s.AcquireLock(w.bob.ID, p.ID, "bob-pc", ""); err != nil || !taken {
		t.Fatalf("takeover: %v taken=%v", err, taken)
	}
	if err := w.s.Heartbeat(w.ann.ID, p.ID, "ann-pc"); !errors.Is(err, ErrLockLost) {
		t.Errorf("the old holder was not told: %v", err)
	}

	// The lock survives a restart; the owner can force it free.
	w.reopen()
	if v, _ := w.s.Project(w.ann.ID, p.ID); v.Lock == nil || v.Lock.User.Name != "bob" {
		t.Fatal("the lock was not reloaded")
	}
	if err := w.s.ReleaseLock(w.ann.ID, p.ID, "ann-pc", false); !errors.Is(err, ErrForbidden) {
		t.Errorf("release by a non-holder: %v", err)
	}
	if err := w.s.ReleaseLock(w.ann.ID, p.ID, "ann-pc", true); err != nil {
		t.Errorf("forced release by the owner: %v", err)
	}
	if v, _ := w.s.Project(w.ann.ID, p.ID); v.Lock != nil {
		t.Error("the lock is still there")
	}

	// Removing the member who holds the lock frees it.
	w.s.AcquireLock(w.bob.ID, p.ID, "bob-pc", "")
	if err := w.s.RemoveMember(w.ann.ID, p.ID, w.bob.ID); err != nil {
		t.Fatal(err)
	}
	if v, _ := w.s.Project(w.ann.ID, p.ID); v.Lock != nil || v.MemberCount != 1 {
		t.Error("removing the holder did not free the lock")
	}
}

func TestBlobsAndCommits(t *testing.T) {
	w := newWorld(t)
	p, _ := w.s.CreateProject(w.ann.ID, "Solo")

	// Bytes that do not match the claimed hash are refused and leave nothing behind.
	wrong := strings.Repeat("f", 64)
	if err := w.s.PutBlob(w.ann.ID, p.ID, wrong, strings.NewReader("kick")); !errors.Is(err, ErrBlobMismatch) {
		t.Fatalf("mismatched upload: %v", err)
	}
	if missing, _ := w.s.MissingBlobs(w.ann.ID, p.ID, []string{wrong}); len(missing) != 1 {
		t.Error("the refused blob was stored")
	}
	if entries, _ := os.ReadDir(filepath.Join(w.dir, "projects", p.ID, "blobs")); len(entries) != 0 {
		t.Errorf("leftover files after a refused upload: %d", len(entries))
	}
	if _, err := w.s.MissingBlobs(w.ann.ID, p.ID, []string{"nope"}); !errors.Is(err, ErrBadRequest) {
		t.Error("a bad hash was accepted")
	}
	if err := w.s.PutBlob(w.bob.ID, p.ID, hashHex("kick"), strings.NewReader("kick")); !errors.Is(err, ErrNotFound) {
		t.Error("a non-member uploaded a blob")
	}

	kick := core.FileEntry{SHA256: hashHex("kick"), Size: 4}
	m1 := manifestOf(map[string]core.FileEntry{"Audio/kick.wav": kick})
	commit := func(parent string, m core.Manifest) core.CommitResult {
		t.Helper()
		r, err := w.s.Commit(w.ann.ID, p.ID, "pc1", core.CommitRequest{ParentID: parent, Manifest: m})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := commit("", m1); r.Status != core.BlobsMissing || len(r.Missing) != 1 {
		t.Fatalf("commit without blobs: %+v", r)
	}
	w.put(p.ID, "kick")
	wrongSize := manifestOf(map[string]core.FileEntry{"Audio/kick.wav": {SHA256: kick.SHA256, Size: 5}})
	if r := commit("", wrongSize); r.Status != core.BlobsMissing {
		t.Errorf("commit with a wrong size: %+v", r)
	}
	bad := manifestOf(map[string]core.FileEntry{"../escape.wav": kick})
	if r := commit("", bad); r.Status != core.InvalidManifest || r.Reason == "" {
		t.Errorf("commit with a bad path: %+v", r)
	}

	first := commit("", m1)
	if first.Status != core.Committed || first.Snapshot.Seq != 1 || first.Snapshot.Author != "ann" {
		t.Fatalf("first commit: %+v", first)
	}
	// The same request again is a retry, not a second version.
	if r := commit("", m1); r.Status != core.AlreadyCommitted || r.Snapshot.ID != first.Snapshot.ID {
		t.Errorf("retry: %+v", r)
	}
	// A different commit on the old parent is refused.
	if r := commit("", core.Manifest{}); r.Status != core.HeadMoved || r.HeadID != first.Snapshot.ID {
		t.Errorf("stale commit: %+v", r)
	}
	second := commit(first.Snapshot.ID, core.Manifest{})
	if second.Status != core.Committed || second.Snapshot.Seq != 2 {
		t.Fatalf("second commit: %+v", second)
	}

	w.reopen()
	history, err := w.s.Snapshots(w.ann.ID, p.ID)
	if err != nil || len(history) != 2 || history[0].ID != second.Snapshot.ID || history[1].ID != first.Snapshot.ID {
		t.Fatalf("history: %v %+v", err, history)
	}
	if m, err := w.s.Manifest(w.ann.ID, p.ID, first.Snapshot.ID); err != nil || !m.Equal(m1) {
		t.Errorf("Manifest: %v", err)
	}
	if _, err := w.s.Manifest(w.ann.ID, p.ID, "nope"); !errors.Is(err, ErrNotFound) {
		t.Error("an unknown snapshot returned a manifest")
	}
	if _, err := w.s.Snapshots(w.bob.ID, p.ID); !errors.Is(err, ErrNotFound) {
		t.Error("a non-member read the history")
	}
}

func TestSharedProjectNeedsTheLock(t *testing.T) {
	w := newWorld(t)
	p := w.shared()
	req := core.CommitRequest{Manifest: core.Manifest{Files: []core.FileEntry{{Path: "a.wav", SHA256: w.put(p.ID, "a").SHA256, Size: 1}}}}

	if r, _ := w.s.Commit(w.ann.ID, p.ID, "ann-pc", req); r.Status != core.LockNotHeld {
		t.Fatalf("commit without the lock: %+v", r)
	}
	w.s.AcquireLock(w.bob.ID, p.ID, "bob-pc", "")
	if r, _ := w.s.Commit(w.ann.ID, p.ID, "ann-pc", req); r.Status != core.LockNotHeld {
		t.Errorf("commit while someone else holds the lock: %+v", r)
	}
	if r, _ := w.s.Commit(w.bob.ID, p.ID, "bob-laptop", req); r.Status != core.LockNotHeld {
		t.Errorf("commit from another device of the holder: %+v", r)
	}
	r, _ := w.s.Commit(w.bob.ID, p.ID, "bob-pc", req)
	if r.Status != core.Committed {
		t.Fatalf("commit by the holder: %+v", r)
	}
	// A retry after the lock was released still gets the existing snapshot.
	w.s.ReleaseLock(w.bob.ID, p.ID, "bob-pc", false)
	if again, _ := w.s.Commit(w.bob.ID, p.ID, "bob-pc", req); again.Status != core.AlreadyCommitted || again.Snapshot.ID != r.Snapshot.ID {
		t.Errorf("retry after release: %+v", again)
	}
}

func TestCleanup(t *testing.T) {
	w := newWorld(t)
	p, _ := w.s.CreateProject(w.ann.ID, "Solo")
	blobDir := filepath.Join(w.dir, "projects", p.ID, "blobs")
	age := func(sha string, d time.Duration) {
		t.Helper()
		when := w.now.Add(-d)
		if err := os.Chtimes(filepath.Join(blobDir, sha), when, when); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(parent string, files map[string]core.FileEntry) core.Snapshot {
		t.Helper()
		r, err := w.s.Commit(w.ann.ID, p.ID, "pc1", core.CommitRequest{ParentID: parent, Manifest: manifestOf(files)})
		if err != nil || r.Status != core.Committed {
			t.Fatalf("commit: %v %+v", err, r)
		}
		return r.Snapshot
	}

	v1 := commit("", map[string]core.FileEntry{"song.flp": w.put(p.ID, "song v1"), "kick.wav": w.put(p.ID, "kick")})
	w.now = w.now.Add(24 * time.Hour)
	v2 := commit(v1.ID, map[string]core.FileEntry{"song.flp": w.put(p.ID, "song v2"), "kick.wav": w.put(p.ID, "kick")})
	w.now = w.now.Add(24 * time.Hour)
	v3 := commit(v2.ID, map[string]core.FileEntry{"song.flp": w.put(p.ID, "song v3"), "kick.wav": w.put(p.ID, "kick")})
	orphan := w.put(p.ID, "uploaded but never committed")

	// Day 2: nothing is older than three days yet.
	if snaps, blobs := w.s.Cleanup(); snaps != 0 || blobs != 0 {
		t.Fatalf("early cleanup removed %d snapshots, %d blobs", snaps, blobs)
	}

	// Day 3.5: v1 has expired. Its unique blob goes once it is past the grace period.
	w.now = w.now.Add(36 * time.Hour)
	for _, sha := range []string{hashHex("song v1"), hashHex("song v2"), hashHex("song v3"), hashHex("kick"), v1.ManifestSHA256} {
		age(sha, 48*time.Hour)
	}
	age(orphan.SHA256, time.Hour) // a fresh upload whose commit may still come
	snaps, blobs := w.s.Cleanup()
	if snaps != 1 || blobs != 2 { // v1's snapshot; v1's song and v1's manifest
		t.Fatalf("cleanup removed %d snapshots, %d blobs", snaps, blobs)
	}
	history, _ := w.s.Snapshots(w.ann.ID, p.ID)
	if len(history) != 2 || history[0].ID != v3.ID || history[1].ID != v2.ID {
		t.Fatalf("history after cleanup: %+v", history)
	}
	for sha, want := range map[string]bool{hashHex("song v1"): false, hashHex("song v2"): true, hashHex("song v3"): true,
		hashHex("kick"): true, orphan.SHA256: true} {
		_, err := os.Stat(filepath.Join(blobDir, sha))
		if (err == nil) != want {
			t.Errorf("blob %.8s present=%v, want %v", sha, err == nil, want)
		}
	}
	if _, err := w.s.Manifest(w.ann.ID, p.ID, v2.ID); err != nil {
		t.Errorf("a kept version lost its manifest: %v", err)
	}

	// Much later: only the head remains, whatever its age, and the orphan is gone.
	w.now = w.now.Add(30 * 24 * time.Hour)
	w.s.Cleanup()
	history, _ = w.s.Snapshots(w.ann.ID, p.ID)
	if len(history) != 1 || history[0].ID != v3.ID {
		t.Fatalf("the head was not kept: %+v", history)
	}
	if m, err := w.s.Manifest(w.ann.ID, p.ID, v3.ID); err != nil || len(m.Files) != 2 {
		t.Errorf("the head's manifest: %v", err)
	}
	if _, err := os.Stat(filepath.Join(blobDir, orphan.SHA256)); err == nil {
		t.Error("the orphan blob was never removed")
	}
	for _, sha := range []string{hashHex("song v3"), hashHex("kick")} {
		if _, err := os.Stat(filepath.Join(blobDir, sha)); err != nil {
			t.Errorf("the head lost blob %.8s", sha)
		}
	}
	// After a restart the expired versions stay gone.
	w.reopen()
	if history, _ := w.s.Snapshots(w.ann.ID, p.ID); len(history) != 1 {
		t.Error("expired snapshots came back after a restart")
	}
}

func TestUserAddedByAnotherProcess(t *testing.T) {
	dir := t.TempDir()
	running, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, annToken, _ := running.AddUser("ann")

	// A second process (the adduser command) opens the same folder.
	other, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Make sure the rewrite gets a different modification time even on
	// file systems with coarse timestamps.
	past := time.Now().Add(-time.Hour)
	os.Chtimes(filepath.Join(dir, "users.json"), past, past)
	running.usersStamp = past
	_, bobToken, err := other.AddUser("bob")
	if err != nil {
		t.Fatal(err)
	}
	if u, ok := running.UserByToken(bobToken); !ok || u.Name != "bob" {
		t.Fatalf("the running store does not know the new account: %+v %v", u, ok)
	}
	if u, ok := running.UserByToken(annToken); !ok || u.Name != "ann" {
		t.Fatalf("an existing account was lost: %+v %v", u, ok)
	}
	if _, ok := running.UserByToken("hn_nope"); ok {
		t.Fatal("an unknown token was accepted")
	}
	// The running store can still add accounts without dropping the other one.
	if _, _, err := running.AddUser("carl"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := running.AddUser("BOB"); err == nil {
		t.Fatal("a duplicate name was accepted")
	}
	fresh, _ := Open(dir)
	if len(fresh.Users()) != 3 {
		t.Fatalf("users on disk: %+v", fresh.Users())
	}
}

func TestSessionJoinAndShareAll(t *testing.T) {
	dir := t.TempDir()
	st, _ := Open(dir)
	ann, _, _ := st.AddUser("ann")
	old, _ := st.CreateProject(ann.ID, "Before")

	// Joining is off until a secret is set.
	if _, _, err := st.Join("", "bob"); !errors.Is(err, core.ErrJoinCode) {
		t.Fatalf("join with no secret set: %v", err)
	}
	if err := st.SetJoinSecret("s3cret"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Join("wrong", "bob"); !errors.Is(err, core.ErrJoinCode) {
		t.Fatalf("join with a wrong secret: %v", err)
	}
	if err := st.SetShareAll(true); err != nil {
		t.Fatal(err)
	}
	bob, token, err := st.Join("s3cret", "bob")
	if err != nil || !strings.HasPrefix(token, "hn_") {
		t.Fatalf("join: %v", err)
	}
	if u, ok := st.UserByToken(token); !ok || u.ID != bob.ID {
		t.Fatal("the joined account's token does not work")
	}
	if _, _, err := st.Join("s3cret", "BOB"); !errors.Is(err, core.ErrNameTaken) {
		t.Fatalf("joining with a taken name: %v", err)
	}
	if _, _, err := st.Join("s3cret", "no spaces"); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("joining with an invalid name: %v", err)
	}
	// Everyone sees everything: projects from before, and new ones from anyone.
	if p, err := st.Project(bob.ID, old.ID); err != nil || p.Role != "editor" || p.MemberCount != 2 {
		t.Fatalf("bob on the earlier project: %v %+v", err, p)
	}
	mine, _ := st.CreateProject(bob.ID, "Bob's")
	if p, err := st.Project(ann.ID, mine.ID); err != nil || p.Role != "editor" || p.OwnerID != bob.ID {
		t.Fatalf("ann on bob's project: %v %+v", err, p)
	}
	carl, _, _ := st.Join("s3cret", "carl")
	if list := st.Projects(carl.ID); len(list) != 2 {
		t.Fatalf("carl sees %d projects", len(list))
	}
	// A new secret stops new joins; existing accounts stay. All of it survives a restart.
	st.SetJoinSecret("another")
	if _, _, err := st.Join("s3cret", "dave"); !errors.Is(err, core.ErrJoinCode) {
		t.Fatalf("join with the old secret: %v", err)
	}
	again, _ := Open(dir)
	if _, _, err := again.Join("another", "dave"); err != nil {
		t.Fatalf("join after a restart: %v", err)
	}
	if list := again.Projects(carl.ID); len(list) != 2 {
		t.Fatalf("after a restart carl sees %d projects", len(list))
	}
	// Without share-all, a new account sees nothing (the standalone server's default).
	plain, _ := Open(t.TempDir())
	a, _, _ := plain.AddUser("a")
	plain.CreateProject(a.ID, "Private")
	b, _, _ := plain.AddUser("b")
	if list := plain.Projects(b.ID); len(list) != 0 {
		t.Fatalf("without share-all a new account sees %d projects", len(list))
	}
}

func TestComments(t *testing.T) {
	w := newWorld(t)
	p := w.shared()
	carl, _, _ := w.s.AddUser("carl")
	commit := func(parent, content string) string {
		t.Helper()
		entry := w.put(p.ID, content)
		entry.Path = "f.wav"
		res, err := w.s.Commit(w.ann.ID, p.ID, "ann-pc", core.CommitRequest{ParentID: parent, Manifest: core.Manifest{Files: []core.FileEntry{entry}}})
		if err != nil || res.Status != core.Committed {
			t.Fatalf("commit: %v %+v", err, res)
		}
		return res.Snapshot.ID
	}
	if _, _, err := w.s.AcquireLock(w.ann.ID, p.ID, "ann-pc", ""); err != nil {
		t.Fatal(err)
	}
	first := commit("", "one")
	second := commit(first, "two")

	c, err := w.s.AddComment(w.bob.ID, p.ID, first, "  the drop hits harder here  ")
	if err != nil || c.Author != "bob" || c.Text != "the drop hits harder here" || c.SnapshotID != first {
		t.Fatalf("comment: %v %+v", err, c)
	}
	w.s.AddComment(w.ann.ID, p.ID, second, "fixed the bass")
	for name, call := range map[string]func() error{
		"empty":           func() error { _, err := w.s.AddComment(w.ann.ID, p.ID, first, "   "); return err },
		"too long":        func() error { _, err := w.s.AddComment(w.ann.ID, p.ID, first, strings.Repeat("x", 2001)); return err },
		"unknown version": func() error { _, err := w.s.AddComment(w.ann.ID, p.ID, "nope", "hi"); return err },
		"not a member":    func() error { _, err := w.s.AddComment(carl.ID, p.ID, first, "hi"); return err },
	} {
		if call() == nil {
			t.Errorf("a comment that is %s was accepted", name)
		}
	}
	if _, err := w.s.Comments(carl.ID, p.ID); err == nil {
		t.Error("a non-member read the comments")
	}
	list, _ := w.s.Comments(w.ann.ID, p.ID)
	if len(list) != 2 || list[0].Author != "bob" || list[1].Text != "fixed the bass" {
		t.Fatalf("comments: %+v", list)
	}
	// Comments are deleted together with their version, and survive a restart until then.
	w.reopen()
	if list, _ := w.s.Comments(w.ann.ID, p.ID); len(list) != 2 {
		t.Fatalf("comments after a restart: %+v", list)
	}
	w.now = w.now.Add(4 * 24 * time.Hour)
	w.s.Cleanup()
	list, _ = w.s.Comments(w.ann.ID, p.ID)
	if len(list) != 1 || list[0].SnapshotID != second {
		t.Fatalf("comments after cleanup: %+v", list)
	}
}

func TestDeleteProject(t *testing.T) {
	w := newWorld(t)
	p := w.shared()
	entry := w.put(p.ID, "one")
	entry.Path = "f.wav"
	if _, _, err := w.s.AcquireLock(w.ann.ID, p.ID, "ann-pc", ""); err != nil {
		t.Fatal(err)
	}
	res, err := w.s.Commit(w.ann.ID, p.ID, "ann-pc", core.CommitRequest{Manifest: core.Manifest{Files: []core.FileEntry{entry}}})
	if err != nil || res.Status != core.Committed {
		t.Fatalf("commit: %v %+v", err, res)
	}
	other, _ := w.s.CreateProject(w.bob.ID, "Bob's own")

	if err := w.s.DeleteProject(w.bob.ID, p.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("an editor deleted the project: %v", err)
	}
	carl, _, _ := w.s.AddUser("carl")
	if err := w.s.DeleteProject(carl.ID, p.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("a stranger deleted the project: %v", err)
	}
	if err := w.s.DeleteProject(w.ann.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.Project(w.ann.ID, p.ID); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("the project is still there: %v", err)
	}
	if _, err := w.s.Project(w.bob.ID, p.ID); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("the other member still sees it: %v", err)
	}
	if _, err := os.Stat(filepath.Join(w.dir, "projects", p.ID)); !os.IsNotExist(err) {
		t.Error("the project's folder is still on disk")
	}
	if list := w.s.Projects(w.bob.ID); len(list) != 1 || list[0].ID != other.ID {
		t.Fatalf("the other project did not survive: %+v", list)
	}
	// Deleting twice, and after a restart, says the same thing.
	if err := w.s.DeleteProject(w.ann.ID, p.ID); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("deleting twice: %v", err)
	}
	w.reopen()
	if _, err := w.s.Project(w.ann.ID, p.ID); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("it came back after a restart: %v", err)
	}
	if len(w.s.Projects(w.bob.ID)) != 1 {
		t.Error("the surviving project was lost on restart")
	}
}
