package agent_test

import (
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"henosis/agent"
	"henosis/client"
	"henosis/config"
	"henosis/core"
	"henosis/fl"
	"henosis/flp/flptest"
	"henosis/server"
	"henosis/store"
)

// --- a small world: one server, two computers, a clock the test controls ----

type clock struct{ t time.Time }

func (c *clock) Now() time.Time { return c.t }

// wire lets a test cut a computer off from the server.
type wire struct {
	down atomic.Bool
	next http.RoundTripper
}

func (w *wire) RoundTrip(r *http.Request) (*http.Response, error) {
	if w.down.Load() {
		return nil, errors.New("simulated: no network")
	}
	return w.next.RoundTrip(r)
}

type seat struct {
	name   string
	c      *client.Client
	wire   *wire
	folder string
	det    *fl.Fake
	w      *agent.Worker
}

type bench struct {
	t       *testing.T
	st      *store.Store
	clock   *clock
	url     string
	project core.ProjectView
	ann     *seat
	bob     *seat
	mtime   time.Time
	dir     string
}

func newBench(t *testing.T, shared bool) *bench {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "server"))
	if err != nil {
		t.Fatal(err)
	}
	b := &bench{t: t, st: st, clock: &clock{t: time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)},
		mtime: time.Now().Add(-24 * time.Hour), dir: dir}
	st.Now = b.clock.Now
	srv := httptest.NewServer(server.New(st))
	t.Cleanup(srv.Close)
	b.url = srv.URL

	b.ann = b.newSeat("ann")
	b.bob = b.newSeat("bob")
	if b.project, err = b.ann.c.CreateProject("My Song"); err != nil {
		t.Fatal(err)
	}
	if shared {
		code, err := b.ann.c.CreateInvite(b.project.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := b.bob.c.AcceptInvite(code); err != nil {
			t.Fatal(err)
		}
		b.start(b.bob)
	}
	b.start(b.ann)
	return b
}

func (b *bench) newSeat(name string) *seat {
	b.t.Helper()
	_, token, err := b.st.AddUser(name)
	if err != nil {
		b.t.Fatal(err)
	}
	s := &seat{name: name, det: &fl.Fake{}, folder: filepath.Join(b.dir, name, "My Song")}
	s.c = client.New(b.url, token, name+"-pc")
	s.wire = &wire{next: s.c.HTTP.Transport}
	s.c.HTTP.Transport = s.wire
	if err := os.MkdirAll(s.folder, 0o755); err != nil {
		b.t.Fatal(err)
	}
	return s
}

// start (re)creates the seat's worker, like starting the app.
func (b *bench) start(s *seat) {
	b.t.Helper()
	me, err := s.c.Me()
	if err != nil {
		b.t.Fatal(err)
	}
	link := config.Link{ProjectID: b.project.ID, Name: "My Song", Folder: s.folder}
	s.w = agent.NewWorker(link, s.c.Remote(b.project.ID), me, s.c.Device, s.det, agent.DefaultTiming(), b.clock.Now)
}

// write puts a file into a folder with a modification time no earlier write used.
func (b *bench) write(s *seat, rel, content string) {
	b.t.Helper()
	path := filepath.Join(s.folder, filepath.FromSlash(rel))
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		b.t.Fatal(err)
	}
	b.mtime = b.mtime.Add(time.Second)
	os.Chtimes(path, b.mtime, b.mtime)
}

func read(s *seat, rel string) string {
	data, err := os.ReadFile(filepath.Join(s.folder, filepath.FromSlash(rel)))
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
			out[rel] = string(data)
		}
		return nil
	})
	return out
}

// pass lets time go by, second by second, with every started worker ticking.
func (b *bench) pass(seconds int) {
	for i := 0; i < seconds; i++ {
		b.clock.t = b.clock.t.Add(time.Second)
		for _, s := range []*seat{b.ann, b.bob} {
			if s.w != nil {
				s.w.Tick()
			}
		}
	}
}

func (b *bench) head() core.ProjectView {
	b.t.Helper()
	p, err := b.ann.c.Project(b.project.ID)
	if err != nil {
		b.t.Fatal(err)
	}
	return p
}

