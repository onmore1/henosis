package core

import "errors"

// Errors every layer agrees on. Remotes return (or wrap) these so callers can
// test for them with errors.Is.
var (
	ErrNotFound  = errors.New("not found")
	ErrLockHeld  = errors.New("the project is locked by another member")
	ErrLockLost  = errors.New("the lock is no longer held by this device")
	ErrNotAtHead = errors.New("this device is not at the latest version")
	ErrForbidden = errors.New("your role in the project does not allow this")
	ErrSignIn    = errors.New("the server did not accept the token")
	ErrJoinCode  = errors.New("the session code is not valid any more")
	ErrNameTaken = errors.New("that name is already taken")
)

// Snapshot is one version of a project.
type Snapshot struct {
	ID             string   `json:"id"`
	Seq            int      `json:"seq"`       // order within the project, oldest first
	ParentID       string   `json:"parent_id"` // empty for the first snapshot
	ManifestSHA256 string   `json:"manifest_sha256"`
	Author         string   `json:"author"`
	Device         string   `json:"device,omitempty"`
	Message        string   `json:"message,omitempty"`
	FLVersion      string   `json:"fl_version,omitempty"`
	Plugins        []string `json:"plugins,omitempty"`
	IsConflict     bool     `json:"is_conflict"`
	CreatedAt      string   `json:"created_at"` // RFC 3339, UTC
}

// CommitRequest asks for a new snapshot on top of ParentID.
type CommitRequest struct {
	ParentID   string   `json:"parent_id"` // the caller's base; empty when the project has no snapshot
	Manifest   Manifest `json:"manifest"`
	Message    string   `json:"message,omitempty"`
	FLVersion  string   `json:"fl_version,omitempty"`
	Plugins    []string `json:"plugins,omitempty"`
	IsConflict bool     `json:"is_conflict,omitempty"`
}

// CommitStatus is the outcome of a commit.
type CommitStatus string

const (
	Committed        CommitStatus = "committed"
	AlreadyCommitted CommitStatus = "already_committed" // a retry of a commit that had succeeded
	HeadMoved        CommitStatus = "head_moved"        // ParentID is not the head
	LockNotHeld      CommitStatus = "lock_not_held"     // shared project, caller lacks the lock
	BlobsMissing     CommitStatus = "blobs_missing"
	InvalidManifest  CommitStatus = "invalid_manifest"
)

// CommitResult is the answer to a CommitRequest.
type CommitResult struct {
	Status   CommitStatus `json:"status"`
	Snapshot Snapshot     `json:"snapshot"`                   // for Committed and AlreadyCommitted
	HeadID   string       `json:"head_snapshot_id,omitempty"` // for HeadMoved
	Missing  []string     `json:"missing,omitempty"`          // for BlobsMissing
	Reason   string       `json:"reason,omitempty"`           // for InvalidManifest
}

// UserView is what the API shows of a user.
type UserView struct {
	ID   string `json:"id"`
	Name string `json:"username"`
}

// LockView describes who holds a project's lock.
type LockView struct {
	User        UserView `json:"user"`
	Device      string   `json:"device"`
	AcquiredAt  string   `json:"acquired_at"`
	HeartbeatAt string   `json:"heartbeat_at"`
	Stale       bool     `json:"stale"`
	RequestedBy string   `json:"requested_by,omitempty"` // a member asking for the lock
}

// MemberView is one member of a project.
type MemberView struct {
	User     UserView `json:"user"`
	Role     string   `json:"role"`
	JoinedAt string   `json:"joined_at"`
}

// ProjectView is a project as one member sees it.
type ProjectView struct {
	ID            string       `json:"id"`
	Name          string       `json:"name"`
	OwnerID       string       `json:"owner_id"`
	Role          string       `json:"role"`
	MemberCount   int          `json:"member_count"`
	RetentionDays int          `json:"retention_days"`
	Head          string       `json:"head_snapshot_id"`
	Lock          *LockView    `json:"lock"`
	CreatedAt     string       `json:"created_at"`
	Members       []MemberView `json:"members,omitempty"`
}

// Comment is a remark a member left on one version.
type Comment struct {
	ID         string `json:"id"`
	SnapshotID string `json:"snapshot_id"`
	Author     string `json:"author"`
	Text       string `json:"text"`
	CreatedAt  string `json:"created_at"`
}
