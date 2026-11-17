package engine

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"henosis/core"
)

// Kind classifies an engine error so callers can react to it.
type Kind int

const (
	KindNone            Kind = iota
	KindFolderMissing        // the project folder is gone; nothing was changed anywhere
	KindBusy                 // files are still being written; try again shortly
	KindScan                 // the folder cannot be turned into a valid manifest
	KindRemote               // the remote failed or is unreachable
	KindHeadMoved            // someone committed first; pull, then push again
	KindLockNotHeld          // a shared project, and this device does not hold the lock
	KindUpload               // an upload failed; the head is untouched
	KindDownload             // a download failed; the folder is untouched
	KindBlobCorrupt          // downloaded bytes do not match their hash
	KindMassDelete           // the push would delete more than half the files' content
	KindInvalidManifest      // the remote refused the manifest
	KindNotFound             // no such snapshot, or its content has expired
	KindLocalIO              // a local file operation failed
)

// Error is an engine failure with its Kind.
type Error struct {
	Kind Kind
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

func fail(kind Kind, format string, args ...any) error {
	return &Error{Kind: kind, Msg: fmt.Sprintf(format, args...)}
}

// KindOf returns the Kind of an engine error, KindNone for nil.
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	if err != nil {
		return KindRemote
	}
	return KindNone
}

// Result says what an operation did.
type Result struct {
	Pushed         bool     // a new snapshot was committed
	Pulled         bool     // the folder was brought to a newer head
	Uploaded       int      // blobs uploaded
	Downloaded     int      // blobs downloaded
	ConflictCopies []string // paths created by this operation
	SnapshotID     string   // the base after the operation
}

// PushOptions tunes a push.
type PushOptions struct {
	AllowMassDelete bool
	Message         string
	FLVersion       string
	Plugins         []string
}

// Status is a snapshot of how the folder relates to its base and the remote.
type Status struct {
	Local           core.Diff // disk compared with the base
	BaseID          string
	HeadID          string // "" when the remote has no snapshot or is unreachable
	RemoteReachable bool
	RemoteAhead     bool
	ConflictPending bool
}

type state struct {
	BaseSnapshot    string `json:"base_snapshot"`
	ConflictPending bool   `json:"conflict_pending"`
}

// Engine syncs one project folder. Local state lives in <folder>/.henosis/:
// base.json (the manifest of the last synced version), state.json (its id),
// index.json (the scanner's cache), tmp/ and trash/.
type Engine struct {
	Folder string
	Remote Remote
	User   string
	// Today gives the date used in conflict copy names. Tests replace it.
	Today func() string
	// BeforeApply, when set, is called after a pull has downloaded everything
	// and right before it changes the first project file. Returning an error
	// stops the pull with nothing changed; the caller can try again later.
	BeforeApply func() error

	meta  string
	base  core.Manifest
	state state
	index Index
}

// Open loads the engine state for a project folder.
func Open(folder string, remote Remote, user string) *Engine {
	e := &Engine{
		Folder: folder, Remote: remote, User: user, meta: filepath.Join(folder, ".henosis"),
		Today: func() string { return time.Now().UTC().Format("2006-01-02") },
	}
	if data, err := os.ReadFile(filepath.Join(e.meta, "state.json")); err == nil {
		_ = json.Unmarshal(data, &e.state)
	}
	if e.state.BaseSnapshot != "" {
		data, err := os.ReadFile(filepath.Join(e.meta, "base.json"))
		base, parseErr := core.ParseManifest(data)
		if err != nil || parseErr != nil {
			// Without its manifest the base is unknown. An empty base is safe:
			// every local file then counts as a local change.
			e.state.BaseSnapshot = ""
		} else {
			e.base = base
		}
	}
	e.index = LoadIndex(filepath.Join(e.meta, "index.json"))
	// A folder from an older version, or one that lost the mark, is hidden again.
	if info, err := os.Stat(e.meta); err == nil && info.IsDir() {
		hideFolder(e.meta)
	}
	return e
}