func (b *bench) versions() []core.Snapshot {
	b.t.Helper()
	list, err := b.ann.c.Remote(b.project.ID).History()
	if err != nil {
		b.t.Fatal(err)
	}
	return list
}

func (b *bench) same() {
	b.t.Helper()
	if !reflect.DeepEqual(tree(b.ann.folder), tree(b.bob.folder)) {
		b.t.Fatalf("folders differ:\n ann %v\n bob %v", tree(b.ann.folder), tree(b.bob.folder))
	}
}

func (b *bench) want(s *seat, state, headline string) agent.Status {
	b.t.Helper()
	st := s.w.Status()
	if st.State != state || st.Headline != headline {
		b.t.Fatalf("%s: status is %q / %q (%s), want %q / %q", s.name, st.State, st.Headline, st.Detail, state, headline)
	}
	return st
}

func opens(s *seat, caption string) {
	s.det.State = fl.State{Running: true, Captions: []string{caption}}
}
func closes(s *seat) { s.det.State = fl.State{} }

const caption = "My Song.flp - FL Studio 2025"

// --- tests -------------------------------------------------------------------

func TestSoloSavesAreUploadedOnceQuiet(t *testing.T) {
	b := newBench(t, false)
	b.pass(3)
	b.want(b.ann, "synced", "Up to date")
	if len(b.versions()) != 0 {
		t.Fatal("an empty folder produced a version")
	}

	b.write(b.ann, "My Song.flp", "v1")
	b.write(b.ann, "Audio/kick.wav", "kick")
	b.pass(1)
	if len(b.versions()) != 0 {
		t.Fatal("uploaded before the files had settled")
	}
	b.pass(3)
	if v := b.versions(); len(v) != 1 || v[0].Message != "Added 2 files" || v[0].Author != "ann" {
		t.Fatalf("versions: %+v", v)
	}
	st := b.want(b.ann, "synced", "Up to date")
	if st.Shared || st.UnsentChanges || st.LastSync == "" || st.LastActivity != "Uploaded your changes" {
		t.Fatalf("status: %+v", st)
	}

	// While FL Studio is in the middle of a save, nothing is uploaded.
	b.write(b.ann, "My Song.flp~", "half written")
	b.pass(10)
	if len(b.versions()) != 1 {
		t.Fatal("uploaded during a save")
	}
	os.Remove(filepath.Join(b.ann.folder, "My Song.flp~"))
	b.write(b.ann, "My Song.flp", "v2")
	b.pass(4)
	if v := b.versions(); len(v) != 2 || v[0].Message != "Changed My Song.flp" {
		t.Fatalf("versions after the save: %+v", v)
	}
	// Backups and the app's own folder never leave the computer.
	b.write(b.ann, "Backup/My Song (overwritten at 12h00).flp", "old")
	b.pass(5)
	if len(b.versions()) != 2 {
		t.Fatal("a backup file caused an upload")
	}
}

func TestIdlePartnersFollowEachOther(t *testing.T) {
	b := newBench(t, true)
	b.write(b.ann, "My Song.flp", "v1")
	b.pass(10)
	b.same()
	b.write(b.bob, "Audio/snare.wav", "snare")
	b.pass(10)
	b.same()
	if len(tree(b.ann.folder)) != 2 || len(b.versions()) != 2 {
		t.Fatalf("ann has %v, versions %d", tree(b.ann.folder), len(b.versions()))
	}
	st := b.want(b.bob, "synced", "Up to date")
	if !st.Shared || !reflect.DeepEqual(st.Members, []string{"ann", "bob"}) || st.Role != "editor" || st.RetentionDays != 3 {
		t.Fatalf("status: %+v", st)
	}
	if b.head().Lock != nil {
		t.Fatal("a lock was left behind")
	}
}

