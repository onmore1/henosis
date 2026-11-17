// Package server is the HTTP API in front of the store. See "API reference"
// in the technical docs.
package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"henosis/core"
	"henosis/store"
)

const maxJSONBody = 8 << 20 // 8 MiB, enough for a manifest of tens of thousands of files

// Server serves the Henosis API for one store.
type Server struct {
	Store *store.Store
	// Log, when set, receives one line per request. It never sees tokens or
	// invite codes: the route pattern is logged, not the URL.
	Log func(format string, args ...any)
	mux *http.ServeMux
}

// statusWriter remembers the status code for the request log.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(data)
}

type ctxUser struct {
	user   core.UserView
	device string
}

// New builds the API handler.
func New(st *store.Store) *Server {
	s := &Server{Store: st, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	// Joining a session needs no token: the session code's secret is the proof.
	s.mux.HandleFunc("POST /v1/join", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Secret string `json:"secret"`
			Name   string `json:"name"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		if !readJSON(w, r, &body) {
			return
		}
		user, token, err := st.Join(body.Secret, strings.TrimSpace(body.Name))
		if s.Log != nil {
			s.Log("- POST /v1/join -> %v", err == nil)
		}
		reply(w, http.StatusCreated, map[string]any{"token": token, "user": user}, err)
	})
	route := func(pattern string, h func(w http.ResponseWriter, r *http.Request, c ctxUser)) {
		s.mux.HandleFunc(pattern, func(plain http.ResponseWriter, r *http.Request) {
			started := time.Now()
			w := &statusWriter{ResponseWriter: plain}
			token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			user, found := st.UserByToken(token)
			if !ok || !found {
				writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid token", nil)
			} else {
				r.Body = http.MaxBytesReader(w, r.Body, bodyLimit(r))
				h(w, r, ctxUser{user: user, device: r.Header.Get("X-Henosis-Device")})
			}
			if s.Log != nil {
				name := user.Name
				if name == "" {
					name = "-"
				}
				s.Log("%s %s -> %d (%s, %s)", name, pattern, w.status, r.PathValue("id"), time.Since(started).Round(time.Millisecond))
			}
		})
	}

	route("GET /v1/me", func(w http.ResponseWriter, r *http.Request, c ctxUser) {
		writeJSON(w, http.StatusOK, c.user)
	})
	route("POST /v1/projects", func(w http.ResponseWriter, r *http.Request, c ctxUser) {
		var body struct {
			Name string `json:"name"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		p, err := st.CreateProject(c.user.ID, body.Name)
		reply(w, http.StatusCreated, p, err)
	})
	route("GET /v1/projects", func(w http.ResponseWriter, r *http.Request, c ctxUser) {
		writeJSON(w, http.StatusOK, map[string]any{"projects": st.Projects(c.user.ID)})
	})
	route("GET /v1/projects/{id}", func(w http.ResponseWriter, r *http.Request, c ctxUser) {
		p, err := st.Project(c.user.ID, r.PathValue("id"))
		reply(w, http.StatusOK, p, err)
	})
	route("PATCH /v1/projects/{id}", func(w http.ResponseWriter, r *http.Request, c ctxUser) {
		var body struct {
			Name          *string `json:"name"`
			RetentionDays *int    `json:"retention_days"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		p, err := st.UpdateProject(c.user.ID, r.PathValue("id"), body.Name, body.RetentionDays)
		reply(w, http.StatusOK, p, err)
	})
	route("DELETE /v1/projects/{id}", func(w http.ResponseWriter, r *http.Request, c ctxUser) {
		reply(w, http.StatusNoContent, nil, st.DeleteProject(c.user.ID, r.PathValue("id")))
	})
	route("POST /v1/projects/{id}/invites", func(w http.ResponseWriter, r *http.Request, c ctxUser) {
		code, expires, err := st.CreateInvite(c.user.ID, r.PathValue("id"))
		reply(w, http.StatusCreated, map[string]string{"code": code, "expires_at": expires.UTC().Format("2006-01-02T15:04:05Z")}, err)
	})
	route("POST /v1/invites/{code}/accept", func(w http.ResponseWriter, r *http.Request, c ctxUser) {
		p, err := st.AcceptInvite(c.user.ID, r.PathValue("code"))
		reply(w, http.StatusOK, p, err)
	})
	route("DELETE /v1/projects/{id}/members/{user}", func(w http.ResponseWriter, r *http.Request, c ctxUser) {
		reply(w, http.StatusNoContent, nil, st.RemoveMember(c.user.ID, r.PathValue("id"), r.PathValue("user")))
	})

	route("POST /v1/projects/{id}/lock", func(w http.ResponseWriter, r *http.Request, c ctxUser) {
		var body struct {
			BaseSnapshotID string `json:"base_snapshot_id"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		view, takenOver, err := st.AcquireLock(c.user.ID, r.PathValue("id"), c.device, body.BaseSnapshotID)
		reply(w, http.StatusOK, map[string]any{"lock": view, "taken_over": takenOver}, err)
	})
	route("POST /v1/projects/{id}/lock/heartbeat", func(w http.ResponseWriter, r *http.Request, c ctxUser) {
		reply(w, http.StatusNoContent, nil, st.Heartbeat(c.user.ID, r.PathValue("id"), c.device))
	})
	route("POST /v1/projects/{id}/lock/request", func(w http.ResponseWriter, r *http.Request, c ctxUser) {
		reply(w, http.StatusNoContent, nil, st.RequestLock(c.user.ID, r.PathValue("id")))
	})
	route("DELETE /v1/projects/{id}/lock", func(w http.ResponseWriter, r *http.Request, c ctxUser) {
		force := r.URL.Query().Get("force") == "true"
		reply(w, http.StatusNoContent, nil, st.ReleaseLock(c.user.ID, r.PathValue("id"), c.device, force))
	})

	route("POST /v1/projects/{id}/blobs/check", func(w http.ResponseWriter, r *http.Request, c ctxUser) {
		var body struct {
			Hashes []string `json:"hashes"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		missing, err := st.MissingBlobs(c.user.ID, r.PathValue("id"), body.Hashes)
		reply(w, http.StatusOK, map[string]any{"missing": missing}, err)
	})
	route("PUT /v1/projects/{id}/blobs/{sha}", func(w http.ResponseWriter, r *http.Request, c ctxUser) {
		reply(w, http.StatusNoContent, nil, st.PutBlob(c.user.ID, r.PathValue("id"), r.PathValue("sha"), r.Body))
	})
	route("GET /v1/projects/{id}/blobs/{sha}", func(w http.ResponseWriter, r *http.Request, c ctxUser) {
		path, err := st.BlobPath(c.user.ID, r.PathValue("id"), r.PathValue("sha"))
		if err != nil {
			reply(w, 0, nil, err)
			return
		}
		f, err := os.Open(path)
		if err != nil {
			reply(w, 0, nil, core.ErrNotFound)
			return
		}
		defer f.Close()
		info, _ := f.Stat()
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, r, "", info.ModTime(), f) // supports Range, so downloads can resume
	})

	route("POST /v1/projects/{id}/snapshots", func(w http.ResponseWriter, r *http.Request, c ctxUser) {
		var req core.CommitRequest
		if !readJSON(w, r, &req) {
			return
		}
		res, err := st.Commit(c.user.ID, r.PathValue("id"), c.device, req)
		if err != nil {
			reply(w, 0, nil, err)
			return
		}
		status := map[core.CommitStatus]int{
			core.Committed: http.StatusCreated, core.AlreadyCommitted: http.StatusOK,
			core.HeadMoved: http.StatusConflict, core.LockNotHeld: http.StatusForbidden,
			core.BlobsMissing: http.StatusUnprocessableEntity, core.InvalidManifest: http.StatusUnprocessableEntity,
		}[res.Status]
		writeJSON(w, status, res)
	})
	route("GET /v1/projects/{id}/snapshots", func(w http.ResponseWriter, r *http.Request, c ctxUser) {
		snaps, err := st.Snapshots(c.user.ID, r.PathValue("id"))
		reply(w, http.StatusOK, map[string]any{"snapshots": snaps}, err)
	})
	route("GET /v1/projects/{id}/comments", func(w http.ResponseWriter, r *http.Request, c ctxUser) {
		comments, err := st.Comments(c.user.ID, r.PathValue("id"))
		reply(w, http.StatusOK, map[string]any{"comments": comments}, err)
	})
	route("POST /v1/projects/{id}/snapshots/{sid}/comments", func(w http.ResponseWriter, r *http.Request, c ctxUser) {
		var body struct {
			Text string `json:"text"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		comment, err := st.AddComment(c.user.ID, r.PathValue("id"), r.PathValue("sid"), body.Text)
		reply(w, http.StatusCreated, comment, err)
	})
	route("GET /v1/projects/{id}/snapshots/{sid}/manifest", func(w http.ResponseWriter, r *http.Request, c ctxUser) {
		m, err := st.Manifest(c.user.ID, r.PathValue("id"), r.PathValue("sid"))
		reply(w, http.StatusOK, m, err)
	})
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// bodyLimit: blob uploads may be large; everything else is small JSON.
func bodyLimit(r *http.Request) int64 {
	if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/blobs/") {
		return 16 << 30
	}
	return maxJSONBody
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string, details any) {
	body := map[string]any{"code": code, "message": message}
	if details != nil {
		body["details"] = details
	}
	writeJSON(w, status, map[string]any{"error": body})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", "the request body is too large", nil)
		} else {
			writeError(w, http.StatusBadRequest, "invalid_request", "the request body is not valid JSON", nil)
		}
		return false
	}
	return true
}