// BaseID is the snapshot this folder was last synced to.
func (e *Engine) BaseID() string { return e.state.BaseSnapshot }

// Base is the manifest of that snapshot.
func (e *Engine) Base() core.Manifest { return e.base }

// ensureMeta creates <folder>/.henosis if it is not there yet and keeps it
// hidden, so it does not clutter the project folder.
func (e *Engine) ensureMeta() {
	if _, err := os.Stat(e.meta); err == nil {
		return
	}
	if os.MkdirAll(e.meta, 0o755) == nil {
		hideFolder(e.meta)
	}
}

func (e *Engine) saveState() error {
	e.ensureMeta()
	// The manifest goes first: the state must never name a base whose
	// manifest is not on disk.
	if err := writeFileAtomic(filepath.Join(e.meta, "base.json"), e.base.JSON()); err != nil {
		return err
	}
	data, _ := json.Marshal(e.state)
	return writeFileAtomic(filepath.Join(e.meta, "state.json"), data)
}

func (e *Engine) scan() (core.Manifest, error) {
	scan, fresh, err := ScanFolder(e.Folder, e.index)
	switch {
	case errors.Is(err, ErrFolderMissing):
		return core.Manifest{}, fail(KindFolderMissing, "the project folder is missing: %s", e.Folder)
	case errors.Is(err, ErrSaveInProgress), errors.Is(err, ErrUnstable):
		return core.Manifest{}, fail(KindBusy, "%v", err)
	case err != nil:
		return core.Manifest{}, fail(KindScan, "%v", err)
	}
	e.index = fresh
	e.ensureMeta()
	_ = e.index.Save(filepath.Join(e.meta, "index.json")) // a cache; failing to save it is harmless
	return scan.Manifest, nil
}

// LocalChanges scans the folder and reports how it differs from the base.
func (e *Engine) LocalChanges() (core.Diff, error) {
	local, err := e.scan()
	if err != nil {
		return core.Diff{}, err
	}
	return core.DiffManifests(e.base, local), nil
}

func (e *Engine) upload(hashes []string, local core.Manifest, res *Result) error {
	pathByHash := map[string]string{}
	for _, entry := range local.Files {
		pathByHash[entry.SHA256] = entry.Path
	}
	for _, hash := range hashes {
		path, ok := pathByHash[hash]
		if !ok {
			return fail(KindUpload, "the remote asked for content this folder does not have: %s", hash)
		}
		if err := e.Remote.UploadBlob(hash, filepath.Join(e.Folder, filepath.FromSlash(path))); err != nil {
			return fail(KindUpload, "%v", err)
		}
		res.Uploaded++
	}
	return nil
}

