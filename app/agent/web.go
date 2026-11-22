package agent

import (
	_ "embed"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"henosis/core"
)

//go:embed page.html
var page []byte

// Handler serves the status page and its API. It is meant for 127.0.0.1
// only: requests that name another host, come from another site, or lack the
// page's own header are refused, so no web page can drive the app.
// agentDroppedFile is DroppedFile as the window sends it.
type agentDroppedFile = DroppedFile

func (a *Agent) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self' 'unsafe-inline'; frame-ancestors 'none'")
		w.Write(page)
	})
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) { reply(w, a.State(), nil) })
	mux.HandleFunc("GET /api/projects/{id}/history", func(w http.ResponseWriter, r *http.Request) {
		worker, err := a.Worker(r.PathValue("id"))
		if err != nil {
			reply(w, nil, err)
			return
		}
		history, err := worker.History()
		if err != nil {
			reply(w, nil, err)
			return
		}
		comments, err := worker.Comments()
		if comments == nil {
			comments = []core.Comment{}
		}
		reply(w, map[string]any{"versions": history, "comments": comments}, err)
	})

	mux.HandleFunc("GET /api/suggestions", func(w http.ResponseWriter, r *http.Request) {
		reply(w, map[string]any{"projects": a.Suggestions()}, nil)
	})
	mux.HandleFunc("GET /api/logs", func(w http.ResponseWriter, r *http.Request) {
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		reply(w, map[string]any{"entries": a.Events.After(after)}, nil)
	})

	type body struct {
		Server     string             `json:"server"`
		Token      string             `json:"token"`
		Name       string             `json:"name"`
		Folder     string             `json:"folder"`
		Code       string             `json:"code"`
		ProjectID  string             `json:"project_id"`
		SnapshotID string             `json:"snapshot_id"`
		Days       int                `json:"days"`
		Port       int                `json:"port"`
		On         bool               `json:"on"`
		AutoSend   bool               `json:"auto_send"`
		AutoUpdate bool               `json:"auto_update"`
		Text       string             `json:"text"`
		File       string             `json:"file"`
		Files      []agentDroppedFile `json:"files"`
	}
	post := func(pattern string, h func(r *http.Request, in body) (any, error)) {
		mux.HandleFunc("POST "+pattern, func(w http.ResponseWriter, r *http.Request) {
			var in body
			if r.Header.Get("X-Henosis") != "1" {
				reply(w, nil, errors.New("this request did not come from the status page"))
				return
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
				reply(w, nil, errors.New("unreadable request"))
				return
			}
			// Every press of a button leaves a line in the debug console.
			// Tokens and invite codes are never written there.
			what := strings.TrimPrefix(r.URL.Path, "/api/")
			var details []string
			for _, pair := range [][2]string{{"name", in.Name}, {"folder", in.Folder}, {"file", in.File}, {"server", in.Server}, {"version", in.SnapshotID}} {
				if pair[1] != "" {
					details = append(details, pair[0]+"="+strconv.Quote(pair[1]))
				}
			}
			if what != "ui-error" {
				a.logf("info", "> %s %s", what, strings.Join(details, " "))
			}
			out, err := h(r, in)
			if err != nil {
				a.logf("error", "%s failed: %v", what, err)
			} else if what != "ui-error" {
				a.logf("debug", "%s ok", what)
			}
			reply(w, out, err)
		})
	}
	post("/api/login", func(r *http.Request, in body) (any, error) { return nil, a.Login(in.Server, in.Token) })
	post("/api/logout", func(r *http.Request, in body) (any, error) { return nil, a.Logout() })
	post("/api/quit", func(r *http.Request, in body) (any, error) { a.Quit(); return nil, nil })
	post("/api/ui-error", func(r *http.Request, in body) (any, error) {
		if len(in.Text) > 2000 {
			in.Text = in.Text[:2000]
		}
		a.logf("error", "window: %s", in.Text)
		return nil, nil
	})
	post("/api/host/start", func(r *http.Request, in body) (any, error) {
		token, err := a.HostStart(in.Port, in.Name)
		return map[string]string{"token": token}, err
	})
	post("/api/connect", func(r *http.Request, in body) (any, error) { return nil, a.Connect(in.Name, in.Code) })
	post("/api/host/new-code", func(r *http.Request, in body) (any, error) { return nil, a.HostNewCode() })
	post("/api/host/extra", func(r *http.Request, in body) (any, error) { return nil, a.HostSetExtra(in.Text) })
	post("/api/host/relay", func(r *http.Request, in body) (any, error) { return nil, a.SetRelay(in.Text) })
	post("/api/host/stop", func(r *http.Request, in body) (any, error) { return nil, a.HostStop() })
	post("/api/host/account", func(r *http.Request, in body) (any, error) {
		token, err := a.HostAddAccount(in.Name)
		return map[string]string{"token": token}, err
	})
	post("/api/autostart", func(r *http.Request, in body) (any, error) { return nil, a.SetAutostart(in.On) })
	post("/api/drop", func(r *http.Request, in body) (any, error) {
		files := make([]DroppedFile, 0, len(in.Files))
		for _, f := range in.Files {
			files = append(files, DroppedFile(f))
		}
		return a.AddDropped(strings.TrimSpace(in.Name), files)
	})
	post("/api/create", func(r *http.Request, in body) (any, error) {
		if in.File != "" {
			return a.CreateFromFile(in.File)
		}
		return a.CreateProject(in.Name, in.Folder)
	})
	post("/api/join", func(r *http.Request, in body) (any, error) { return a.Join(in.Code, in.Folder) })
	post("/api/link", func(r *http.Request, in body) (any, error) { return a.LinkProject(in.ProjectID, in.Folder) })
	post("/api/projects/{id}/{action}", func(r *http.Request, in body) (any, error) {
		id := r.PathValue("id")
		switch r.PathValue("action") {
		case "unlink":
			return nil, a.Unlink(id)
		case "delete":
			return nil, a.DeleteProject(id)
		case "invite":
			code, err := a.Invite(id)
			return map[string]string{"code": code}, err
		case "lifetime":
			return nil, a.SetLifetime(id, in.Days)
		}
		worker, err := a.Worker(id)
		if err != nil {
			return nil, err
		}
		defer a.Kick(id)
		switch r.PathValue("action") {
		case "sync":
			worker.SyncNow()
		case "confirm-delete":
			worker.ConfirmDelete()
		case "yield":
			return nil, worker.Yield()
		case "take":
			worker.TakeLock()
		case "ask":
			return nil, worker.AskForLock()
		case "break":
			return nil, worker.BreakLock()
		case "collect":
			return worker.CollectSamples()
		case "restore":
			return nil, worker.Restore(in.SnapshotID)
		case "note":
			return nil, worker.SetNote(in.Text)
		case "comment":
			return nil, worker.Comment(in.SnapshotID, in.Text)
		case "open-folder":
			return nil, OpenPath(worker.Link.Folder)
		case "open-project":
			return nil, a.OpenProject(id)
		case "send":
			worker.SendNow()
		case "update":
			worker.UpdateNow()
		case "auto":
			worker.SetAutoSend(in.AutoSend)
			worker.SetAutoUpdate(in.AutoUpdate)
			return nil, a.SetAuto(id, in.AutoSend, in.AutoUpdate)
		default:
			return nil, errors.New("unknown action")
		}
		return nil, nil
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !localHost(r.Host) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			if u, err := url.Parse(origin); err != nil || u.Host != r.Host {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

// localHost accepts only names for this computer, which defeats DNS tricks
// that point a foreign name at 127.0.0.1.
func localHost(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	return host == "127.0.0.1" || strings.EqualFold(host, "localhost") || host == "::1"
}

func reply(w http.ResponseWriter, v any, err error) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if v == nil {
		v = map[string]bool{"ok": true}
	}
	json.NewEncoder(w).Encode(v)
}
