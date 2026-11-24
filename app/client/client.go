// Package client talks to a Henosis server over HTTP.
package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"henosis/core"
)

// gavanashkaeewe
// Client is a signed-in connection to one server.
type Client struct {
	BaseURL string // for example "https://henosis.example.com"
	Token   string
	Device  string // names this computer; locks belong to a user on a device
	HTTP    *http.Client
}

// New returns a client with sensible timeouts for small requests. Blob
// transfers use no overall timeout, only a connection timeout.
func New(baseURL, token, device string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Token: token, Device: device,
		HTTP: &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: 60 * time.Second,
			IdleConnTimeout:       90 * time.Second,
		}}}
}

// APIError is an error answer from the server.
type APIError struct {
	Status  int
	Code    string
	Message string
	Holder  string // for lock_held: who holds the lock
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("the server answered %d %s", e.Status, e.Code)
}

// Is maps API error codes onto the shared errors, so errors.Is works.
func (e *APIError) Is(target error) bool {
	switch target {
	case core.ErrNotFound:
		return e.Code == "not_found"
	case core.ErrLockHeld:
		return e.Code == "lock_held"
	case core.ErrLockLost:
		return e.Code == "lock_lost"
	case core.ErrNotAtHead:
		return e.Code == "not_at_head"
	case core.ErrForbidden:
		return e.Code == "forbidden"
	case core.ErrSignIn:
		return e.Code == "unauthorized"
	case core.ErrJoinCode:
		return e.Code == "join_invalid"
	case core.ErrNameTaken:
		return e.Code == "name_taken"
	}
	return false
}

func (c *Client) request(method, path string, body io.Reader, contentLength int64) (*http.Response, error) {
	req, err := http.NewRequest(method, c.BaseURL+"/v1"+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("X-Henosis-Device", c.Device)
	if contentLength >= 0 {
		req.ContentLength = contentLength
	}
	return c.HTTP.Do(req)
}

func apiError(resp *http.Response) error {
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Details struct {
				Holder string `json:"holder"`
			} `json:"details"`
		} `json:"error"`
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = json.Unmarshal(data, &body)
	return &APIError{Status: resp.StatusCode, Code: body.Error.Code, Message: body.Error.Message, Holder: body.Error.Details.Holder}
}

// call sends JSON and decodes a JSON answer. accept lists the statuses that
// carry a normal answer (2xx always does).
func (c *Client) call(method, path string, in, out any, accept ...int) (int, error) {
	var body io.Reader
	length := int64(-1)
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body, length = bytes.NewReader(data), int64(len(data))
	}
	resp, err := c.request(method, path, body, length)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	ok := resp.StatusCode >= 200 && resp.StatusCode < 300
	for _, status := range accept {
		ok = ok || resp.StatusCode == status
	}
	if !ok {
		return resp.StatusCode, apiError(resp)
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, fmt.Errorf("the server sent an unreadable answer: %w", err)
		}
	}
	return resp.StatusCode, nil
}

// Join creates an account on a server with a session code's secret and
// returns the new account's token.
func Join(baseURL, secret, name string) (core.UserView, string, error) {
	c := New(baseURL, "", "")
	var out struct {
		Token string        `json:"token"`
		User  core.UserView `json:"user"`
	}
	_, err := c.call("POST", "/join", map[string]string{"secret": secret, "name": name}, &out)
	return out.User, out.Token, err
}

// Reachable reports whether a Henosis server answers at baseURL within the
// given time.
func Reachable(baseURL string, within time.Duration) bool {
	return reachableWith(baseURL, &http.Client{Timeout: within})
}

// ReachableWithClient is like Reachable but uses the given HTTP client, so
// callers can probe through the relay.
func ReachableWithClient(baseURL string, client *http.Client) bool {
	return reachableWith(baseURL, client)
}

