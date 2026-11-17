package store

import (
	"io"
	"os"

	"henosis/core"
)

// ProjectRemote gives one user on one device direct access to a project in
// this store, with the same calls the HTTP client offers. Tests use it, and
// so can a setup where the "server" is just a shared folder.
type ProjectRemote struct {
	Store     *Store
	UserID    string
	ProjectID string
	Device    string
}

func (r *ProjectRemote) HeadID() (string, error) {
	p, err := r.Store.Project(r.UserID, r.ProjectID)
	return p.Head, err
}

func (r *ProjectRemote) MissingBlobs(hashes []string) ([]string, error) {
	return r.Store.MissingBlobs(r.UserID, r.ProjectID, hashes)
}

func (r *ProjectRemote) UploadBlob(sha256, file string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	return r.Store.PutBlob(r.UserID, r.ProjectID, sha256, f)
}

func (r *ProjectRemote) DownloadBlob(sha256, destination string) error {
	path, err := r.Store.BlobPath(r.UserID, r.ProjectID, sha256)
	if err != nil {
		return err
	}
	in, err := os.Open(path)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(destination)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func (r *ProjectRemote) Commit(req core.CommitRequest) (core.CommitResult, error) {
	return r.Store.Commit(r.UserID, r.ProjectID, r.Device, req)
}

func (r *ProjectRemote) Manifest(snapshotID string) (core.Manifest, error) {
	return r.Store.Manifest(r.UserID, r.ProjectID, snapshotID)
}

func (r *ProjectRemote) History() ([]core.Snapshot, error) {
	return r.Store.Snapshots(r.UserID, r.ProjectID)
}

func (r *ProjectRemote) AcquireLock(baseSnapshotID string) error {
	_, _, err := r.Store.AcquireLock(r.UserID, r.ProjectID, r.Device, baseSnapshotID)
	return err
}

func (r *ProjectRemote) Heartbeat() error {
	return r.Store.Heartbeat(r.UserID, r.ProjectID, r.Device)
}

func (r *ProjectRemote) ReleaseLock() error {
	return r.Store.ReleaseLock(r.UserID, r.ProjectID, r.Device, false)
}