// Push commits the folder as a new snapshot if it differs from the base.
func (e *Engine) Push(opts PushOptions) (Result, error) {
	local, err := e.scan()
	if err != nil {
		return Result{}, err
	}
	res := Result{SnapshotID: e.state.BaseSnapshot}
	if local.Equal(e.base) {
		return res, nil
	}

	// Guard against a folder emptied by accident. A file counts as deleted
	// only when its content is gone too, so renaming or moving files, however
	// many, never trips this.
	localHashes := map[string]bool{}
	for _, entry := range local.Files {
		localHashes[entry.SHA256] = true
	}
	deleted := 0
	for _, entry := range e.base.Files {
		if local.Find(entry.Path) == nil && !localHashes[entry.SHA256] {
			deleted++
		}
	}
	if !opts.AllowMassDelete && deleted*2 > len(e.base.Files) {
		return res, fail(KindMassDelete, "this push would delete %d of %d files", deleted, len(e.base.Files))
	}

	// Only content the base does not already have can be new to the remote.
	baseHashes := map[string]bool{}
	for _, entry := range e.base.Files {
		baseHashes[entry.SHA256] = true
	}
	var candidates []string
	seen := map[string]bool{}
	for _, entry := range local.Files {
		if !baseHashes[entry.SHA256] && !seen[entry.SHA256] {
			seen[entry.SHA256] = true
			candidates = append(candidates, entry.SHA256)
		}
	}
	missing, err := e.Remote.MissingBlobs(candidates)
	if err != nil {
		return res, fail(KindRemote, "%v", err)
	}
	if err := e.upload(missing, local, &res); err != nil {
		return res, err
	}

	message := opts.Message
	if message == "" {
		message = core.DiffManifests(e.base, local).Describe()
	}
	req := core.CommitRequest{
		ParentID: e.state.BaseSnapshot, Manifest: local, Message: message,
		FLVersion: opts.FLVersion, Plugins: opts.Plugins, IsConflict: e.state.ConflictPending,
	}
	for attempt := 0; attempt < 2; attempt++ {
		commit, err := e.Remote.Commit(req)
		if err != nil {
			return res, fail(KindRemote, "%v", err)
		}
		switch commit.Status {
		case core.Committed, core.AlreadyCommitted:
			e.base = local
			e.state = state{BaseSnapshot: commit.Snapshot.ID}
			res.SnapshotID, res.Pushed = commit.Snapshot.ID, true
			if err := e.saveState(); err != nil {
				return res, fail(KindLocalIO, "the version was committed but the local state could not be saved: %v", err)
			}
			return res, nil
		case core.HeadMoved:
			return res, fail(KindHeadMoved, "someone committed a newer version first")
		case core.LockNotHeld:
			return res, fail(KindLockNotHeld, "this device does not hold the project's lock")
		case core.BlobsMissing:
			if attempt == 0 {
				if err := e.upload(commit.Missing, local, &res); err != nil {
					return res, err
				}
				continue
			}
			return res, fail(KindUpload, "the remote still reports missing content after a second upload")
		case core.InvalidManifest:
			return res, fail(KindInvalidManifest, "%s", commit.Reason)
		default:
			return res, fail(KindRemote, "unexpected commit status %q", commit.Status)
		}
	}
	return res, fail(KindRemote, "the commit did not complete")
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(to)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// removeEmptyParents removes dir and then its parents while they are empty,
// never removing stop itself.
func removeEmptyParents(dir, stop string) {
	for dir != stop && len(dir) > len(stop) {
		if os.Remove(dir) != nil { // fails when the directory is not empty
			return
		}
		dir = filepath.Dir(dir)
	}
}

func randomSuffix() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Pull brings the folder to the head using the three-way rule. Local changes
// survive as a draft or as conflict copies; nothing is discarded.
func (e *Engine) Pull() (Result, error) {
	res := Result{SnapshotID: e.state.BaseSnapshot}
	headID, err := e.Remote.HeadID()
	if err != nil {
		return res, fail(KindRemote, "%v", err)
	}
	if headID == "" || headID == e.state.BaseSnapshot {
		return res, nil
	}
	head, err := e.Remote.Manifest(headID)
	if err != nil {
		return res, fail(KindRemote, "%v", err)
	}
	local, err := e.scan()
	if err != nil {
		return res, err
	}
	plan := core.PlanMerge(e.base, local, head)
	abs := func(rel string) string { return filepath.Join(e.Folder, filepath.FromSlash(rel)) }
	needsHead := func(step core.MergeStep) bool {
		return step.Head != nil && (step.Action == core.TakeHead || step.Action == core.Conflict)
	}

	// 1. Stage every piece of content the plan needs before touching the
	// project. A source file may itself be renamed or replaced further down.
	tmp := filepath.Join(e.meta, "tmp")
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return res, fail(KindLocalIO, "cannot create %s: %v", tmp, err)
	}
	uses := map[string]int{}
	sizes := map[string]uint64{}
	for _, step := range plan.Steps {
		if needsHead(step) {
			uses[step.Head.SHA256]++
			sizes[step.Head.SHA256] = step.Head.Size
		}
	}
	localByHash := map[string]string{}
	for _, entry := range local.Files {
		localByHash[entry.SHA256] = entry.Path
	}
	needed := make([]string, 0, len(uses))
	for hash := range uses {
		needed = append(needed, hash)
	}
	sort.Strings(needed)
	for _, hash := range needed {
		staged := filepath.Join(tmp, hash+".part")
		localPath, fromLocal := localByHash[hash]
		if fromLocal {
			if err := copyFile(abs(localPath), staged); err != nil {
				return res, fail(KindBusy, "a file changed while it was being copied")
			}
		} else {
			if err := e.Remote.DownloadBlob(hash, staged); err != nil {
				return res, fail(KindDownload, "%v", err)
			}
			res.Downloaded++
		}
		got, size, err := HashFile(staged)
		if err != nil || got != hash || size != sizes[hash] {
			if fromLocal {
				return res, fail(KindBusy, "a file changed while it was being copied")
			}
			return res, fail(KindBlobCorrupt, "downloaded content does not match its hash: %s", hash)
		}
	}

	if e.BeforeApply != nil {
		if err := e.BeforeApply(); err != nil {
			_ = os.RemoveAll(tmp)
			return res, fail(KindBusy, "%v", err)
		}
	}

	// 2. Apply. Order: conflict renames, deletions, then new content, so a
	// path can change from a folder to a file and back.
	taken := map[string]bool{}
	for _, m := range []core.Manifest{local, head} {
		for _, entry := range m.Files {
			taken[core.AsciiLower(entry.Path)] = true
		}
	}
	isTaken := func(name string) bool {
		if taken[core.AsciiLower(name)] {
			return true
		}
		_, err := os.Lstat(abs(name))
		return err == nil
	}
	date := e.Today()
	for _, step := range plan.Steps {
		if step.Action != core.Conflict {
			continue
		}
		name := core.ConflictCopyName(step.Path, e.User, date, isTaken)
		if err := os.Rename(abs(step.Path), abs(name)); err != nil {
			return res, fail(KindLocalIO, "cannot rename %s: %v", step.Path, err)
		}
		taken[core.AsciiLower(name)] = true
		res.ConflictCopies = append(res.ConflictCopies, name)
	}

	trash := ""
	for _, step := range plan.Steps {
		if step.Action != core.DeleteLocal {
			continue
		}
		if trash == "" {
			trash = filepath.Join(e.meta, "trash", time.Now().UTC().Format("2006-01-02")+"-"+randomSuffix())
		}
		target := filepath.Join(trash, filepath.FromSlash(step.Path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return res, fail(KindLocalIO, "cannot create the trash folder: %v", err)
		}
		if err := os.Rename(abs(step.Path), target); err != nil {
			return res, fail(KindLocalIO, "cannot move %s to the trash: %v", step.Path, err)
		}
		removeEmptyParents(filepath.Dir(abs(step.Path)), e.Folder)
	}

	applied := 0
	for _, step := range plan.Steps {
		if !needsHead(step) {
			continue
		}
		target := abs(step.Path)
		staged := filepath.Join(tmp, step.Head.SHA256+".part")
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return res, fail(KindLocalIO, "cannot write %s: %v", step.Path, err)
		}
		source := staged
		uses[step.Head.SHA256]--
		if uses[step.Head.SHA256] > 0 {
			// More paths need this content: place a copy, keep the staged file.
			source = filepath.Join(tmp, "apply-"+strconv.Itoa(applied)+".part")
			applied++
			if err := copyFile(staged, source); err != nil {
				return res, fail(KindLocalIO, "cannot write %s: %v", step.Path, err)
			}
		}
		if err := os.Rename(source, target); err != nil { // replaces an existing file in one step
			return res, fail(KindLocalIO, "cannot write %s: %v", step.Path, err)
		}
	}
	_ = os.RemoveAll(tmp)

	// 3. Only now does the base advance. If anything above failed, the next
	// pull starts from the same base and repeats the remaining steps.
	e.base = head
	e.state.BaseSnapshot = headID
	if len(res.ConflictCopies) > 0 {
		e.state.ConflictPending = true
	}
	res.SnapshotID, res.Pulled = headID, true
	if err := e.saveState(); err != nil {
		return res, fail(KindLocalIO, "cannot save the local state: %v", err)
	}
	return res, nil
}