func TestEditingSession(t *testing.T) {
	b := newBench(t, true)
	b.write(b.ann, "My Song.flp", "v1")
	b.pass(10)

	opens(b.ann, caption)
	b.pass(2)
	st := b.want(b.ann, "editing", "You are editing")
	if lock := b.head().Lock; lock == nil || lock.User.Name != "ann" || !st.HoldingLock || !st.OpenInFL {
		t.Fatalf("lock after opening: %+v", lock)
	}
	b.pass(6)
	if st := b.want(b.bob, "synced", "Up to date"); st.LockHolder != "ann" || st.Detail != "ann is editing right now." {
		t.Fatalf("bob's view: %+v", st)
	}

	// Every save goes up while the lock stays with ann; bob follows.
	for i, content := range []string{"v2", "v3", "v4"} {
		b.write(b.ann, "My Song.flp", content)
		b.pass(8)
		if got := read(b.bob, "My Song.flp"); got != content {
			t.Fatalf("save %d: bob has %q", i, got)
		}
		if lock := b.head().Lock; lock == nil || lock.User.Name != "ann" {
			t.Fatalf("save %d: the lock moved: %+v", i, lock)
		}
	}
	// The lock stays alive over a long session.
	b.pass(15 * 60)
	if lock := b.head().Lock; lock == nil || lock.Stale {
		t.Fatalf("the lock went stale during the session: %+v", lock)
	}

	// A dialog covering the caption for a moment does not end the session.
	b.ann.det.State = fl.State{Running: true, Captions: []string{"FL Studio 2025"}}
	b.pass(2)
	if lock := b.head().Lock; lock == nil || lock.User.Name != "ann" {
		t.Fatal("a short caption change gave the lock away")
	}
	opens(b.ann, caption)
	b.pass(2)
	if !b.ann.w.Status().HoldingLock {
		t.Fatal("a short caption change ended the session")
	}

	// Closing: the last save is uploaded, then the lock is given back.
	b.write(b.ann, "My Song.flp", "final")
	closes(b.ann)
	b.pass(12)
	if b.head().Lock != nil {
		t.Fatalf("the lock was not given back: %+v", b.head().Lock)
	}
	b.same()
	if read(b.bob, "My Song.flp") != "final" {
		t.Fatal("the last save did not arrive")
	}
	b.want(b.ann, "synced", "Up to date")
}

func TestSecondEditorKeepsBothVersions(t *testing.T) {
	b := newBench(t, true)
	b.write(b.ann, "My Song.flp", "v1")
	b.pass(10)
	opens(b.ann, caption)
	b.pass(3)

	// Bob opens the project while ann is editing.
	opens(b.bob, caption)
	b.pass(6)
	st := b.want(b.bob, "waiting", "ann is editing")
	if st.HoldingLock || st.LockHolder != "ann" {
		t.Fatalf("bob's status: %+v", st)
	}
	// Bob saves anyway: nothing is uploaded, nothing is lost.
	b.write(b.bob, "My Song.flp", "bob's idea")
	b.pass(8)
	if len(b.versions()) != 1 || !b.bob.w.Status().UnsentChanges {
		t.Fatalf("bob's save was uploaded or forgotten: %d versions", len(b.versions()))
	}
	// Ann saves; bob's open project is not touched underneath him.
	b.write(b.ann, "My Song.flp", "ann's work")
	b.pass(8)
	if read(b.bob, "My Song.flp") != "bob's idea" {
		t.Fatal("a download changed a project that is open in FL Studio")
	}
	// Ann is done. Bob is still looking at his old version.
	closes(b.ann)
	b.pass(10)
	st = b.want(b.bob, "attention", "A newer version is waiting")
	if !strings.Contains(st.Detail, "kept as a separate copy") || b.head().Lock != nil {
		t.Fatalf("bob's status: %+v, lock %+v", st, b.head().Lock)
	}
	// Bob closes: ann's version arrives, his own is kept beside it, and both travel.
	closes(b.bob)
	b.pass(15)
	if read(b.bob, "My Song.flp") != "ann's work" {
		t.Fatalf("bob has %q", read(b.bob, "My Song.flp"))
	}
	st = b.want(b.bob, "synced", "Up to date")
	if len(st.Conflicts) != 1 || read(b.bob, st.Conflicts[0]) != "bob's idea" || !strings.Contains(st.Conflicts[0], "(conflict, bob, ") {
		t.Fatalf("conflict copies: %v", st.Conflicts)
	}
	b.same()
	if v := b.versions(); len(v) != 3 || !v[0].IsConflict || v[0].Author != "bob" {
		t.Fatalf("versions: %+v", v)
	}
}

