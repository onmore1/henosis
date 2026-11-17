package engine

import (
	"errors"

	"henosis/core"
)

// ErrNotFound is returned by a Remote for an unknown snapshot or blob.
var ErrNotFound = core.ErrNotFound

// Locker is the lock side of a remote.
type Locker interface {
	// AcquireLock takes the project lock for this device. It returns an
	// error matching core.ErrLockHeld or core.ErrNotAtHead when it cannot.
	AcquireLock(baseSnapshotID string) error
	ReleaseLock() error
}

// AutoLock wraps a remote so that every commit takes the lock just for the
// commit and releases it again. One-shot commands use it; the background
// agent holds the lock for a whole session instead.
type AutoLock struct {
	Remote
	Locker Locker
}

// Commit takes the lock, commits, and releases the lock.
func (a AutoLock) Commit(req core.CommitRequest) (core.CommitResult, error) {
	switch err := a.Locker.AcquireLock(req.ParentID); {
	case errors.Is(err, core.ErrLockHeld):
		return core.CommitResult{Status: core.LockNotHeld}, nil
	case errors.Is(err, core.ErrNotAtHead):
		// Normally someone committed first. But this can also be the retry of
		// a commit whose answer was lost, which the remote recognises itself.
		if res, err := a.Remote.Commit(req); err == nil && res.Status == core.AlreadyCommitted {
			return res, nil
		}
		return core.CommitResult{Status: core.HeadMoved}, nil
	case err != nil:
		return core.CommitResult{}, err
	}
	defer a.Locker.ReleaseLock()
	return a.Remote.Commit(req)
}

// Remote is what the engine needs from the place versions are stored. The
// HTTP client implements it against the server.
type Remote interface {
	// HeadID is the id of the latest snapshot, or "" when there is none yet.
	HeadID() (string, error)
	// MissingBlobs returns which of the hashes are not stored yet.
	MissingBlobs(hashes []string) ([]string, error)
	// UploadBlob stores a file under its hash. The remote refuses bytes that
	// do not match the hash.
	UploadBlob(sha256, file string) error
	// DownloadBlob writes a blob to destination. The engine verifies it.
	DownloadBlob(sha256, destination string) error
	Commit(req core.CommitRequest) (core.CommitResult, error)
	Manifest(snapshotID string) (core.Manifest, error)
	// History is the list of snapshots, newest first.
	History() ([]core.Snapshot, error)
}