// CleanTrash deletes files that pulls moved to .henosis/trash more than
// maxAge ago. It returns how many trash folders were removed.
func (e *Engine) CleanTrash(maxAge time.Duration) int {
	entries, err := os.ReadDir(filepath.Join(e.meta, "trash"))
	if err != nil {
		return 0
	}
	removed := 0
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || time.Since(info.ModTime()) < maxAge {
			continue
		}
		if os.RemoveAll(filepath.Join(e.meta, "trash", entry.Name())) == nil {
			removed++
		}
	}
	return removed
}

// Sync pulls, then pushes, repeating when another commit lands in between.
func (e *Engine) Sync(opts PushOptions) (Result, error) {
	var total Result
	for attempt := 0; attempt < 5; attempt++ {
		pulled, err := e.Pull()
		total.Downloaded += pulled.Downloaded
		total.Pulled = total.Pulled || pulled.Pulled
		total.ConflictCopies = append(total.ConflictCopies, pulled.ConflictCopies...)
		total.SnapshotID = pulled.SnapshotID
		if err != nil {
			return total, err
		}
		pushed, err := e.Push(opts)
		total.Uploaded += pushed.Uploaded
		total.SnapshotID = pushed.SnapshotID
		if err == nil {
			total.Pushed = pushed.Pushed
			return total, nil
		}
		if KindOf(err) != KindHeadMoved {
			return total, err
		}
		// Someone committed between our pull and our push: go round again.
	}
	return total, fail(KindHeadMoved, "other commits kept arriving; try again")
}