func TestWaitingEditorGetsTheProjectWhenItIsFree(t *testing.T) {
	b := newBench(t, true)
	b.write(b.ann, "My Song.flp", "v1")
	b.pass(10)
	opens(b.ann, caption)
	b.pass(3)
	opens(b.bob, caption)
	b.pass(6)
	b.write(b.bob, "My Song.flp", "bob's take")
	b.pass(6)
	b.want(b.bob, "waiting", "ann is editing")

	// Ann closes without saving: nothing newer exists, so bob simply takes over.
	closes(b.ann)
	b.pass(15)
	st := b.want(b.bob, "editing", "You are editing")
	if lock := b.head().Lock; lock == nil || lock.User.Name != "bob" || st.UnsentChanges {
		t.Fatalf("after ann left: lock %+v, status %+v", lock, st)
	}
	if read(b.ann, "My Song.flp") != "bob's take" || len(b.versions()) != 2 {
		t.Fatalf("bob's waiting save did not reach ann: %q", read(b.ann, "My Song.flp"))
	}
}

func TestLockOfAVanishedComputerIsTakenOver(t *testing.T) {
	b := newBench(t, true)
	b.write(b.ann, "My Song.flp", "v1")
	b.pass(10)
	opens(b.ann, caption)
	b.pass(3)

	// Ann's computer disappears without closing anything.
	annWorker := b.ann.w
	b.ann.w = nil
	opens(b.bob, caption)
	b.pass(60)
	b.want(b.bob, "waiting", "ann is editing")
	b.pass(11 * 60)
	b.want(b.bob, "editing", "You are editing")
	b.write(b.bob, "My Song.flp", "bob v2")
	b.pass(6)

	// Ann's computer comes back with the project still open and an unsent save.
	b.ann.w = annWorker
	b.write(b.ann, "My Song.flp", "ann offline work")
	b.pass(40)
	st := b.want(b.ann, "waiting", "bob is editing")
	if st.HoldingLock || !st.UnsentChanges || read(b.ann, "My Song.flp") != "ann offline work" {
		t.Fatalf("ann after coming back: %+v", st)
	}
	if lock := b.head().Lock; lock == nil || lock.User.Name != "bob" {
		t.Fatalf("lock: %+v", lock)
	}
	// Both close; ann's offline work survives as a copy.
	closes(b.ann)
	closes(b.bob)
	b.pass(20)
	b.same()
	if read(b.ann, "My Song.flp") != "bob v2" || len(b.ann.w.Status().Conflicts) != 1 {
		t.Fatalf("after closing: %v", tree(b.ann.folder))
	}
}

func TestHandingTheProjectOver(t *testing.T) {
	b := newBench(t, true)
	b.write(b.ann, "My Song.flp", "v1")
	b.pass(10)
	opens(b.ann, caption)
	b.pass(3)
	opens(b.bob, caption)
	b.pass(6)

	if err := b.bob.w.AskForLock(); err != nil {
		t.Fatal(err)
	}
	b.pass(6)
	if st := b.want(b.ann, "editing", "You are editing"); st.LockRequestedBy != "bob" || !strings.Contains(st.Detail, "bob would like to edit") {
		t.Fatalf("ann does not see the request: %+v", st)
	}
	// Ann has one more save, then hands over; the save goes up first.
	b.write(b.ann, "My Song.flp", "ann's last")
	if err := b.ann.w.Yield(); err != nil {
		t.Fatal(err)
	}
	if v := b.versions(); len(v) != 2 || b.head().Lock != nil {
		t.Fatalf("after yielding: %d versions, lock %+v", len(v), b.head().Lock)
	}
	b.want(b.ann, "waiting", "You handed the project over")
	// Bob still has the old version open, so he must reopen to edit.
	b.pass(8)
	b.want(b.bob, "attention", "A newer version is waiting")
	closes(b.bob)
	b.pass(8)
	opens(b.bob, caption)
	b.pass(8)
	b.want(b.bob, "editing", "You are editing")
	if read(b.bob, "My Song.flp") != "ann's last" {
		t.Fatalf("bob has %q", read(b.bob, "My Song.flp"))
	}
	// Ann does not grab the lock back while bob works, and can ask for it later.
	b.pass(30)
	b.want(b.ann, "waiting", "bob is editing")
	if err := b.ann.w.Yield(); err == nil {
		t.Fatal("yielding without the lock should fail")
	}
	closes(b.bob)
	b.ann.w.TakeLock()
	b.pass(12)
	if st := b.ann.w.Status(); !st.HoldingLock || st.Yielded {
		t.Fatalf("ann could not take the project back: %+v", st)
	}
}