func reachableWith(baseURL string, client *http.Client) bool {
	resp, err := client.Get(strings.TrimRight(baseURL, "/") + "/v1/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var out struct {
		OK bool `json:"ok"`
	}
	return resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(&out) == nil && out.OK
}

// NewWith returns a client that uses the given HTTP client. Hénōsis uses
// this to hand the client a transport that dials through the relay.
func NewWith(baseURL, token, device string, httpClient *http.Client) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Token: token, Device: device, HTTP: httpClient}
}

// JoinWithClient is like Join but uses the given HTTP client. Hénōsis uses
// this to join through the relay.
func JoinWithClient(baseURL, secret, name string, httpClient *http.Client) (core.UserView, string, error) {
	c := NewWith(baseURL, "", "", httpClient)
	var out struct {
		Token string        `json:"token"`
		User  core.UserView `json:"user"`
	}
	_, err := c.call("POST", "/join", map[string]string{"secret": secret, "name": name}, &out)
	return out.User, out.Token, err
}

// Me returns the signed-in user.
func (c *Client) Me() (core.UserView, error) {
	var u core.UserView
	_, err := c.call("GET", "/me", nil, &u)
	return u, err
}

func (c *Client) CreateProject(name string) (core.ProjectView, error) {
	var p core.ProjectView
	_, err := c.call("POST", "/projects", map[string]string{"name": name}, &p)
	return p, err
}

func (c *Client) Projects() ([]core.ProjectView, error) {
	var out struct {
		Projects []core.ProjectView `json:"projects"`
	}
	_, err := c.call("GET", "/projects", nil, &out)
	return out.Projects, err
}

func (c *Client) Project(id string) (core.ProjectView, error) {
	var p core.ProjectView
	_, err := c.call("GET", "/projects/"+url.PathEscape(id), nil, &p)
	return p, err
}

// UpdateProject changes the name or the version lifetime; nil leaves a field alone.
func (c *Client) UpdateProject(id string, name *string, retentionDays *int) (core.ProjectView, error) {
	var p core.ProjectView
	body := map[string]any{}
	if name != nil {
		body["name"] = *name
	}
	if retentionDays != nil {
		body["retention_days"] = *retentionDays
	}
	_, err := c.call("PATCH", "/projects/"+url.PathEscape(id), body, &p)
	return p, err
}

// DeleteProject removes a project from the server for everyone. Owner only.
func (c *Client) DeleteProject(id string) error {
	_, err := c.call("DELETE", "/projects/"+url.PathEscape(id), nil, nil)
	return err
}

// CreateInvite returns a single-use code for one new member.
func (c *Client) CreateInvite(projectID string) (string, error) {
	var out struct {
		Code string `json:"code"`
	}
	_, err := c.call("POST", "/projects/"+url.PathEscape(projectID)+"/invites", nil, &out)
	return out.Code, err
}

func (c *Client) AcceptInvite(code string) (core.ProjectView, error) {
	var p core.ProjectView
	_, err := c.call("POST", "/invites/"+url.PathEscape(strings.TrimSpace(code))+"/accept", nil, &p)
	return p, err
}

func (c *Client) RemoveMember(projectID, userID string) error {
	_, err := c.call("DELETE", "/projects/"+url.PathEscape(projectID)+"/members/"+url.PathEscape(userID), nil, nil)
	return err
}

// ProjectRemote is one project on the server. It implements engine.Remote
// and engine.Locker.
type ProjectRemote struct {
	C         *Client
	ProjectID string
}

// Remote returns the remote for one project.
func (c *Client) Remote(projectID string) *ProjectRemote {
	return &ProjectRemote{C: c, ProjectID: projectID}
}

func (r *ProjectRemote) path(suffix string) string {
	return "/projects/" + url.PathEscape(r.ProjectID) + suffix
}

// View returns the project as the server sees it now: head, lock, members.
func (r *ProjectRemote) View() (core.ProjectView, error) { return r.C.Project(r.ProjectID) }

