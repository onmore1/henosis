package client_test

// End-to-end tests: two clients and a real HTTP server on one machine.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"henosis/client"
	"henosis/core"
	"henosis/engine"
	"henosis/server"
	"henosis/store"
)

type side struct {
	c      *client.Client
	folder string
	remote *client.ProjectRemote
	e      *engine.Engine
}

type world struct {
	t        *testing.T
	st       *store.Store
	url      string
	ann, bob *side
	project  core.ProjectView
}

func write(t *testing.T, folder, rel string, content []byte) {
	t.Helper()
	path := filepath.Join(folder, filepath.FromSlash(rel))
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(folder, rel string) string {
	data, err := os.ReadFile(filepath.Join(folder, filepath.FromSlash(rel)))
	if err != nil {
		return "<missing>"
	}
	return string(data)
}

func tree(folder string) map[string]string {
	out := map[string]string{}
	filepath.WalkDir(folder, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(folder, path)
		if rel = filepath.ToSlash(rel); !core.IsIgnored(rel) {
			data, _ := os.ReadFile(path)
			sum := sha256.Sum256(data)
			out[rel] = hex.EncodeToString(sum[:])
		}
		return nil
	})
	return out
}

// newWorld starts a server with users ann and bob, a shared project, and one
// linked folder per user. Each commit takes the lock just for the commit.
func newWorld(t *testing.T) *world {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "server-data"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(st))
	t.Cleanup(srv.Close)
	w := &world{t: t, st: st, url: srv.URL}

	_, annToken, _ := st.AddUser("ann")
	_, bobToken, _ := st.AddUser("bob")
	annClient := client.New(srv.URL, annToken, "ann-pc")
	bobClient := client.New(srv.URL, bobToken, "bob-pc")

	if w.project, err = annClient.CreateProject("My Song"); err != nil {
		t.Fatal(err)
	}
	code, err := annClient.CreateInvite(w.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if joined, err := bobClient.AcceptInvite(code); err != nil || joined.Role != "editor" || joined.MemberCount != 2 {
		t.Fatalf("AcceptInvite: %v %+v", err, joined)
	}
	make := func(c *client.Client, name string) *side {
		s := &side{c: c, folder: filepath.Join(dir, name, "My Song"), remote: c.Remote(w.project.ID)}
		os.MkdirAll(s.folder, 0o755)
		s.e = engine.Open(s.folder, engine.AutoLock{Remote: s.remote, Locker: s.remote}, name)
		s.e.Today = func() string { return "2026-11-15" }
		return s
	}
	w.ann, w.bob = make(annClient, "ann"), make(bobClient, "bob")
	return w
}

func (w *world) sync(s *side) engine.Result {
	w.t.Helper()
	res, err := s.e.Sync(engine.PushOptions{})
	if err != nil {
		w.t.Fatalf("sync: %v", err)
	}
	return res
}

func (w *world) same() {
	w.t.Helper()
	if !reflect.DeepEqual(tree(w.ann.folder), tree(w.bob.folder)) {
		w.t.Fatalf("folders differ:\n ann %v\n bob %v", tree(w.ann.folder), tree(w.bob.folder))
	}
}

func apiCode(err error) string {
	var api *client.APIError
	if errors.As(err, &api) {
		return api.Code
	}
	return ""
}

func TestAuthAndAccess(t *testing.T) {
	w := newWorld(t)
	if resp, err := http.Get(w.url + "/v1/health"); err != nil || resp.StatusCode != 200 {
		t.Fatalf("health: %v", err)
	}
	if me, err := w.ann.c.Me(); err != nil || me.Name != "ann" {
		t.Fatalf("Me: %v %+v", err, me)
	}
	for _, token := range []string{"", "hn_wrong"} {
		if _, err := client.New(w.url, token, "x").Me(); apiCode(err) != "unauthorized" {
			t.Errorf("token %q: %v", token, err)
		}
	}
	// A third user cannot see, read or write the project.
	_, carlToken, _ := w.st.AddUser("carl")
	carl := client.New(w.url, carlToken, "carl-pc")
	if _, err := carl.Project(w.project.ID); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("a non-member read the project: %v", err)
	}
	if list, err := carl.Projects(); err != nil || len(list) != 0 {
		t.Errorf("a non-member lists %d projects (%v)", len(list), err)
	}
	outsider := carl.Remote(w.project.ID)
	if _, err := outsider.History(); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("a non-member read the history: %v", err)
	}
	file := filepath.Join(t.TempDir(), "f")
	os.WriteFile(file, []byte("x"), 0o644)
	sum := sha256.Sum256([]byte("x"))
	if err := outsider.UploadBlob(hex.EncodeToString(sum[:]), file); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("a non-member uploaded a blob: %v", err)
	}
	// Only the owner changes settings.
	seven := 7
	if _, err := w.bob.c.UpdateProject(w.project.ID, nil, &seven); apiCode(err) != "forbidden" {
		t.Errorf("an editor changed the settings: %v", err)
	}
	if p, err := w.ann.c.UpdateProject(w.project.ID, nil, &seven); err != nil || p.RetentionDays != 7 {
		t.Errorf("UpdateProject: %v %+v", err, p)
	}
	if _, err := w.bob.c.AcceptInvite("HN-AAAA-BBBB-CCCC"); apiCode(err) != "invite_invalid" {
		t.Errorf("a made-up invite: %v", err)
	}
}