func TestOwnerCanBreakALock(t *testing.T) {
	b := newBench(t, true)
	b.write(b.ann, "My Song.flp", "v1")
	b.pass(10)
	opens(b.bob, caption)
	b.pass(5)
	bobWorker := b.bob.w
	b.bob.w = nil // bob's computer is gone, the lock is still fresh
	if err := bobWorker.BreakLock(); err != nil {
		t.Fatalf("the holder breaking his own lock: %v", err)
	}
	opens(b.bob, caption)
	b.bob.w = bobWorker
	b.pass(5)
	b.bob.w = nil
	if b.head().Lock == nil {
		t.Fatal("bob should hold the lock again")
	}
	if err := b.ann.w.BreakLock(); err != nil {
		t.Fatalf("the owner could not break the lock: %v", err)
	}
	if b.head().Lock != nil {
		t.Fatal("the lock is still there")
	}
	// An editor may not break someone else's lock.
	opens(b.ann, caption)
	b.pass(5)
	if err := bobWorker.BreakLock(); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("an editor broke the owner's lock: %v", err)
	}
}

func TestMassDeleteNeedsAYes(t *testing.T) {
	b := newBench(t, false)
	for _, name := range []string{"a.wav", "b.wav", "c.wav", "d.wav"} {
		b.write(b.ann, name, name)
	}
	b.pass(6)
	for _, name := range []string{"a.wav", "b.wav", "c.wav"} {
		os.Remove(filepath.Join(b.ann.folder, name))
	}
	b.pass(30)
	st := b.want(b.ann, "attention", "Check before uploading")
	if st.ConfirmDelete == "" || len(b.versions()) != 1 {
		t.Fatalf("status %+v, versions %d", st, len(b.versions()))
	}
	b.ann.w.ConfirmDelete()
	b.pass(4)
	b.want(b.ann, "synced", "Up to date")
	if v := b.versions(); len(v) != 2 || v[0].Message != "Removed 3 files" {
		t.Fatalf("versions: %+v", v)
	}
	// The yes was for that one upload only.
	b.write(b.ann, "e.wav", "e")
	b.write(b.ann, "f.wav", "f")
	b.pass(6)
	for _, name := range []string{"d.wav", "e.wav"} {
		os.Remove(filepath.Join(b.ann.folder, name))
	}
	b.pass(10)
	b.want(b.ann, "attention", "Check before uploading")
}

func TestOfflineSavesAreKeptAndSentLater(t *testing.T) {
	b := newBench(t, false)
	b.write(b.ann, "My Song.flp", "v1")
	b.pass(6)
	b.ann.wire.down.Store(true)
	b.write(b.ann, "My Song.flp", "v2 offline")
	b.pass(30)
	st := b.want(b.ann, "offline", "Offline")
	if !st.UnsentChanges || st.Online {
		t.Fatalf("status: %+v", st)
	}
	b.ann.wire.down.Store(false)
	b.pass(70)
	b.want(b.ann, "synced", "Up to date")
	if v := b.versions(); len(v) != 2 {
		t.Fatalf("versions: %d", len(v))
	}
}

func TestMissingFolderDeletesNothing(t *testing.T) {
	b := newBench(t, true)
	b.write(b.ann, "My Song.flp", "v1")
	b.pass(10)
	moved := b.ann.folder + " moved"
	if err := os.Rename(b.ann.folder, moved); err != nil {
		t.Fatal(err)
	}
	b.pass(30)
	b.want(b.ann, "problem", "Folder not found")
	if len(b.versions()) != 1 || read(b.bob, "My Song.flp") != "v1" {
		t.Fatal("a missing folder was treated as a deletion")
	}
	os.Rename(moved, b.ann.folder)
	b.pass(10)
	b.want(b.ann, "synced", "Up to date")
	if len(b.versions()) != 1 {
		t.Fatal("putting the folder back made a new version")
	}
}