// Restore commits the manifest of an older snapshot as a new version, then
// pulls. History is never rewritten.
func (e *Engine) Restore(snapshotID string) (Result, error) {
	// Local changes are committed first, so they stay in the history.
	res, err := e.Sync(PushOptions{})
	if err != nil {
		return res, err
	}
	wanted, err := e.Remote.Manifest(snapshotID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return res, fail(KindNotFound, "no such version: %s", snapshotID)
		}
		return res, fail(KindRemote, "%v", err)
	}
	if wanted.Equal(e.base) {
		res.Pushed = false
		return res, nil // the head already has exactly this content
	}
	message := "Restored an earlier version"
	if history, err := e.Remote.History(); err == nil {
		for _, snap := range history {
			if snap.ID == snapshotID {
				message = "Restored version from " + snap.CreatedAt
			}
		}
	}
	commit, err := e.Remote.Commit(core.CommitRequest{ParentID: e.state.BaseSnapshot, Manifest: wanted, Message: message})
	if err != nil {
		return res, fail(KindRemote, "%v", err)
	}
	switch commit.Status {
	case core.Committed, core.AlreadyCommitted:
	case core.HeadMoved:
		return res, fail(KindHeadMoved, "someone committed a newer version first")
	case core.LockNotHeld:
		return res, fail(KindLockNotHeld, "this device does not hold the project's lock")
	case core.BlobsMissing:
		return res, fail(KindNotFound, "that version's content has expired on the remote")
	default:
		return res, fail(KindRemote, "the restore was refused: %s %s", commit.Status, commit.Reason)
	}
	// The folder still equals the old head, so this pull is a plain update.
	pulled, err := e.Pull()
	pulled.Uploaded += res.Uploaded
	pulled.Downloaded += res.Downloaded
	pulled.Pushed = err == nil
	return pulled, err
}

// Status reports local changes and whether the remote is ahead.
func (e *Engine) Status() (Status, error) {
	st := Status{BaseID: e.state.BaseSnapshot, ConflictPending: e.state.ConflictPending}
	if headID, err := e.Remote.HeadID(); err == nil {
		st.RemoteReachable = true
		st.HeadID = headID
		st.RemoteAhead = headID != "" && headID != e.state.BaseSnapshot
	}
	local, err := e.LocalChanges()
	st.Local = local
	return st, err
}