func TestSyncOverHTTP(t *testing.T) {
	w := newWorld(t)
	big := make([]byte, 3<<20) // 3 MiB of noise stands in for audio
	rand.New(rand.NewSource(1)).Read(big)
	write(t, w.ann.folder, "My Song.flp", []byte("project v1"))
	write(t, w.ann.folder, "Audio/take.wav", big)
	write(t, w.ann.folder, "Audio/Malibu — Camargue & co.mp3", []byte("music"))
	write(t, w.ann.folder, "Audio/empty.wav", nil)

	if res := w.sync(w.ann); !res.Pushed || res.Uploaded != 4 {
		t.Fatalf("first push: %+v", res)
	}
	if res := w.sync(w.bob); !res.Pulled || res.Downloaded != 4 {
		t.Fatalf("first pull: %+v", res)
	}
	w.same()
	if got, _ := os.ReadFile(filepath.Join(w.bob.folder, "Audio", "take.wav")); !bytes.Equal(got, big) {
		t.Fatal("the large file arrived damaged")
	}

	// Conflict: both edit the project file; the second to sync keeps a copy.
	write(t, w.ann.folder, "My Song.flp", []byte("ann's version"))
	write(t, w.bob.folder, "My Song.flp", []byte("bob's version"))
	w.sync(w.ann)
	res := w.sync(w.bob)
	if len(res.ConflictCopies) != 1 || read(w.bob.folder, "My Song.flp") != "ann's version" ||
		read(w.bob.folder, "My Song (conflict, bob, 2026-11-15).flp") != "bob's version" {
		t.Fatalf("conflict: %+v", res)
	}
	w.sync(w.ann)
	w.same()

	// History and restore.
	history, err := w.ann.remote.History()
	if err != nil || len(history) != 3 || history[0].Author != "bob" || !history[0].IsConflict || history[0].Device != "bob-pc" {
		t.Fatalf("history: %v %+v", err, history)
	}
	if _, err := w.ann.e.Restore(history[2].ID); err != nil {
		t.Fatal(err)
	}
	if read(w.ann.folder, "My Song.flp") != "project v1" {
		t.Error("restore did not bring the first version back")
	}
	w.sync(w.bob)
	w.same()
	_, err = w.ann.e.Restore("no-such-version")
	if engine.KindOf(err) != engine.KindNotFound {
		t.Errorf("restoring an unknown version: %v", err)
	}
}

func TestLocksOverHTTP(t *testing.T) {
	w := newWorld(t)
	write(t, w.ann.folder, "My Song.flp", []byte("v1"))
	w.sync(w.ann)
	w.sync(w.bob)

	// Ann starts a session: she holds the lock until she releases it.
	if err := w.ann.remote.AcquireLock(w.ann.e.BaseID()); err != nil {
		t.Fatal(err)
	}
	err := w.bob.remote.AcquireLock(w.bob.e.BaseID())
	var api *client.APIError
	if !errors.Is(err, core.ErrLockHeld) || !errors.As(err, &api) || api.Holder != "ann" {
		t.Fatalf("second acquire: %v", err)
	}
	if err := w.bob.remote.Heartbeat(); !errors.Is(err, core.ErrLockLost) {
		t.Errorf("heartbeat by a non-holder: %v", err)
	}
	if err := w.ann.remote.Heartbeat(); err != nil {
		t.Errorf("heartbeat by the holder: %v", err)
	}

	// Bob saves anyway: his work stays on his disk and nothing reaches the server.
	write(t, w.bob.folder, "My Song.flp", []byte("bob's draft"))
	head, _ := w.bob.remote.HeadID()
	_, err = w.bob.e.Sync(engine.PushOptions{})
	if engine.KindOf(err) != engine.KindLockNotHeld {
		t.Fatalf("sync while locked: %v", err)
	}
	if now, _ := w.bob.remote.HeadID(); now != head || read(w.bob.folder, "My Song.flp") != "bob's draft" {
		t.Fatal("the refused sync changed something")
	}

	// Bob asks for the lock; Ann's app can see who is asking.
	if err := w.bob.remote.RequestLock(); err != nil {
		t.Fatal(err)
	}
	p, _ := w.ann.c.Project(w.project.ID)
	if p.Lock == nil || p.Lock.User.Name != "ann" || p.Lock.RequestedBy != "bob" || p.Lock.Stale {
		t.Fatalf("lock view: %+v", p.Lock)
	}
	// Ann keeps working during her session without re-taking the lock.
	write(t, w.ann.folder, "My Song.flp", []byte("ann v2"))
	if _, err := engine.Open(w.ann.folder, w.ann.remote, "ann").Sync(engine.PushOptions{}); err != nil {
		t.Fatalf("holder's commit: %v", err)
	}
	// An editor cannot force the lock away; the holder releases it.
	if err := w.bob.remote.ForceRelease(); apiCode(err) != "forbidden" {
		t.Errorf("forced release by an editor: %v", err)
	}
	if err := w.ann.remote.ReleaseLock(); err != nil {
		t.Fatal(err)
	}
	// Now Bob's draft meets Ann's newer version: both are kept.
	res := w.sync(w.bob)
	if len(res.ConflictCopies) != 1 || read(w.bob.folder, "My Song.flp") != "ann v2" {
		t.Fatalf("after release: %+v", res)
	}
}