func (r *ProjectRemote) HeadID() (string, error) {
	p, err := r.C.Project(r.ProjectID)
	return p.Head, err
}

func (r *ProjectRemote) MissingBlobs(hashes []string) ([]string, error) {
	if len(hashes) == 0 {
		return nil, nil
	}
	var out struct {
		Missing []string `json:"missing"`
	}
	_, err := r.C.call("POST", r.path("/blobs/check"), map[string]any{"hashes": hashes}, &out)
	return out.Missing, err
}

func (r *ProjectRemote) UploadBlob(sha256, file string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	resp, err := r.C.request("PUT", r.path("/blobs/"+sha256), f, info.Size())
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return apiError(resp)
	}
	return nil
}

func (r *ProjectRemote) DownloadBlob(sha256, destination string) error {
	resp, err := r.C.request("GET", r.path("/blobs/"+sha256), nil, -1)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return apiError(resp)
	}
	out, err := os.Create(destination)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func (r *ProjectRemote) Commit(req core.CommitRequest) (core.CommitResult, error) {
	var res core.CommitResult
	_, err := r.C.call("POST", r.path("/snapshots"), req, &res,
		http.StatusConflict, http.StatusForbidden, http.StatusUnprocessableEntity)
	if err == nil && res.Status == "" {
		// A 403 or 409 that is not a commit answer (for example a plain error).
		return res, fmt.Errorf("the server refused the commit")
	}
	return res, err
}

func (r *ProjectRemote) Manifest(snapshotID string) (core.Manifest, error) {
	var m core.Manifest
	_, err := r.C.call("GET", r.path("/snapshots/"+url.PathEscape(snapshotID)+"/manifest"), nil, &m)
	if err != nil {
		return core.Manifest{}, err
	}
	// Never trust a manifest that breaks the path rules: it decides where
	// files are written on this computer.
	if err := m.Validate(); err != nil {
		return core.Manifest{}, fmt.Errorf("the server sent an invalid manifest: %w", err)
	}
	return m, nil
}

func (r *ProjectRemote) History() ([]core.Snapshot, error) {
	var out struct {
		Snapshots []core.Snapshot `json:"snapshots"`
	}
	_, err := r.C.call("GET", r.path("/snapshots"), nil, &out)
	return out.Snapshots, err
}

func (r *ProjectRemote) AcquireLock(baseSnapshotID string) error {
	_, err := r.C.call("POST", r.path("/lock"), map[string]string{"base_snapshot_id": baseSnapshotID}, nil)
	return err
}

func (r *ProjectRemote) Heartbeat() error {
	_, err := r.C.call("POST", r.path("/lock/heartbeat"), nil, nil)
	return err
}

func (r *ProjectRemote) ReleaseLock() error {
	_, err := r.C.call("DELETE", r.path("/lock"), nil, nil)
	return err
}

// Comments lists the project's comments on versions, oldest first.
func (r *ProjectRemote) Comments() ([]core.Comment, error) {
	var out struct {
		Comments []core.Comment `json:"comments"`
	}
	_, err := r.C.call("GET", r.path("/comments"), nil, &out)
	return out.Comments, err
}

// AddComment leaves a remark on one version.
func (r *ProjectRemote) AddComment(snapshotID, text string) (core.Comment, error) {
	var c core.Comment
	_, err := r.C.call("POST", r.path("/snapshots/"+url.PathEscape(snapshotID)+"/comments"), map[string]string{"text": text}, &c)
	return c, err
}

// ForceRelease frees anyone's lock. Owner only.
func (r *ProjectRemote) ForceRelease() error {
	_, err := r.C.call("DELETE", r.path("/lock?force=true"), nil, nil)
	return err
}

// RequestLock tells the holder that this user would like the lock.
func (r *ProjectRemote) RequestLock() error {
	_, err := r.C.call("POST", r.path("/lock/request"), nil, nil)
	return err
}