// reply writes v with the given status, or turns a store error into the
// matching API error.
func reply(w http.ResponseWriter, status int, v any, err error) {
	var held *store.LockHeldError
	switch {
	case err == nil:
		if status == http.StatusNoContent {
			w.WriteHeader(status)
		} else {
			writeJSON(w, status, v)
		}
	case errors.As(err, &held):
		writeError(w, http.StatusConflict, "lock_held", err.Error(), map[string]string{"holder": held.Holder})
	case errors.Is(err, core.ErrJoinCode):
		writeError(w, http.StatusForbidden, "join_invalid", err.Error(), nil)
	case errors.Is(err, core.ErrNameTaken):
		writeError(w, http.StatusConflict, "name_taken", err.Error(), nil)
	case errors.Is(err, core.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "no such resource", nil)
	case errors.Is(err, store.ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden", "your role does not allow this", nil)
	case errors.Is(err, core.ErrLockLost):
		writeError(w, http.StatusConflict, "lock_lost", err.Error(), nil)
	case errors.Is(err, core.ErrNotAtHead):
		writeError(w, http.StatusConflict, "not_at_head", err.Error(), nil)
	case errors.Is(err, store.ErrInviteInvalid):
		writeError(w, http.StatusGone, "invite_invalid", err.Error(), nil)
	case errors.Is(err, store.ErrBlobMismatch):
		writeError(w, http.StatusUnprocessableEntity, "blob_mismatch", err.Error(), nil)
	case errors.Is(err, store.ErrBadRequest):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error(), nil)
	default:
		writeError(w, http.StatusInternalServerError, "internal", "server error", nil)
	}
}
