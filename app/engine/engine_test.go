package engine_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"henosis/core"
	"henosis/engine"
	"henosis/store"
)

// --- helpers -----------------------------------------------------------------

func write(t *testing.T, folder, rel, content string) {
	t.Helper()
	path := filepath.Join(folder, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
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

func remove(t *testing.T, folder, rel string) {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(folder, filepath.FromSlash(rel))); err != nil {
		t.Fatal(err)
	}
}

// tree maps relative path to content for every synced file.
func tree(folder string) map[string]string {
	out := map[string]string{}
	filepath.WalkDir(folder, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(folder, path)
		rel = filepath.ToSlash(rel)
		if !core.IsIgnored(rel) {
			data, _ := os.ReadFile(path)
			out[rel] = string(data)
		}
		return nil
	})
	return out
}

func sha(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// flaky wraps a remote, counts calls and can be told to fail.
type flaky struct {
	engine.Remote
	uploads, downloads   int
	failUploadAt         int // the n-th upload from now fails; 0 = never
	failDownloadAt       int
	loseNextCommitAnswer bool
}

func (f *flaky) UploadBlob(sha256, file string) error {
	if f.failUploadAt > 0 {
		if f.failUploadAt--; f.failUploadAt == 0 {
			return errors.New("simulated network failure")
		}
	}
	f.uploads++
	return f.Remote.UploadBlob(sha256, file)
}

func (f *flaky) DownloadBlob(sha256, destination string) error {
	if f.failDownloadAt > 0 {
		if f.failDownloadAt--; f.failDownloadAt == 0 {
			return errors.New("simulated network failure")
		}
	}
	f.downloads++
	return f.Remote.DownloadBlob(sha256, destination)
}

func (f *flaky) Commit(req core.CommitRequest) (core.CommitResult, error) {
	res, err := f.Remote.Commit(req)
	if f.loseNextCommitAnswer {
		f.loseNextCommitAnswer = false
		return core.CommitResult{}, errors.New("simulated lost response")
	}
	return res, err
}

// world is two project folders ("ann" and "bob") sharing one project on one
// store. Each commit takes the lock just for the commit.
type world struct {
	t          *testing.T
	st         *store.Store
	projectID  string
	a, b       string
	ra, rb     *flaky
	direct     *store.ProjectRemote // ann's remote without wrappers
	ann, bob   *engine.Engine
	annID      string
	blobFolder string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "server"))
	if err != nil {
		t.Fatal(err)
	}
	ann, _, _ := st.AddUser("ann")
	bob, _, _ := st.AddUser("bob")
	p, err := st.CreateProject(ann.ID, "My Song")
	if err != nil {
		t.Fatal(err)
	}
	code, _, _ := st.CreateInvite(ann.ID, p.ID)
	if _, err := st.AcceptInvite(bob.ID, code); err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, st: st, projectID: p.ID, annID: ann.ID,
		a: filepath.Join(dir, "ann", "My Song"), b: filepath.Join(dir, "bob", "My Song"),
		blobFolder: filepath.Join(dir, "server", "projects", p.ID, "blobs")}
	os.MkdirAll(w.a, 0o755)
	os.MkdirAll(w.b, 0o755)
	w.direct = &store.ProjectRemote{Store: st, UserID: ann.ID, ProjectID: p.ID, Device: "ann-pc"}
	bobDirect := &store.ProjectRemote{Store: st, UserID: bob.ID, ProjectID: p.ID, Device: "bob-pc"}
	w.ra = &flaky{Remote: engine.AutoLock{Remote: w.direct, Locker: w.direct}}
	w.rb = &flaky{Remote: engine.AutoLock{Remote: bobDirect, Locker: bobDirect}}
	w.reopenAnn()
	w.reopenBob()
	return w
}

// Reopening an engine reloads its state from disk, like an app restart.
func (w *world) reopenAnn() {
	w.ann = engine.Open(w.a, w.ra, "ann")
	w.ann.Today = func() string { return "2026-11-15" }
}

func (w *world) reopenBob() {
	w.bob = engine.Open(w.b, w.rb, "bob")
	w.bob.Today = func() string { return "2026-11-15" }
}

func (w *world) headID() string {
	id, err := w.direct.HeadID()
	if err != nil {
		w.t.Fatal(err)
	}
	return id
}

func (w *world) history() []core.Snapshot {
	h, err := w.direct.History()
	if err != nil {
		w.t.Fatal(err)
	}
	return h
}

func (w *world) sync(e *engine.Engine) engine.Result {
	w.t.Helper()
	res, err := e.Sync(engine.PushOptions{})
	if err != nil {
		w.t.Fatalf("sync failed: %v", err)
	}
	return res
}