func TestRestartWhileHoldingTheLock(t *testing.T) {
	b := newBench(t, true)
	b.write(b.ann, "My Song.flp", "v1")
	b.pass(10)
	opens(b.ann, caption)
	b.pass(3)
	b.write(b.ann, "My Song.flp", "saved just before the crash")

	// The app is restarted; FL Studio has been closed in the meantime.
	closes(b.ann)
	b.start(b.ann)
	b.pass(12)
	if b.head().Lock != nil {
		t.Fatalf("the lock survived the restart: %+v", b.head().Lock)
	}
	if read(b.bob, "My Song.flp") != "saved just before the crash" {
		t.Fatal("the save made before the restart was not uploaded")
	}
	// Restart while FL Studio still has the project open: the session goes on.
	opens(b.ann, caption)
	b.pass(3)
	b.start(b.ann)
	b.pass(3)
	if st := b.ann.w.Status(); !st.HoldingLock || st.State != "editing" {
		t.Fatalf("the session did not continue: %+v", st)
	}
}

func TestRemovedMemberStopsSyncing(t *testing.T) {
	b := newBench(t, true)
	b.write(b.ann, "My Song.flp", "v1")
	b.pass(10)
	bob, _ := b.bob.c.Me()
	if err := b.ann.c.RemoveMember(b.project.ID, bob.ID); err != nil {
		t.Fatal(err)
	}
	b.write(b.bob, "My Song.flp", "bob after removal")
	b.pass(12)
	b.want(b.bob, "problem", "No longer available")
	if read(b.bob, "My Song.flp") != "bob after removal" || len(b.versions()) != 1 {
		t.Fatal("a removed member still synced")
	}
	// The project is solo again: ann edits without any lock.
	opens(b.ann, caption)
	b.write(b.ann, "My Song.flp", "v2")
	b.pass(8)
	if st := b.want(b.ann, "editing", "Open in FL Studio"); st.Shared || st.HoldingLock {
		t.Fatalf("status: %+v", st)
	}
	if len(b.versions()) != 2 {
		t.Fatal("ann's save was not uploaded")
	}
}

func TestProjectRecognisedByTitleAndBySave(t *testing.T) {
	b := newBench(t, true)
	b.write(b.ann, "My Song.flp", string(flptest.Project("Night Drive", nil, "1")))
	b.pass(10)

	// The caption shows the title stored in the file, not the file name.
	opens(b.ann, "Night Drive - FL Studio 2025")
	b.pass(3)
	if !b.ann.w.Status().HoldingLock {
		t.Fatal("the project was not recognised by its title")
	}
	closes(b.ann)
	b.pass(8)

	// A brand-new title that is in no file yet: the first save gives it away.
	opens(b.ann, "Fresh Title - FL Studio 2025")
	b.pass(5)
	if b.ann.w.Status().OpenInFL {
		t.Fatal("an unknown title was taken for this project")
	}
	b.write(b.ann, "My Song.flp", string(flptest.Project("Fresh Title", nil, "2")))
	b.pass(6)
	if st := b.ann.w.Status(); !st.OpenInFL || !st.HoldingLock {
		t.Fatalf("the save did not mark the project as open: %+v", st)
	}
	v := b.versions()
	if v[0].FLVersion != "25.1.6.4997" || !reflect.DeepEqual(v[0].Plugins, []string{"Fruity Limiter"}) {
		t.Fatalf("the version lacks the project details: %+v", v[0])
	}
}

// lateOpener reports FL Studio as closed until a download has started, like a
// user opening the project while a new version is still on its way.
type lateOpener struct {
	calls int
	after int
}

func (d *lateOpener) Detect() (fl.State, error) {
	d.calls++
	if d.calls > d.after {
		return fl.State{Running: true, Captions: []string{caption}}, nil
	}
	return fl.State{}, nil
}