func TestServerRefusesBadInput(t *testing.T) {
	w := newWorld(t)
	file := filepath.Join(t.TempDir(), "kick.wav")
	os.WriteFile(file, []byte("kick"), 0o644)
	if err := w.ann.remote.UploadBlob(strings.Repeat("f", 64), file); apiCode(err) != "blob_mismatch" {
		t.Errorf("upload with a wrong hash: %v", err)
	}
	if err := w.ann.remote.UploadBlob("not-a-hash", file); apiCode(err) != "invalid_request" {
		t.Errorf("upload with a bad hash: %v", err)
	}
	if err := w.ann.remote.DownloadBlob(strings.Repeat("e", 64), filepath.Join(t.TempDir(), "x")); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("download of an unknown blob: %v", err)
	}
	w.ann.remote.AcquireLock("")
	sum := sha256.Sum256([]byte("kick"))
	escape := core.Manifest{Files: []core.FileEntry{{Path: "../../escape.wav", SHA256: hex.EncodeToString(sum[:]), Size: 4}}}
	if res, err := w.ann.remote.Commit(core.CommitRequest{Manifest: escape}); err != nil || res.Status != core.InvalidManifest {
		t.Errorf("commit with a path that escapes the folder: %v %+v", err, res)
	}
	missing := core.Manifest{Files: []core.FileEntry{{Path: "kick.wav", SHA256: hex.EncodeToString(sum[:]), Size: 4}}}
	if res, err := w.ann.remote.Commit(core.CommitRequest{Manifest: missing}); err != nil || res.Status != core.BlobsMissing || len(res.Missing) != 1 {
		t.Errorf("commit without its blobs: %v %+v", err, res)
	}
	if _, err := w.ann.remote.Manifest("nope"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("manifest of an unknown snapshot: %v", err)
	}
}

func TestJoinWithASessionSecretAndComments(t *testing.T) {
	w := newWorld(t)
	if !client.Reachable(w.url, time.Second) || client.Reachable("http://127.0.0.1:1", 300*time.Millisecond) {
		t.Fatal("Reachable is wrong about the server")
	}
	if _, _, err := client.Join(w.url, "anything", "carl"); !errors.Is(err, core.ErrJoinCode) {
		t.Fatalf("joining a server that has no session code: %v", err)
	}
	w.st.SetJoinSecret("open sesame")
	w.st.SetShareAll(true)
	if _, _, err := client.Join(w.url, "wrong", "carl"); !errors.Is(err, core.ErrJoinCode) {
		t.Fatalf("joining with a wrong secret: %v", err)
	}
	if _, _, err := client.Join(w.url, "open sesame", "ann"); !errors.Is(err, core.ErrNameTaken) {
		t.Fatalf("joining with a taken name: %v", err)
	}
	user, token, err := client.Join(w.url, "open sesame", " carl ")
	if err != nil || user.Name != "carl" {
		t.Fatalf("join: %v %+v", err, user)
	}
	carl := client.New(w.url, token, "carl-pc")
	list, err := carl.Projects()
	if err != nil || len(list) != 1 || list[0].Role != "editor" {
		t.Fatalf("the new member's projects: %v %+v", err, list)
	}

	// Comments on versions.
	write(t, w.ann.folder, "My Song.flp", []byte("v1"))
	w.sync(w.ann)
	history, _ := w.ann.remote.History()
	remote := carl.Remote(w.project.ID)
	c, err := remote.AddComment(history[0].ID, "love the intro")
	if err != nil || c.Author != "carl" {
		t.Fatalf("comment: %v %+v", err, c)
	}
	if _, err := remote.AddComment("nope", "hi"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("a comment on an unknown version: %v", err)
	}
	if _, err := remote.AddComment(history[0].ID, ""); apiCode(err) != "invalid_request" {
		t.Fatalf("an empty comment: %v", err)
	}
	comments, err := w.bob.remote.Comments()
	if err != nil || len(comments) != 1 || comments[0].Text != "love the intro" || comments[0].SnapshotID != history[0].ID {
		t.Fatalf("comments: %v %+v", err, comments)
	}
}