func (w *world) same() {
	w.t.Helper()
	if a, b := tree(w.a), tree(w.b); !reflect.DeepEqual(a, b) {
		w.t.Fatalf("folders differ:\n ann: %v\n bob: %v", keys(a), keys(b))
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func wantKind(t *testing.T, err error, kind engine.Kind) {
	t.Helper()
	if engine.KindOf(err) != kind {
		t.Fatalf("error kind = %d (%v), want %d", engine.KindOf(err), err, kind)
	}
}

const conflictCopy = "My Song (conflict, bob, 2026-11-15).flp"

// --- scanner -----------------------------------------------------------------

func TestScanner(t *testing.T) {
	root := filepath.Join(t.TempDir(), "proj")
	write(t, root, "My Song.flp", "project")
	write(t, root, "Audio/kick.wav", "kick")
	write(t, root, "Audio/empty.wav", "")
	write(t, root, "Audio/Malibu — Camargue.mp3", "music")
	write(t, root, "Backup/My Song (overwritten at 19h26).flp", "old")
	write(t, root, ".henosis/state.json", "{}")
	write(t, root, "Thumbs.db", "junk")

	scan, index, err := engine.ScanFolder(root, nil)
	if err != nil || len(scan.Manifest.Files) != 4 || scan.Hashed != 4 {
		t.Fatalf("scan: %v files=%d hashed=%d", err, len(scan.Manifest.Files), scan.Hashed)
	}
	if e := scan.Manifest.Find("Audio/kick.wav"); e == nil || e.SHA256 != sha("kick") || e.Size != 4 {
		t.Error("kick.wav is wrong")
	}
	if scan.Manifest.Find("Audio/Malibu — Camargue.mp3") == nil || scan.Manifest.Find("Thumbs.db") != nil {
		t.Error("wrong set of files")
	}
	// Fresh files are not cached, so a rewrite within the same tick is never missed.
	if again, _, _ := engine.ScanFolder(root, index); again.Hashed != 4 {
		t.Errorf("fresh files were served from the cache (hashed %d)", again.Hashed)
	}
	// Once a file is older than two seconds it is hashed only when it changes.
	old := time.Now().Add(-30 * time.Second)
	for _, e := range scan.Manifest.Files {
		os.Chtimes(filepath.Join(root, filepath.FromSlash(e.Path)), old, old)
	}
	_, index, _ = engine.ScanFolder(root, index)
	if again, _, _ := engine.ScanFolder(root, index); again.Hashed != 0 {
		t.Errorf("unchanged old files were hashed again (%d)", again.Hashed)
	}
	write(t, root, "Audio/kick.wav", "KICK") // same size, new time
	scan, index, _ = engine.ScanFolder(root, index)
	if scan.Hashed != 1 || scan.Manifest.Find("Audio/kick.wav").SHA256 != sha("KICK") {
		t.Errorf("the change was not picked up (hashed %d)", scan.Hashed)
	}
	// The index survives a save and load.
	file := filepath.Join(t.TempDir(), "index.json")
	if err := index.Save(file); err != nil || !reflect.DeepEqual(engine.LoadIndex(file), index) {
		t.Errorf("index round trip failed: %v", err)
	}

	write(t, root, "My Song.flp~", "half written")
	if _, _, err := engine.ScanFolder(root, index); !errors.Is(err, engine.ErrSaveInProgress) {
		t.Errorf("save in progress not detected: %v", err)
	}
	remove(t, root, "My Song.flp~")
	if _, _, err := engine.ScanFolder(filepath.Join(root, "nope"), nil); !errors.Is(err, engine.ErrFolderMissing) {
		t.Errorf("missing folder: %v", err)
	}
	if os.Symlink(filepath.Join(root, "My Song.flp"), filepath.Join(root, "link.flp")) == nil {
		if scan, _, err := engine.ScanFolder(root, index); err != nil || scan.Manifest.Find("link.flp") != nil {
			t.Error("a symbolic link was followed")
		}
	}
}

// --- basic sync ----------------------------------------------------------------

func TestPushThenClone(t *testing.T) {
	w := newWorld(t)
	write(t, w.a, "My Song.flp", "v1")
	write(t, w.a, "Audio/kick.wav", "kick")
	write(t, w.a, "Audio/same.wav", "kick") // identical content: stored once

	res, err := w.ann.Push(engine.PushOptions{})
	if err != nil || !res.Pushed || res.Uploaded != 2 || len(w.history()) != 1 {
		t.Fatalf("push: %v %+v", err, res)
	}
	if res, _ := w.ann.Push(engine.PushOptions{}); res.Pushed || len(w.history()) != 1 {
		t.Error("an unchanged folder produced a new version")
	}
	res, err = w.bob.Pull()
	if err != nil || !res.Pulled || res.Downloaded != 2 || w.bob.BaseID() != w.headID() {
		t.Fatalf("pull: %v %+v", err, res)
	}
	w.same()
	if st, err := w.bob.Status(); err != nil || !st.Local.Empty() || st.RemoteAhead || !st.RemoteReachable {
		t.Errorf("status: %v %+v", err, st)
	}
}

func TestEditsRenamesDeletes(t *testing.T) {
	w := newWorld(t)
	write(t, w.a, "My Song.flp", "v1")
	write(t, w.a, "Audio/kick.wav", "kick")
	write(t, w.a, "Audio/snare.wav", "snare")
	write(t, w.a, "Audio/hat.wav", "hat")
	w.sync(w.ann)
	w.sync(w.bob)

	// Edit: only the changed file travels.
	write(t, w.b, "My Song.flp", "v2")
	w.rb.uploads, w.ra.downloads = 0, 0
	w.sync(w.bob)
	w.sync(w.ann)
	if w.rb.uploads != 1 || w.ra.downloads != 1 {
		t.Errorf("edit moved %d uploads, %d downloads", w.rb.uploads, w.ra.downloads)
	}
	w.same()

	// Rename and move: same content, so nothing is uploaded or downloaded.
	os.MkdirAll(filepath.Join(w.a, "Audio", "Drums"), 0o755)
	os.Rename(filepath.Join(w.a, "Audio", "kick.wav"), filepath.Join(w.a, "Audio", "Drums", "kick 01.wav"))
	w.ra.uploads, w.rb.downloads = 0, 0
	w.sync(w.ann)
	w.sync(w.bob)
	if w.ra.uploads != 0 || w.rb.downloads != 0 {
		t.Errorf("rename moved %d uploads, %d downloads", w.ra.uploads, w.rb.downloads)
	}
	w.same()
	if read(w.b, "Audio/Drums/kick 01.wav") != "kick" || read(w.b, "Audio/kick.wav") != "<missing>" {
		t.Error("the rename did not arrive")
	}

	// Delete: the file leaves the partner's folder and lands in the trash.
	remove(t, w.b, "Audio/snare.wav")
	remove(t, w.b, "Audio/Drums/kick 01.wav")
	w.sync(w.bob)
	w.sync(w.ann)
	w.same()
	if _, err := os.Stat(filepath.Join(w.a, "Audio", "Drums")); err == nil {
		t.Error("the emptied folder was left behind")
	}
	found := false
	filepath.WalkDir(filepath.Join(w.a, ".henosis", "trash"), func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.Name() == "snare.wav" {
			data, _ := os.ReadFile(path)
			found = string(data) == "snare"
		}
		return nil
	})
	if !found {
		t.Error("the deleted file is not in the trash")
	}
	head, _ := w.direct.Manifest(w.headID())
	if !head.Equal(w.ann.Base()) || !head.Equal(w.bob.Base()) {
		t.Error("the bases do not match the head")
	}
}

func TestStateSurvivesRestart(t *testing.T) {
	w := newWorld(t)
	write(t, w.a, "My Song.flp", "v1")
	w.sync(w.ann)
	base := w.ann.BaseID()
	w.reopenAnn()
	if w.ann.BaseID() != base || len(w.history()) != 1 {
		t.Fatal("the base was not reloaded")
	}
	w.sync(w.ann)
	write(t, w.a, "My Song.flp", "v2")
	w.reopenAnn()
	if d, _ := w.ann.LocalChanges(); len(d.Changed) != 1 {
		t.Errorf("local changes after restart: %+v", d)
	}
	w.sync(w.ann)
	if len(w.history()) != 2 {
		t.Errorf("history has %d versions", len(w.history()))
	}
}

// --- conflicts -----------------------------------------------------------------

func TestConflictKeepsBothVersions(t *testing.T) {
	w := newWorld(t)
	write(t, w.a, "My Song.flp", "base")
	w.sync(w.ann)
	w.sync(w.bob)

	write(t, w.a, "My Song.flp", "ann's version")
	write(t, w.b, "My Song.flp", "bob's version")
	w.sync(w.ann)

	before := w.headID()
	_, err := w.bob.Push(engine.PushOptions{})
	wantKind(t, err, engine.KindHeadMoved)
	if w.headID() != before {
		t.Fatal("a refused push changed the head")
	}
	res := w.sync(w.bob)
	if !res.Pulled || !res.Pushed || !reflect.DeepEqual(res.ConflictCopies, []string{conflictCopy}) {
		t.Fatalf("sync result: %+v", res)
	}
	if read(w.b, "My Song.flp") != "ann's version" || read(w.b, conflictCopy) != "bob's version" {
		t.Fatal("the conflict did not keep both versions")
	}
	if h := w.history(); len(h) != 3 || !h[0].IsConflict || h[1].IsConflict {
		t.Errorf("the conflict commit is not flagged: %+v", h)
	}
	w.sync(w.ann)
	w.same()

	// A second conflict on the same day gets a counter.
	write(t, w.a, "My Song.flp", "ann again")
	write(t, w.b, "My Song.flp", "bob again")
	w.sync(w.ann)
	w.sync(w.bob)
	if read(w.b, "My Song (conflict, bob, 2026-11-15 2).flp") != "bob again" || read(w.b, conflictCopy) != "bob's version" ||
		read(w.b, "My Song.flp") != "ann again" {
		t.Error("the second conflict went wrong")
	}
}

func TestMergeWithoutConflict(t *testing.T) {
	w := newWorld(t)
	write(t, w.a, "My Song.flp", "base")
	write(t, w.a, "Audio/kick.wav", "kick")
	write(t, w.a, "Audio/old.wav", "old")
	w.sync(w.ann)
	w.sync(w.bob)

	write(t, w.a, "My Song.flp", "ann edit")
	remove(t, w.a, "Audio/old.wav")
	write(t, w.b, "Audio/kick.wav", "bob kick")
	write(t, w.b, "Audio/new.wav", "bob new")
	w.sync(w.ann)
	if res := w.sync(w.bob); len(res.ConflictCopies) != 0 {
		t.Fatalf("unexpected conflict copies: %v", res.ConflictCopies)
	}
	w.sync(w.ann)
	w.same()
	want := map[string]string{"My Song.flp": "ann edit", "Audio/kick.wav": "bob kick", "Audio/new.wav": "bob new"}
	if got := tree(w.a); !reflect.DeepEqual(got, want) {
		t.Errorf("merged tree = %v", got)
	}

	// The same edit on both sides is not a conflict.
	write(t, w.a, "My Song.flp", "same edit")
	write(t, w.b, "My Song.flp", "same edit")
	w.sync(w.ann)
	w.sync(w.bob)
	w.same()
	if len(tree(w.b)) != 3 {
		t.Error("an identical edit produced a conflict copy")
	}
}

func TestDeleteAgainstEdit(t *testing.T) {
	w := newWorld(t)
	for _, name := range []string{"one.wav", "two.wav", "keep1.wav", "keep2.wav"} {
		write(t, w.a, name, name)
	}
	w.sync(w.ann)
	w.sync(w.bob)

	write(t, w.a, "one.wav", "one edited by ann")
	remove(t, w.a, "two.wav")
	remove(t, w.b, "one.wav")
	write(t, w.b, "two.wav", "two edited by bob")
	w.sync(w.ann)
	w.sync(w.bob)
	// Deleted here, edited there: the edit wins and the file comes back.
	if read(w.b, "one.wav") != "one edited by ann" {
		t.Error("the edited file did not come back")
	}
	// Edited here, deleted there: the edit is kept under a conflict name.
	if read(w.b, "two.wav") != "<missing>" || read(w.b, "two (conflict, bob, 2026-11-15).wav") != "two edited by bob" {
		t.Error("the local edit was not kept as a conflict copy")
	}
	w.sync(w.ann)
	w.same()
}

func TestFileAndFolderSwap(t *testing.T) {
	w := newWorld(t)
	write(t, w.a, "x/inner.wav", "inner")
	write(t, w.a, "y", "plain file")
	for _, name := range []string{"keep1.wav", "keep2.wav", "keep3.wav"} {
		write(t, w.a, name, name)
	}
	w.sync(w.ann)
	w.sync(w.bob)

	remove(t, w.a, "x")
	write(t, w.a, "x", "now a file")
	remove(t, w.a, "y")
	write(t, w.a, "y/deep/inner.wav", "now in a folder")
	w.sync(w.ann)
	if res := w.sync(w.bob); len(res.ConflictCopies) != 0 {
		t.Fatalf("unexpected conflict copies: %v", res.ConflictCopies)
	}
	w.same()
}

// --- failures --------------------------------------------------------------------

func TestInterruptedUpload(t *testing.T) {
	w := newWorld(t)
	write(t, w.a, "My Song.flp", "v1")
	w.sync(w.ann)
	before := w.headID()

	write(t, w.a, "a.wav", "aaa")
	write(t, w.a, "b.wav", "bbb")
	write(t, w.a, "c.wav", "ccc")
	w.ra.failUploadAt = 2
	_, err := w.ann.Push(engine.PushOptions{})
	wantKind(t, err, engine.KindUpload)
	if w.headID() != before || w.ann.BaseID() != before {
		t.Fatal("a failed upload moved the head or the base")
	}
	w.ra.uploads = 0
	if res, err := w.ann.Push(engine.PushOptions{}); err != nil || !res.Pushed || w.ra.uploads != 2 {
		t.Fatalf("retry: %v %+v uploads=%d", err, res, w.ra.uploads) // the finished blob is not sent again
	}
	w.sync(w.bob)
	w.same()
}

func TestInterruptedDownload(t *testing.T) {
	w := newWorld(t)
	write(t, w.a, "My Song.flp", "v1")
	write(t, w.a, "a.wav", "aaa")
	w.sync(w.ann)
	w.sync(w.bob)
	write(t, w.a, "My Song.flp", "v2")
	write(t, w.a, "a.wav", "AAA2")
	write(t, w.a, "b.wav", "bbb")
	w.sync(w.ann)

	before, base := tree(w.b), w.bob.BaseID()
	w.rb.failDownloadAt = 2
	_, err := w.bob.Pull()
	wantKind(t, err, engine.KindDownload)
	if !reflect.DeepEqual(tree(w.b), before) || w.bob.BaseID() != base {
		t.Fatal("a failed download changed the folder or the base")
	}
	if _, err := w.bob.Pull(); err != nil {
		t.Fatal(err)
	}
	w.same()
}

func TestHalfAppliedPullCompletes(t *testing.T) {
	w := newWorld(t)
	write(t, w.a, "My Song.flp", "v1")
	write(t, w.a, "a.wav", "aaa")
	write(t, w.a, "gone.wav", "gone")
	w.sync(w.ann)
	w.sync(w.bob)
	write(t, w.a, "My Song.flp", "v2")
	write(t, w.a, "a.wav", "AAA2")
	remove(t, w.a, "gone.wav")
	w.sync(w.ann)

	// As if the app had been killed after applying one file of the pull.
	write(t, w.b, "a.wav", "AAA2")
	res, err := w.bob.Pull()
	if err != nil || len(res.ConflictCopies) != 0 {
		t.Fatalf("pull: %v %+v", err, res)
	}
	w.same()
}

func TestCorruptBlobIsRejected(t *testing.T) {
	w := newWorld(t)
	write(t, w.a, "My Song.flp", "v1")
	w.sync(w.ann)
	w.sync(w.bob)
	write(t, w.a, "My Song.flp", "v2 good bytes")
	w.sync(w.ann)

	os.WriteFile(filepath.Join(w.blobFolder, sha("v2 good bytes")), []byte("v2 BAD bytes!"), 0o644)
	_, err := w.bob.Pull()
	wantKind(t, err, engine.KindBlobCorrupt)
	if read(w.b, "My Song.flp") != "v1" {
		t.Error("a corrupt download reached the project folder")
	}
}

func TestLostCommitAnswer(t *testing.T) {
	w := newWorld(t)
	write(t, w.a, "My Song.flp", "v1")
	w.ra.loseNextCommitAnswer = true
	_, err := w.ann.Push(engine.PushOptions{})
	wantKind(t, err, engine.KindRemote)
	if len(w.history()) != 1 {
		t.Fatal("the commit should have landed on the server")
	}
	// The retry is answered with the existing snapshot, not a second one.
	if res, err := w.ann.Push(engine.PushOptions{}); err != nil || !res.Pushed || len(w.history()) != 1 || w.ann.BaseID() != w.headID() {
		t.Fatalf("retry: %v %+v", err, res)
	}
}

// --- safety rules ------------------------------------------------------------------

func TestMissingFolderDeletesNothing(t *testing.T) {
	w := newWorld(t)
	write(t, w.a, "My Song.flp", "v1")
	write(t, w.a, "a.wav", "aaa")
	w.sync(w.ann)
	before := w.headID()
	os.Rename(w.a, w.a+" moved away")
	_, err := w.ann.Push(engine.PushOptions{})
	wantKind(t, err, engine.KindFolderMissing)
	_, err = w.ann.Sync(engine.PushOptions{})
	wantKind(t, err, engine.KindFolderMissing)
	if w.headID() != before || len(w.history()) != 1 {
		t.Error("a missing folder changed the remote")
	}
}

func TestMassDeleteNeedsAYes(t *testing.T) {
	w := newWorld(t)
	for _, name := range []string{"a.wav", "b.wav", "c.wav", "d.wav"} {
		write(t, w.a, name, name)
	}
	w.sync(w.ann)
	remove(t, w.a, "a.wav")
	remove(t, w.a, "b.wav")
	w.sync(w.ann) // exactly half: allowed

	remove(t, w.a, "c.wav")
	remove(t, w.a, "d.wav")
	before := w.headID()
	_, err := w.ann.Push(engine.PushOptions{})
	wantKind(t, err, engine.KindMassDelete)
	if w.headID() != before {
		t.Fatal("a refused push changed the head")
	}
	if _, err := w.ann.Push(engine.PushOptions{AllowMassDelete: true}); err != nil || w.headID() == before {
		t.Fatalf("confirmed push: %v", err)
	}

	// Renaming or moving every file is not a deletion: the content is all there.
	for _, name := range []string{"p.wav", "q.wav", "r.wav"} {
		write(t, w.a, "Audio/"+name, name)
	}
	w.sync(w.ann)
	os.Rename(filepath.Join(w.a, "Audio"), filepath.Join(w.a, "Samples"))
	if res, err := w.ann.Push(engine.PushOptions{}); err != nil || !res.Pushed || res.Uploaded != 0 {
		t.Fatalf("folder rename: %v %+v", err, res)
	}
	w.sync(w.bob)
	w.same()
}

func TestIgnoredFilesStayLocal(t *testing.T) {
	w := newWorld(t)
	write(t, w.a, "My Song.flp", "v1")
	write(t, w.a, "Backup/My Song (overwritten at 19h26).flp", "older")
	write(t, w.a, "Thumbs.db", "junk")
	w.sync(w.ann)
	w.sync(w.bob)
	if len(tree(w.b)) != 1 || read(w.b, "Backup/My Song (overwritten at 19h26).flp") != "<missing>" {
		t.Fatal("ignored files were synced")
	}
	write(t, w.b, "Backup/bob.flp", "bob's backup")
	write(t, w.a, "My Song.flp", "v2")
	w.sync(w.ann)
	w.sync(w.bob)
	if read(w.b, "Backup/bob.flp") != "bob's backup" {
		t.Error("a pull touched an ignored file")
	}

	// While FL Studio's temporary file exists, nothing is pushed.
	write(t, w.a, "My Song.flp~", "half")
	remove(t, w.a, "My Song.flp")
	before := w.headID()
	_, err := w.ann.Push(engine.PushOptions{})
	wantKind(t, err, engine.KindBusy)
	if w.headID() != before {
		t.Fatal("a push happened in the middle of a save")
	}
	os.Rename(filepath.Join(w.a, "My Song.flp~"), filepath.Join(w.a, "My Song.flp"))
	w.sync(w.ann)
	w.sync(w.bob)
	if read(w.b, "My Song.flp") != "half" {
		t.Error("the finished save did not sync")
	}
}

func TestLostBaseIsSafe(t *testing.T) {
	w := newWorld(t)
	write(t, w.a, "My Song.flp", "v1")
	write(t, w.a, "a.wav", "aaa")
	w.sync(w.ann)
	w.sync(w.bob)
	write(t, w.b, "a.wav", "bob changed this")
	remove(t, w.b, ".henosis")
	w.reopenBob()
	if w.bob.BaseID() != "" {
		t.Fatal("the base should be unknown")
	}
	w.sync(w.bob)
	if read(w.b, "My Song.flp") != "v1" || read(w.b, "a.wav") != "aaa" ||
		read(w.b, "a (conflict, bob, 2026-11-15).wav") != "bob changed this" {
		t.Errorf("tree after losing the metadata: %v", tree(w.b))
	}
}

func TestSomeoneElseHoldsTheLock(t *testing.T) {
	w := newWorld(t)
	write(t, w.a, "My Song.flp", "v1")
	w.sync(w.ann)
	w.sync(w.bob)

	// Ann starts a session and keeps the lock.
	if err := w.direct.AcquireLock(w.headID()); err != nil {
		t.Fatal(err)
	}
	write(t, w.b, "My Song.flp", "bob edits anyway")
	before := w.headID()
	_, err := w.bob.Sync(engine.PushOptions{})
	wantKind(t, err, engine.KindLockNotHeld)
	if w.headID() != before || read(w.b, "My Song.flp") != "bob edits anyway" {
		t.Fatal("the refused push changed something")
	}
	// Once the lock is free, the draft goes through untouched.
	w.direct.ReleaseLock()
	w.sync(w.bob)
	w.sync(w.ann)
	w.same()
	if read(w.a, "My Song.flp") != "bob edits anyway" {
		t.Error("the draft was not pushed")
	}
}

// --- restore -----------------------------------------------------------------------

func TestRestore(t *testing.T) {
	w := newWorld(t)
	write(t, w.a, "My Song.flp", "v1")
	write(t, w.a, "a.wav", "aaa")
	w.sync(w.ann)
	v1 := w.headID()
	write(t, w.a, "My Song.flp", "v2")
	write(t, w.a, "b.wav", "bbb")
	remove(t, w.a, "a.wav")
	w.sync(w.ann)
	w.sync(w.bob)

	// Uncommitted work at the time of the restore is committed first.
	write(t, w.a, "My Song.flp", "v3 not yet synced")
	if _, err := w.ann.Restore(v1); err != nil {
		t.Fatal(err)
	}
	h := w.history()
	if len(h) != 4 || !strings.HasPrefix(h[0].Message, "Restored version from ") || h[0].ParentID != h[1].ID {
		t.Fatalf("history after restore: %+v", h)
	}
	if got := tree(w.a); !reflect.DeepEqual(got, map[string]string{"My Song.flp": "v1", "a.wav": "aaa"}) {
		t.Errorf("tree after restore: %v", got)
	}
	if m, _ := w.direct.Manifest(h[1].ID); m.Find("My Song.flp").SHA256 != sha("v3 not yet synced") {
		t.Error("the unsynced work is not in the history")
	}
	w.sync(w.bob)
	w.same()
	_, err := w.ann.Restore("no-such-snapshot")
	wantKind(t, err, engine.KindNotFound)
	if _, err := w.ann.Restore(w.headID()); err != nil || len(w.history()) != 4 {
		t.Errorf("restoring the head: %v, %d versions", err, len(w.history()))
	}
}

// --- random operations ---------------------------------------------------------------

func TestRandomOperations(t *testing.T) {
	names := []string{"My Song.flp", "notes.txt", "Audio/kick.wav", "Audio/snare.wav", "Audio/vox take.wav",
		"Audio/Drums/hat.wav", "Renders/final.mp3", "Renders/demo.mp3"}
	runs := 40
	if testing.Short() {
		runs = 8
	}
	if env := os.Getenv("HENOSIS_RANDOM_RUNS"); env != "" {
		fmt.Sscan(env, &runs)
	}
	syncs, conflicts := 0, 0
	for seed := 1; seed <= runs; seed++ {
		rng := rand.New(rand.NewSource(int64(seed)))
		w := newWorld(t)
		counter := 0

		syncSide := func(isAnn bool) {
			folder, e := w.b, w.bob
			if isAnn {
				folder, e = w.a, w.ann
			}
			before := map[string]bool{}
			for _, content := range tree(folder) {
				before[sha(content)] = true
			}
			if rng.Intn(8) == 0 { // app restart
				if isAnn {
					w.reopenAnn()
					e = w.ann
				} else {
					w.reopenBob()
					e = w.bob
				}
			}
			res, err := e.Sync(engine.PushOptions{AllowMassDelete: true})
			if err != nil {
				t.Fatalf("seed %d: sync failed: %v", seed, err)
			}
			syncs++
			conflicts += len(res.ConflictCopies)
			// Nothing a side had on disk before a sync may be lost: every
			// content is either still there or stored on the server.
			for hash := range before {
				if _, err := os.Stat(filepath.Join(w.blobFolder, hash)); err != nil {
					t.Fatalf("seed %d: content %.8s was lost", seed, hash)
				}
			}
			// After a sync the folder is exactly the version it reports.
			if res.SnapshotID == "" {
				if len(tree(folder)) != 0 {
					t.Fatalf("seed %d: files but no version", seed)
				}
				return
			}
			head, err := w.direct.Manifest(res.SnapshotID)
			if err != nil {
				t.Fatalf("seed %d: %v", seed, err)
			}
			got := tree(folder)
			if len(got) != len(head.Files) {
				t.Fatalf("seed %d: %d files on disk, %d in the manifest", seed, len(got), len(head.Files))
			}
			for _, entry := range head.Files {
				if sha(got[entry.Path]) != entry.SHA256 {
					t.Fatalf("seed %d: %s differs from its version", seed, entry.Path)
				}
			}
		}

		for step := 0; step < 60; step++ {
			isAnn := rng.Intn(2) == 0
			folder := w.b
			if isAnn {
				folder = w.a
			}
			current := keys(tree(folder))
			switch op := rng.Intn(10); {
			case op < 5 || len(current) == 0:
				content := fmt.Sprintf("unique %d", counter)
				counter++
				if rng.Intn(4) == 0 {
					content = fmt.Sprintf("shared %d", rng.Intn(3)) // content both sides may produce
				}
				write(t, folder, names[rng.Intn(len(names))], content)
			case op < 7:
				remove(t, folder, current[rng.Intn(len(current))])
			case op < 9:
				target := filepath.Join(folder, filepath.FromSlash(names[rng.Intn(len(names))]))
				if _, err := os.Stat(target); err != nil {
					os.MkdirAll(filepath.Dir(target), 0o755)
					os.Rename(filepath.Join(folder, filepath.FromSlash(current[rng.Intn(len(current))])), target)
				}
			}
			if rng.Intn(5) < 2 {
				syncSide(isAnn)
			}
		}
		// Settle: after each side has synced twice, both folders are identical.
		syncSide(true)
		syncSide(false)
		syncSide(true)
		syncSide(false)
		w.same()
		if w.ann.BaseID() != w.bob.BaseID() {
			t.Fatalf("seed %d: the sides ended on different versions", seed)
		}
	}
	t.Logf("%d runs, %d syncs, %d conflict copies", runs, syncs, conflicts)
	if conflicts == 0 {
		t.Error("the runs never exercised the conflict path")
	}
}

func TestDefaultMessages(t *testing.T) {
	w := newWorld(t)
	write(t, w.a, "song.flp", "v1")
	w.sync(w.ann)
	write(t, w.a, "song.flp", "v2")
	write(t, w.a, "Audio/kick.wav", "kick")
	write(t, w.a, "Audio/snare.wav", "snare")
	w.sync(w.ann)
	if _, err := w.ann.Push(engine.PushOptions{Message: "my own words"}); err != nil {
		t.Fatal(err)
	}
	write(t, w.a, "song.flp", "v3")
	if _, err := w.ann.Push(engine.PushOptions{Message: "my own words"}); err != nil {
		t.Fatal(err)
	}
	h := w.history()
	got := []string{h[2].Message, h[1].Message, h[0].Message}
	want := []string{"Added song.flp", "Added 2 files, changed 1", "my own words"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("messages: %q, want %q", got, want)
	}
}

func TestBeforeApplyCanStopAPull(t *testing.T) {
	w := newWorld(t)
	write(t, w.a, "song.flp", "v1")
	w.sync(w.ann)
	w.sync(w.bob)
	write(t, w.a, "song.flp", "v2")
	write(t, w.a, "new.wav", "new")
	w.sync(w.ann)

	calls := 0
	w.bob.BeforeApply = func() error {
		calls++
		return errors.New("the project was just opened in FL Studio")
	}
	before := w.bob.BaseID()
	_, err := w.bob.Pull()
	wantKind(t, err, engine.KindBusy)
	if calls != 1 || w.bob.BaseID() != before || read(w.b, "song.flp") != "v1" || read(w.b, "new.wav") != "<missing>" {
		t.Fatalf("a stopped pull changed something: calls=%d %v", calls, tree(w.b))
	}
	// Once the check passes, the same pull completes.
	w.bob.BeforeApply = func() error { calls++; return nil }
	if res, err := w.bob.Pull(); err != nil || !res.Pulled {
		t.Fatalf("pull: %v %+v", err, res)
	}
	w.same()
	// Nothing to apply means nothing to check.
	if _, err := w.bob.Pull(); err != nil || calls != 2 {
		t.Fatalf("an empty pull called the check: calls=%d err=%v", calls, err)
	}
}

func TestCleanTrash(t *testing.T) {
	w := newWorld(t)
	write(t, w.a, "song.flp", "v1")
	write(t, w.a, "old.wav", "old")
	w.sync(w.ann)
	w.sync(w.bob)
	remove(t, w.a, "old.wav")
	w.sync(w.ann)
	w.sync(w.bob) // bob's old.wav moves to the trash
	trash := filepath.Join(w.b, ".henosis", "trash")
	entries, _ := os.ReadDir(trash)
	if len(entries) != 1 {
		t.Fatalf("trash folders: %d", len(entries))
	}
	if n := w.bob.CleanTrash(time.Hour); n != 0 {
		t.Fatalf("fresh trash was deleted: %d", n)
	}
	old := time.Now().Add(-48 * time.Hour)
	os.Chtimes(filepath.Join(trash, entries[0].Name()), old, old)
	if n := w.bob.CleanTrash(24 * time.Hour); n != 1 {
		t.Fatalf("old trash was kept: %d", n)
	}
	if entries, _ := os.ReadDir(trash); len(entries) != 0 {
		t.Fatal("the trash folder still has entries")
	}
}

// A folder synced with one project, then used for another (a project of the
// same name in a later session), must not keep the first project's base:
// pull would say "nothing new" and push "someone was first", forever.
func TestFolderReusedForAnotherProject(t *testing.T) {
	for _, otherHasVersions := range []bool{false, true} {
		t.Run(fmt.Sprintf("other project has versions: %v", otherHasVersions), func(t *testing.T) {
			dir := t.TempDir()
			st, err := store.Open(filepath.Join(dir, "server"))
			if err != nil {
				t.Fatal(err)
			}
			st.Now = time.Now
			ann, _, _ := st.AddUser("ann")
			old, _ := st.CreateProject(ann.ID, "get hi get buck")
			folder := filepath.Join(dir, "get hi get buck")
			write(t, folder, "beat.flp", "v1")
			first := engine.Open(folder, &store.ProjectRemote{Store: st, UserID: ann.ID, ProjectID: old.ID, Device: "pc"}, "ann")
			if _, err := first.Sync(engine.PushOptions{}); err != nil {
				t.Fatal(err)
			}

			// A new session: same folder, different project.
			fresh, _ := st.CreateProject(ann.ID, "get hi get buck")
			remote := &store.ProjectRemote{Store: st, UserID: ann.ID, ProjectID: fresh.ID, Device: "pc"}
			if otherHasVersions {
				elsewhere := filepath.Join(dir, "elsewhere")
				write(t, elsewhere, "beat.flp", "their v1")
				write(t, elsewhere, "kick.wav", "kick")
				if _, err := engine.Open(elsewhere, remote, "ann").Sync(engine.PushOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			write(t, folder, "beat.flp", "v2 here")
			second := engine.Open(folder, remote, "ann")
			if _, err := second.Sync(engine.PushOptions{}); err != nil {
				t.Fatalf("sync in a reused folder: %v", err)
			}
			p, _ := st.Project(ann.ID, fresh.ID)
			if p.Head == "" || second.BaseID() != p.Head {
				t.Fatalf("not synced: head %q, base %q", p.Head, second.BaseID())
			}
			got := tree(folder)
			if !otherHasVersions && got["beat.flp"] != "v2 here" {
				t.Fatalf("the local file was changed: %v", got)
			}
			if otherHasVersions {
				// Both versions are kept (as with any conflict), and their
				// other file arrived.
				contents := map[string]bool{}
				for _, content := range got {
					contents[content] = true
				}
				if !contents["v2 here"] || !contents["their v1"] || got["kick.wav"] != "kick" || len(got) != 3 {
					t.Fatalf("after the merge: %v", got)
				}
			}
		})
	}
}