func TestProjectOpenedDuringADownloadIsNotTouched(t *testing.T) {
	b := newBench(t, true)
	b.write(b.ann, "My Song.flp", "v1")
	b.pass(10)
	b.bob.w = nil
	b.write(b.ann, "My Song.flp", "v2")
	b.pass(6)

	me, _ := b.bob.c.Me()
	late := &lateOpener{after: 1}
	link := config.Link{ProjectID: b.project.ID, Name: "My Song", Folder: b.bob.folder}
	b.bob.w = agent.NewWorker(link, b.bob.c.Remote(b.project.ID), me, b.bob.c.Device, late, agent.DefaultTiming(), b.clock.Now)
	b.pass(1) // the tick sees FL Studio closed; the check before applying sees it open
	if read(b.bob, "My Song.flp") != "v1" {
		t.Fatal("the project changed underneath FL Studio")
	}
	b.pass(8)
	if st := b.bob.w.Status(); !st.OpenInFL || !st.NewerOnServer || read(b.bob, "My Song.flp") != "v1" {
		t.Fatalf("status: %+v", st)
	}
}

func TestRestoreThroughTheWorker(t *testing.T) {
	b := newBench(t, true)
	b.write(b.ann, "My Song.flp", "v1")
	b.pass(8)
	b.write(b.ann, "My Song.flp", "v2")
	b.pass(8)
	history, err := b.ann.w.History()
	if err != nil || len(history) != 2 {
		t.Fatalf("history: %v %+v", err, history)
	}
	first := history[1].ID

	opens(b.ann, caption)
	b.pass(3)
	if err := b.ann.w.Restore(first); err == nil || !strings.Contains(err.Error(), "close the project") {
		t.Fatalf("restore while open: %v", err)
	}
	closes(b.ann)
	b.pass(8)
	opens(b.bob, caption)
	b.pass(6)
	if err := b.ann.w.Restore(first); err == nil || !strings.Contains(err.Error(), "bob is editing") {
		t.Fatalf("restore while a partner edits: %v", err)
	}
	closes(b.bob)
	b.pass(8)
	if err := b.ann.w.Restore(first); err != nil {
		t.Fatal(err)
	}
	b.pass(10)
	if read(b.ann, "My Song.flp") != "v1" || read(b.bob, "My Song.flp") != "v1" || len(b.versions()) != 3 {
		t.Fatalf("after restore: ann %q bob %q versions %d", read(b.ann, "My Song.flp"), read(b.bob, "My Song.flp"), len(b.versions()))
	}
	if err := b.ann.w.Restore("nope"); err == nil {
		t.Fatal("restoring an unknown version succeeded")
	}
}

func TestCollectSamples(t *testing.T) {
	b := newBench(t, true)
	outside := filepath.Join(b.dir, "downloads")
	os.MkdirAll(filepath.Join(outside, "other"), 0o755)
	kick := filepath.Join(outside, "kick.wav")
	vocal := filepath.Join(outside, "vocal take.wav")
	twin := filepath.Join(outside, "other", "kick.wav")
	gone := filepath.Join(outside, "deleted.wav")
	os.WriteFile(kick, []byte("KICK"), 0o644)
	os.WriteFile(vocal, []byte("VOCAL"), 0o644)
	os.WriteFile(twin, []byte("ANOTHER KICK"), 0o644)
	inside := filepath.Join(b.ann.folder, "Audio", "hat.wav")
	b.write(b.ann, "Audio/hat.wav", "HAT")
	samples := []string{kick, vocal, twin, gone, inside, `%FLStudioFactoryData%\Data\Patches\Packs\Drums\Clap.wav`}
	b.write(b.ann, "My Song.flp", string(flptest.Project("", samples, "")))
	b.pass(8)

	// The samples that are on this computer were copied next to the project
	// file by themselves, and the partner got them, with nobody pressing
	// anything.
	if read(b.ann, "kick.wav") != "KICK" || read(b.ann, "vocal take.wav") != "VOCAL" {
		t.Fatalf("the samples were not copied next to the project file: %v", tree(b.ann.folder))
	}
	b.pass(10)
	if read(b.bob, "kick.wav") != "KICK" || read(b.bob, "vocal take.wav") != "VOCAL" {
		t.Fatalf("bob did not get the samples: %v", tree(b.bob.folder))
	}
	// What is left to say: a different sample with a name already taken,
	// and one that is on no computer at all.
	st := b.ann.w.Status()
	if !reflect.DeepEqual(st.OutsideSamples, []string{twin}) || !reflect.DeepEqual(st.MissingSamples, []string{"deleted.wav"}) {
		t.Fatalf("after copying: clashes %q, missing %q", st.OutsideSamples, st.MissingSamples)
	}
	if read(b.ann, "kick.wav") != "KICK" {
		t.Fatal("the same-named sample replaced the first one")
	}
	// Collecting by hand finds nothing more, and overwrites nothing.
	result, err := b.ann.w.CollectSamples()
	want := agent.CollectResult{NotFound: []string{gone}, Clashes: []string{twin}}
	if err != nil || !reflect.DeepEqual(result, want) {
		t.Fatalf("collect by hand: %+v %v", result, err)
	}
	os.WriteFile(vocal, []byte("CHANGED ON DISK"), 0o644)
	b.pass(5)
	if read(b.ann, "vocal take.wav") != "VOCAL" {
		t.Fatal("a copy in the project was overwritten")
	}
}

// A sample that is only on the other computer is reported as missing here,
// and arrives once the computer that has it runs Henosis.
func TestSampleOnlyOnTheOtherComputer(t *testing.T) {
	b := newBench(t, true)
	elsewhere := filepath.Join(b.dir, "roman downloads", "Mystic Stylez Open Hat.wav")
	samples := []string{elsewhere}
	// Ann's project refers to a sample that is not on disk yet (her pack
	// is still downloading), so she cannot copy it.
	b.write(b.ann, "My Song.flp", string(flptest.Project("", samples, "")))
	b.pass(10)
	if st := b.bob.w.Status(); !reflect.DeepEqual(st.MissingSamples, []string{"Mystic Stylez Open Hat.wav"}) {
		t.Fatalf("bob's missing samples: %q", st.MissingSamples)
	}
	// The pack finishes downloading at ann's: the hat travels on its own.
	os.MkdirAll(filepath.Dir(elsewhere), 0o755)
	os.WriteFile(elsewhere, []byte("OPEN HAT"), 0o644)
	b.pass(12)
	if read(b.bob, "Mystic Stylez Open Hat.wav") != "OPEN HAT" {
		t.Fatalf("the hat did not reach bob: %v", tree(b.bob.folder))
	}
	if st := b.bob.w.Status(); len(st.MissingSamples) != 0 {
		t.Fatalf("still reported missing at bob's: %q", st.MissingSamples)
	}
}

func TestStopGivesTheLockBack(t *testing.T) {
	b := newBench(t, true)
	b.write(b.ann, "My Song.flp", "v1")
	b.pass(10)
	opens(b.ann, caption)
	b.pass(3)
	b.ann.w.Stop()
	if b.head().Lock != nil {
		t.Fatal("stopping the app left the lock behind")
	}
}

// The project file names a sample folder that was renamed afterwards: the
// sample is found by its name and travels with the project.
func TestRenamedSampleFolderStillTravels(t *testing.T) {
	b := newBench(t, true)
	pack := filepath.Join(b.dir, "downloads", "Apoc Krysis Vol 1-4")
	real := filepath.Join(pack, "Drum Kit Vol.IV", "Perc & Hi-Hats", "Mystic Stylez Open Hat.wav")
	os.MkdirAll(filepath.Dir(real), 0o755)
	os.WriteFile(real, []byte("OPEN HAT"), 0o644)
	stored := filepath.Join(pack, "Drum Kit Vol . IV", "Perc Hi Hats", "Mystic Stylez Open Hat.wav")
	b.write(b.ann, "My Song.flp", string(flptest.Project("", []string{stored}, "")))
	b.pass(12)
	if read(b.ann, "Mystic Stylez Open Hat.wav") != "OPEN HAT" || read(b.bob, "Mystic Stylez Open Hat.wav") != "OPEN HAT" {
		t.Fatalf("the hat did not travel: ann %v, bob %v", tree(b.ann.folder), tree(b.bob.folder))
	}
	if st := b.ann.w.Status(); len(st.MissingSamples)+len(st.OutsideSamples) != 0 {
		t.Fatalf("still reported: missing %q, outside %q", st.MissingSamples, st.OutsideSamples)
	}
}
