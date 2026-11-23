package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"henosis/agent"
	"henosis/agent/relay/relaytest"
	"henosis/config"
	"henosis/fl"
	"henosis/server"
	"henosis/store"
)

// desk is one computer running the app: an agent and its status page.
type desk struct {
	t    *testing.T
	a    *agent.Agent
	page *httptest.Server
	home string
}

func newDesk(t *testing.T, name string) *desk {
	t.Helper()
	return newDeskWithHome(t, filepath.Join(t.TempDir(), name))
}

// newDeskWithHome starts an app on settings that are already there, like a
// second run on the same computer.
func newDeskWithHome(t *testing.T, home string) *desk {
	t.Helper()
	cfg, err := config.Load(filepath.Join(home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	a := agent.New(cfg, &fl.Fake{})
	a.BaseFolder = filepath.Join(home, "Documents", "Henosis")
	a.ProjectRoots = []string{filepath.Join(home, "Documents", "Image-Line", "FL Studio", "Projects")}
	a.SearchRoots = []string{home}
	a.LocalIPs = func() []string { return []string{"127.0.0.1"} }
	a.AutoLinkEvery = 50 * time.Millisecond
	a.TickEvery = 20 * time.Millisecond
	a.Timing.Quiet, a.Timing.Poll, a.Timing.CloseDelay = 60*time.Millisecond, 60*time.Millisecond, 60*time.Millisecond
	a.Timing.RetryMin = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	a.Start(ctx)
	page := httptest.NewServer(a.Handler())
	t.Cleanup(func() { page.Close(); cancel(); a.Close() })
	return &desk{t: t, a: a, page: page, home: home}
}

// call talks to the status page's API the way the page does.
func (d *desk) call(method, path string, body any, out any) (int, string) {
	d.t.Helper()
	var reader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reader = bytes.NewReader(data)
	}
	req, _ := http.NewRequest(method, d.page.URL+path, reader)
	if method == "POST" {
		req.Header.Set("X-Henosis", "1")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		d.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode == 200 {
		if err := json.Unmarshal(data, out); err != nil {
			d.t.Fatalf("%s %s: %v in %s", method, path, err, data)
		}
	}
	return resp.StatusCode, string(data)
}

func (d *desk) ok(path string, body any, out any) {
	d.t.Helper()
	if body == nil {
		body = map[string]string{}
	}
	if code, text := d.call("POST", path, body, out); code != 200 {
		d.t.Fatalf("POST %s: %d %s", path, code, text)
	}
}

func (d *desk) state() agent.AppState {
	d.t.Helper()
	var st agent.AppState
	if code, text := d.call("GET", "/api/state", nil, &st); code != 200 {
		d.t.Fatalf("state: %d %s", code, text)
	}
	return st
}

func eventually(t *testing.T, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTwoComputersThroughTheStatusPage(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "server"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(st))
	defer srv.Close()
	_, annToken, _ := st.AddUser("ann")
	_, bobToken, _ := st.AddUser("bob")
	ann, bob := newDesk(t, "ann"), newDesk(t, "bob")

	// Signing in.
	if s := ann.state(); s.SignedIn || len(s.Projects) != 0 {
		t.Fatalf("fresh state: %+v", s)
	}
	if code, text := ann.call("POST", "/api/login", map[string]string{"server": srv.URL, "token": "hn_wrong"}, nil); code != 400 || !strings.Contains(text, "did not accept") {
		t.Fatalf("login with a wrong token: %d %s", code, text)
	}
	if code, _ := ann.call("POST", "/api/create", map[string]string{"name": "X"}, nil); code != 400 {
		t.Fatal("creating a project without signing in worked")
	}
	ann.ok("/api/login", map[string]string{"server": srv.URL + "/", "token": " " + annToken + " "}, nil)
	bob.ok("/api/login", map[string]string{"server": srv.URL, "token": bobToken}, nil)
	if s := ann.state(); !s.SignedIn || s.User != "ann" || s.Device == "" {
		t.Fatalf("state after login: %+v", s)
	}
	// The token is on disk for the next start, readable by this user only.
	saved, _ := config.Load(filepath.Join(ann.home, "config.json"))
	if !saved.SignedIn() || saved.User != "ann" {
		t.Fatalf("saved config: %+v", saved)
	}

	// Ann creates a project from an existing folder with files in it.
	annFolder := filepath.Join(ann.home, "Projects", "My Song")
	os.MkdirAll(annFolder, 0o755)
	os.WriteFile(filepath.Join(annFolder, "My Song.flp"), []byte("v1"), 0o644)
	if code, text := ann.call("POST", "/api/create", map[string]string{"name": " ", "folder": " "}, nil); code != 400 {
		t.Fatalf("a project without a name or a folder: %d %s", code, text)
	}
	if code, text := ann.call("POST", "/api/create", map[string]string{"folder": filepath.Join(ann.home, "typo")}, nil); code != 400 || !strings.Contains(text, "there is no folder") {
		t.Fatalf("a project in a folder that does not exist: %d %s", code, text)
	}
	var link config.Link
	ann.ok("/api/create", map[string]string{"name": "My Song", "folder": annFolder}, &link)
	os.MkdirAll(filepath.Join(annFolder, "Audio"), 0o755)
	if code, text := ann.call("POST", "/api/create", map[string]string{"name": "Dup", "folder": filepath.Join(annFolder, "Audio")}, nil); code != 400 || !strings.Contains(text, "overlaps") {
		t.Fatalf("a folder inside a linked folder: %d %s", code, text)
	}
	if list := st.Projects(mustID(t, st, annToken)); len(list) != 1 {
		t.Fatalf("a refused create left a project behind: %d projects", len(list))
	}
	eventually(t, "ann's first upload", func() bool {
		s := ann.state()
		return len(s.Projects) == 1 && s.Projects[0].State == "synced" && s.Projects[0].LastSync != ""
	})

	// Ann invites; bob joins into a folder of his choice.
	var invite struct{ Code string }
	ann.ok("/api/projects/"+link.ProjectID+"/invite", nil, &invite)
	if code, _ := bob.call("POST", "/api/join", map[string]string{"code": "HN-AAAA-AAAA-AAAA"}, nil); code != 400 {
		t.Fatal("a made-up code worked")
	}
	bobFolder := filepath.Join(bob.home, "Music", "From Ann")
	bob.ok("/api/join", map[string]string{"code": invite.Code, "folder": bobFolder}, nil)
	eventually(t, "bob's download", func() bool {
		data, _ := os.ReadFile(filepath.Join(bobFolder, "My Song.flp"))
		return string(data) == "v1"
	})
	if code, _ := bob.call("POST", "/api/projects/"+link.ProjectID+"/invite", map[string]string{}, nil); code != 400 {
		t.Fatal("an editor made an invite")
	}

	// A change on bob's side reaches ann without anyone doing anything.
	os.WriteFile(filepath.Join(bobFolder, "bass.wav"), []byte("bass"), 0o644)
	eventually(t, "bob's file at ann's", func() bool {
		data, _ := os.ReadFile(filepath.Join(annFolder, "bass.wav"))
		return string(data) == "bass"
	})
	eventually(t, "both shown as shared", func() bool {
		s := ann.state()
		return s.Projects[0].Shared && len(s.Projects[0].Members) == 2
	})

	// Versions and restore.
	var history struct {
		Versions []struct{ ID, Message, Author string }
	}
	if code, text := ann.call("GET", "/api/projects/"+link.ProjectID+"/history", nil, &history); code != 200 || len(history.Versions) != 2 {
		t.Fatalf("history: %d %s", code, text)
	}
	if history.Versions[0].Author != "bob" || history.Versions[0].Message != "Added bass.wav" {
		t.Fatalf("history: %+v", history.Versions)
	}
	ann.ok("/api/projects/"+link.ProjectID+"/restore", map[string]string{"snapshot_id": history.Versions[1].ID}, nil)
	eventually(t, "the restore at bob's", func() bool {
		_, err := os.Stat(filepath.Join(bobFolder, "bass.wav"))
		return os.IsNotExist(err)
	})

	// Settings: only the owner changes the lifetime of old versions.
	ann.ok("/api/projects/"+link.ProjectID+"/lifetime", map[string]int{"days": 10}, nil)
	if code, _ := bob.call("POST", "/api/projects/"+link.ProjectID+"/lifetime", map[string]int{"days": 1}, nil); code != 400 {
		t.Fatal("an editor changed the lifetime")
	}
	eventually(t, "the new lifetime on both pages", func() bool {
		return ann.state().Projects[0].RetentionDays == 10 && bob.state().Projects[0].RetentionDays == 10
	})
	ann.ok("/api/projects/"+link.ProjectID+"/sync", nil, nil)
	if code, _ := ann.call("POST", "/api/projects/"+link.ProjectID+"/frobnicate", map[string]string{}, nil); code != 400 {
		t.Fatal("an unknown action worked")
	}

	// Unlinking keeps the files and offers the project again.
	bob.ok("/api/projects/"+link.ProjectID+"/unlink", nil, nil)
	if _, err := os.Stat(filepath.Join(bobFolder, "My Song.flp")); err != nil {
		t.Fatal("unlinking removed files")
	}
	eventually(t, "the project listed as not on this computer", func() bool {
		s := bob.state()
		return len(s.Projects) == 0 && len(s.Unlinked) == 1 && s.Unlinked[0].Name == "My Song"
	})
	// Linking it again to the same folder picks up where it was.
	bob.ok("/api/link", map[string]string{"project_id": link.ProjectID, "folder": bobFolder}, nil)
	os.WriteFile(filepath.Join(annFolder, "lead.wav"), []byte("lead"), 0o644)
	eventually(t, "sync after linking again", func() bool {
		data, _ := os.ReadFile(filepath.Join(bobFolder, "lead.wav"))
		return string(data) == "lead"
	})

	// Signing out stops syncing; quitting is announced.
	bob.ok("/api/logout", nil, nil)
	if s := bob.state(); s.SignedIn || len(s.Projects) != 0 {
		t.Fatalf("after logout: %+v", s)
	}
	ann.ok("/api/quit", nil, nil)
	select {
	case <-ann.a.Done():
	case <-time.After(time.Second):
		t.Fatal("quit was not announced")
	}
}

func mustID(t *testing.T, st *store.Store, token string) string {
	t.Helper()
	u, ok := st.UserByToken(token)
	if !ok {
		t.Fatal("unknown token")
	}
	return u.ID
}

func TestStatusPageRefusesOutsiders(t *testing.T) {
	d := newDesk(t, "ann")
	get := func(mutate func(*http.Request)) int {
		req, _ := http.NewRequest("POST", d.page.URL+"/api/logout", strings.NewReader("{}"))
		req.Header.Set("X-Henosis", "1")
		mutate(req)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := get(func(r *http.Request) {}); code != 200 {
		t.Fatalf("the page's own request: %d", code)
	}
	if code := get(func(r *http.Request) { r.Header.Del("X-Henosis") }); code != 400 {
		t.Errorf("a request without the page's header: %d", code)
	}
	if code := get(func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }); code != 403 {
		t.Errorf("a request from another site: %d", code)
	}
	if code := get(func(r *http.Request) { r.Host = "evil.example" }); code != 403 {
		t.Errorf("a request naming another host: %d", code)
	}
	resp, err := http.Get(d.page.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "<title>Hénōsis</title>") {
		t.Fatalf("the page itself: %d", resp.StatusCode)
	}
}

// logs returns the debug console's lines as one text.
func (d *desk) logs(after int64) (string, int64) {
	d.t.Helper()
	var out struct{ Entries []agent.LogEntry }
	if code, text := d.call("GET", "/api/logs?after="+strconv.FormatInt(after, 10), nil, &out); code != 200 {
		d.t.Fatalf("logs: %d %s", code, text)
	}
	var b strings.Builder
	last := after
	for _, e := range out.Entries {
		b.WriteString(e.Level + " " + e.Text + "\n")
		last = e.Seq
	}
	return b.String(), last
}

func TestSessionInTwoClicks(t *testing.T) {
	ann, bob := newDesk(t, "ann"), newDesk(t, "bob")

	// Ann: type a name, press "Start a session". That is all.
	if code, text := ann.call("POST", "/api/host/start", map[string]any{"name": "bad name!", "port": 0}, nil); code != 400 {
		t.Fatalf("hosting with an invalid name: %d %s", code, text)
	}
	ann.ok("/api/host/start", map[string]any{"name": "ann", "port": 0}, nil)
	st := ann.state()
	if !st.SignedIn || st.User != "ann" || !st.Host.Running || !strings.HasPrefix(st.Host.Code, "HEN") {
		t.Fatalf("after starting a session: %+v", st)
	}
	sessionCode := st.Host.Code

	// Ann adds a project just by pointing at its folder; it is named after the folder.
	annFolder := filepath.Join(ann.home, "Beats", "Night Drive")
	os.MkdirAll(annFolder, 0o755)
	os.WriteFile(filepath.Join(annFolder, "Night Drive.flp"), []byte("v1"), 0o644)
	var link config.Link
	ann.ok("/api/create", map[string]string{"folder": annFolder}, &link)
	if link.Name != "Night Drive" {
		t.Fatalf("the project is called %q", link.Name)
	}

	// Bob: type a name, paste the code, press Join.
	for what, in := range map[string]map[string]string{
		"no name":         {"name": " ", "code": sessionCode},
		"not a code":      {"name": "bob", "code": "hello"},
		"a mistyped code": {"name": "bob", "code": sessionCode[:20] + map[bool]string{true: "B", false: "A"}[sessionCode[20] == 'A'] + sessionCode[21:]},
		"the host's name": {"name": "ann", "code": sessionCode},
	} {
		if code, text := bob.call("POST", "/api/connect", in, nil); code != 400 {
			t.Errorf("connecting with %s: %d %s", what, code, text)
		}
	}
	if bob.state().SignedIn {
		t.Fatal("a failed connect signed bob in")
	}
	bob.ok("/api/connect", map[string]string{"name": "bob", "code": "  " + strings.ToLower(sessionCode) + "\n"}, nil)
	if s := bob.state(); !s.SignedIn || s.User != "bob" || s.Host.Running {
		t.Fatalf("bob after connecting: %+v", s)
	}
	// The project comes to bob by itself, into Documents\Henosis\<name>.
	bobFolder := filepath.Join(bob.home, "Documents", "Henosis", "Night Drive")
	eventually(t, "the project at bob's without any click", func() bool {
		data, _ := os.ReadFile(filepath.Join(bobFolder, "Night Drive.flp"))
		return string(data) == "v1"
	})
	eventually(t, "both listed as connected", func() bool {
		s := ann.state()
		return len(s.Host.Accounts) == 2 && len(s.Projects) == 1 && s.Projects[0].Shared
	})

	// Bob's own project reaches ann the same way. FL Studio's projects folder
	// is offered so adding it is one click.
	projects := filepath.Join(bob.home, "Documents", "Image-Line", "FL Studio", "Projects")
	os.MkdirAll(filepath.Join(projects, "bobs beat", "Backup"), 0o755)
	os.MkdirAll(filepath.Join(projects, "Templates", "Minimal"), 0o755)
	os.WriteFile(filepath.Join(projects, "bobs beat", "bobs beat.flp"), []byte("beat"), 0o644)
	os.WriteFile(filepath.Join(projects, "bobs beat", "Backup", "old.flp"), []byte("old"), 0o644)
	os.WriteFile(filepath.Join(projects, "Templates", "Minimal", "Minimal.flp"), []byte("tpl"), 0o644)
	os.WriteFile(filepath.Join(projects, "loose idea.flp"), []byte("idea"), 0o644)
	var found struct{ Projects []agent.Suggestion }
	if code, text := bob.call("GET", "/api/suggestions", nil, &found); code != 200 || len(found.Projects) != 2 {
		t.Fatalf("suggestions: %d %s", code, text)
	}
	byName := map[string]agent.Suggestion{}
	for _, s := range found.Projects {
		byName[s.Name] = s
	}
	if s := byName["bobs beat"]; s.Loose || s.Folder != filepath.Join(projects, "bobs beat") {
		t.Fatalf("suggestion for a project folder: %+v", s)
	}
	if s := byName["loose idea"]; !s.Loose || s.File != filepath.Join(projects, "loose idea.flp") {
		t.Fatalf("suggestion for a loose file: %+v", s)
	}
	bob.ok("/api/create", map[string]string{"folder": byName["bobs beat"].Folder}, nil)
	bob.ok("/api/create", map[string]string{"file": byName["loose idea"].File}, nil)
	eventually(t, "bob's projects at ann's", func() bool {
		a, _ := os.ReadFile(filepath.Join(ann.home, "Documents", "Henosis", "bobs beat", "bobs beat.flp"))
		b, _ := os.ReadFile(filepath.Join(ann.home, "Documents", "Henosis", "loose idea", "loose idea.flp"))
		return string(a) == "beat" && string(b) == "idea"
	})
	if _, err := os.Stat(filepath.Join(ann.home, "Documents", "Henosis", "bobs beat", "Backup")); err == nil {
		t.Fatal("FL Studio's backup folder was synced")
	}
	// The loose file was copied, not moved, and is not offered again.
	if data, _ := os.ReadFile(filepath.Join(projects, "loose idea.flp")); string(data) != "idea" {
		t.Fatal("the original loose file was touched")
	}
	if data, _ := os.ReadFile(filepath.Join(bob.home, "Documents", "Henosis", "loose idea", "loose idea.flp")); string(data) != "idea" {
		t.Fatal("the loose file was not copied into its own folder")
	}
	bob.call("GET", "/api/suggestions", nil, &found)
	if len(found.Projects) != 0 {
		t.Fatalf("already added projects are still suggested: %+v", found.Projects)
	}

	// History with a note for the next save, and comments from both sides.
	ann.ok("/api/projects/"+link.ProjectID+"/note", map[string]string{"text": "new bassline, check the drop"}, nil)
	if s := ann.state(); projectByID(s, link.ProjectID).NextNote != "new bassline, check the drop" {
		t.Fatalf("the note is not shown: %+v", projectByID(s, link.ProjectID))
	}
	os.WriteFile(filepath.Join(annFolder, "Night Drive.flp"), []byte("v2 with bass"), 0o644)
	type historyView struct {
		Versions []struct{ ID, Message, Author string }
		Comments []struct {
			SnapshotID string `json:"snapshot_id"`
			Author     string
			Text       string
		}
	}
	var history historyView
	eventually(t, "the save with its note in bob's history", func() bool {
		history = historyView{}
		bob.call("GET", "/api/projects/"+link.ProjectID+"/history", nil, &history)
		return len(history.Versions) == 2 && history.Versions[0].Message == "new bassline, check the drop"
	})
	if history.Versions[1].Message != "Added Night Drive.flp" || history.Versions[0].Author != "ann" {
		t.Fatalf("history: %+v", history.Versions)
	}
	eventually(t, "the note being used up", func() bool { return projectByID(ann.state(), link.ProjectID).NextNote == "" })
	os.WriteFile(filepath.Join(annFolder, "hat.wav"), []byte("hat"), 0o644)
	eventually(t, "a later save described by the app again", func() bool {
		history = historyView{}
		ann.call("GET", "/api/projects/"+link.ProjectID+"/history", nil, &history)
		return len(history.Versions) == 3 && history.Versions[0].Message == "Added hat.wav"
	})
	history.Versions = history.Versions[1:]
	bob.ok("/api/projects/"+link.ProjectID+"/comment", map[string]string{"snapshot_id": history.Versions[0].ID, "text": "drop is fire"}, nil)
	ann.ok("/api/projects/"+link.ProjectID+"/comment", map[string]string{"snapshot_id": history.Versions[0].ID, "text": "thanks, mixing next"}, nil)
	if code, _ := bob.call("POST", "/api/projects/"+link.ProjectID+"/comment", map[string]string{"snapshot_id": history.Versions[0].ID, "text": " "}, nil); code != 400 {
		t.Fatal("an empty comment was accepted")
	}
	history = historyView{}
	ann.call("GET", "/api/projects/"+link.ProjectID+"/history", nil, &history)
	if len(history.Comments) != 2 || history.Comments[0].Author != "bob" || history.Comments[0].Text != "drop is fire" || history.Comments[1].Author != "ann" {
		t.Fatalf("comments: %+v", history.Comments)
	}

	// The debug console shows what was pressed, what happened and what failed,
	// and never a token, an invite code or the session code.
	ann.call("POST", "/api/projects/"+link.ProjectID+"/restore", map[string]string{"snapshot_id": "nope"}, nil)
	ann.call("POST", "/api/ui-error", map[string]string{"text": "x is not defined (line 3)"}, nil)
	console, last := ann.logs(0)
	for _, want := range []string{
		"info Henosis " + agent.Version + " started",
		"info server: running on this computer",
		`info > host/start name="ann"`,
		`info > create folder=`,
		"info Night Drive: Uploaded your changes",
		"info Night Drive: Up to date.",
		"info bobs beat: new in the session, downloading to",
		"error host/start failed:",
		"error projects/" + link.ProjectID + "/restore failed:",
		"error window: x is not defined (line 3)",
		"debug server: - POST /v1/join -> true",
	} {
		if !strings.Contains(console, want) {
			t.Errorf("ann's console lacks %q", want)
		}
	}
	bobConsole, _ := bob.logs(0)
	for _, want := range []string{"info connect: found the session at http://127.0.0.1:", "error connect failed:", "info Night Drive: new in the session, downloading to"} {
		if !strings.Contains(bobConsole, want) {
			t.Errorf("bob's console lacks %q", want)
		}
	}
	if t.Failed() {
		t.Logf("ann's console:\n%s\nbob's console:\n%s", console, bobConsole)
	}
	for _, text := range []string{console, bobConsole} {
		if strings.Contains(text, "hn_") || strings.Contains(strings.ToUpper(text), sessionCode[:20]) {
			t.Error("a console shows a token or the session code")
		}
	}
	if more, _ := ann.logs(last); strings.Contains(more, "started") {
		t.Error("asking for newer lines returned old ones")
	}

	// Taking a project off this computer is respected: it is not downloaded again.
	bob.ok("/api/projects/"+link.ProjectID+"/unlink", nil, nil)
	time.Sleep(400 * time.Millisecond)
	if s := bob.state(); len(s.Unlinked) != 1 || s.Unlinked[0].Name != "Night Drive" {
		t.Fatalf("after unlinking: %d linked, unlinked %+v", len(s.Projects), s.Unlinked)
	}
	bob.ok("/api/link", map[string]string{"project_id": link.ProjectID, "folder": bobFolder}, nil)

	// A new code locks out new friends; bob stays connected. An extra address rides along in the code.
	ann.ok("/api/host/new-code", nil, nil)
	ann.ok("/api/host/extra", map[string]string{"text": "vadym.example.com"}, nil)
	fresh := ann.state().Host
	if fresh.Code == sessionCode || fresh.Extra != "vadym.example.com" {
		t.Fatalf("after a new code: %+v", fresh)
	}
	if addresses, _, err := agent.DecodeCode(fresh.Code); err != nil || addresses[0] != "http://vadym.example.com:"+strconv.Itoa(fresh.Port) {
		t.Fatalf("the code's addresses: %v %v", addresses, err)
	}
	carl := newDesk(t, "carl")
	if code, text := carl.call("POST", "/api/connect", map[string]string{"name": "carl", "code": sessionCode}, nil); code != 400 || !strings.Contains(text, "not valid any more") {
		t.Fatalf("connecting with the old code: %d %s", code, text)
	}
	ann.ok("/api/host/extra", map[string]string{"text": ""}, nil)
	carl.ok("/api/connect", map[string]string{"name": "carl", "code": ann.state().Host.Code}, nil)
	eventually(t, "carl getting all three projects", func() bool { return len(carl.state().Projects) == 3 })

	// Deleting a project from the session: the files stay on both computers,
	// and the other member is told it is gone.
	loose := filepath.Join(bob.home, "Documents", "Henosis", "loose idea")
	var looseID string
	for _, p := range bob.state().Projects {
		if p.Name == "loose idea" {
			looseID = p.ProjectID
		}
	}
	if looseID == "" {
		t.Fatal("bob does not have the loose project")
	}
	if code, text := ann.call("POST", "/api/projects/"+looseID+"/delete", map[string]string{}, nil); code != 400 || !strings.Contains(text, "only the member who added") {
		t.Fatalf("a member deleting someone else's project: %d %s", code, text)
	}
	bob.ok("/api/projects/"+looseID+"/delete", nil, nil)
	if s := bob.state(); len(s.Unlinked) != 0 {
		t.Fatalf("the deleted project is still offered: %+v", s.Unlinked)
	}
	if _, err := os.Stat(filepath.Join(loose, "loose idea.flp")); err != nil {
		t.Fatal("deleting from the session deleted bob's files")
	}
	eventually(t, "ann being told the project is gone", func() bool {
		for _, p := range ann.state().Projects {
			if p.Name == "loose idea" {
				return p.State == "problem" && strings.Contains(p.Detail, "no longer available") || p.Headline == "No longer available"
			}
		}
		return false
	})
	if _, err := os.Stat(filepath.Join(ann.home, "Documents", "Henosis", "loose idea", "loose idea.flp")); err != nil {
		t.Fatal("ann's copy was deleted")
	}

	// The session stops: bob sees it. It starts again on the same port with everything kept.
	ann.ok("/api/host/stop", nil, nil)
	eventually(t, "bob going offline", func() bool {
		for _, p := range bob.state().Projects {
			if p.State != "offline" {
				return false
			}
		}
		return true
	})
	os.WriteFile(filepath.Join(annFolder, "kick.wav"), []byte("kick"), 0o644)
	ann.ok("/api/host/start", map[string]any{}, nil)
	if again := ann.state().Host; again.Port != st.Host.Port || len(again.Accounts) != 3 {
		t.Fatalf("hosting again: %+v", again)
	}
	eventually(t, "sync after the session came back", func() bool {
		data, _ := os.ReadFile(filepath.Join(bobFolder, "kick.wav"))
		return string(data) == "kick"
	})
	saved, _ := config.Load(filepath.Join(ann.home, "config.json"))
	if !saved.Host.Enabled || saved.Host.Port != st.Host.Port || !saved.AutoLink {
		t.Fatalf("saved choices: %+v", saved)
	}

	// Nobody answering at the code's address is explained, not just "failed".
	ann.ok("/api/host/stop", nil, nil)
	dave := newDesk(t, "dave")
	if code, text := dave.call("POST", "/api/connect", map[string]string{"name": "dave", "code": fresh.Code}, nil); code != 400 || !strings.Contains(text, "could not reach your friend") {
		t.Fatalf("connecting to a session that is not running: %d %s", code, text)
	}
}

func projectByID(s agent.AppState, id string) agent.Status {
	for _, p := range s.Projects {
		if p.ProjectID == id {
			return p
		}
	}
	return agent.Status{}
}

// TestHostKeepsTheKnownPortAndTheHostsAccount covers what a second run on a
// computer that has hosted before must do.
func TestHostKeepsTheKnownPortAndTheHostsAccount(t *testing.T) {
	d := newDesk(t, "ann")
	d.ok("/api/host/start", map[string]any{"name": "ann"}, nil)
	first := d.state().Host
	if first.Port != config.DefaultServerPort {
		t.Fatalf("a session started on port %d, not %d", first.Port, config.DefaultServerPort)
	}
	// The same name again, after signing out: the account comes back rather
	// than being refused, since this computer holds the session's data.
	d.ok("/api/logout", nil, nil)
	d.ok("/api/host/start", map[string]any{"name": "ann"}, nil)
	if s := d.state(); !s.SignedIn || s.User != "ann" || len(s.Host.Accounts) != 1 {
		t.Fatalf("signing back in as the host: %+v", s)
	}
	// A friend's name, taken, is still refused.
	var made struct{ Token string }
	d.ok("/api/host/account", map[string]string{"name": "bob"}, &made)
	if code, text := d.call("POST", "/api/host/account", map[string]string{"name": "bob"}, nil); code != 400 || !strings.Contains(text, "taken") {
		t.Fatalf("a second account for a friend's name: %d %s", code, text)
	}
	// A port the system handed out is not kept across restarts.
	cfg, _ := config.Load(filepath.Join(d.home, "config.json"))
	cfg.Host.Port = 50934
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	d.page.Close() // free the port, as quitting the app would
	d.a.Close()
	again := newDeskWithHome(t, d.home)
	if port := again.state().Host.Port; port != config.DefaultServerPort {
		t.Fatalf("after a restart the session is on port %d, not %d", port, config.DefaultServerPort)
	}
}

// TestConnectThroughTheRelay is the "no Tailscale" case: the session code's
// addresses cannot be reached from the friend's computer, so the friend's
// app pairs with the host through the relay, syncs, and keeps working after
// a restart.
func TestConnectThroughTheRelay(t *testing.T) {
	relaySrv := relaytest.NewServer()
	defer relaySrv.Close()
	ann, bob := newDesk(t, "ann"), newDesk(t, "bob")
	// The code will carry an address nobody can reach (TEST-NET-1).
	ann.a.LocalIPs = func() []string { return []string{"192.0.2.1"} }

	for _, bad := range []string{"https://henosis-relay.<your-account>.workers.dev", "https://", "https://relay", "workers.dev"} {
		if code, _ := ann.call("POST", "/api/host/relay", map[string]string{"text": bad}, nil); code != 400 {
			t.Errorf("relay %q was accepted", bad)
		}
	}
	ann.ok("/api/host/relay", map[string]string{"text": relaySrv.URL}, nil)
	ann.ok("/api/host/start", map[string]any{"name": "ann", "port": 0}, nil)
	st := ann.state()
	if st.Host.Relay != relaySrv.URL {
		t.Fatalf("the host's relay: %q", st.Host.Relay)
	}
	annFolder := filepath.Join(ann.home, "Beats", "Far Away")
	os.MkdirAll(annFolder, 0o755)
	os.WriteFile(filepath.Join(annFolder, "Far Away.flp"), []byte("v1"), 0o644)
	ann.ok("/api/create", map[string]string{"folder": annFolder}, nil)

	// Without a relay, bob cannot get in, and is told about the relay.
	if code, text := bob.call("POST", "/api/connect", map[string]string{"name": "bob", "code": st.Host.Code}, nil); code != 400 || !strings.Contains(text, "relay") {
		t.Fatalf("connecting without a relay: %d %s", code, text)
	}
	// With the relay, the same code works.
	bob.ok("/api/host/relay", map[string]string{"text": relaySrv.URL}, nil)
	bob.ok("/api/connect", map[string]string{"name": "bob", "code": st.Host.Code}, nil)
	if s := bob.state(); !s.SignedIn || s.User != "bob" || !strings.HasPrefix(s.Server, "relay://") {
		t.Fatalf("bob after connecting through the relay: signed in %v as %q at %q", s.SignedIn, s.User, s.Server)
	}
	bobFile := filepath.Join(bob.home, "Documents", "Henosis", "Far Away", "Far Away.flp")
	defer func() {
		if t.Failed() {
			text, _ := bob.logs(0)
			t.Logf("bob's log:\n%s", text)
		}
	}()
	eventually(t, "the project at bob's through the relay", func() bool {
		data, _ := os.ReadFile(bobFile)
		return string(data) == "v1"
	})
	// Bob's change goes back to ann the same way.
	os.WriteFile(bobFile, []byte("v2 from bob"), 0o644)
	eventually(t, "bob's save at ann's through the relay", func() bool {
		data, _ := os.ReadFile(filepath.Join(annFolder, "Far Away.flp"))
		return string(data) == "v2 from bob"
	})

	// Bob's app restarts: it dials through the relay again on its own.
	bob.page.Close()
	bob.a.Close()
	bob2 := newDeskWithHome(t, bob.home)
	if s := bob2.state(); !s.SignedIn || s.User != "bob" {
		t.Fatalf("bob after a restart: %+v", s)
	}
	os.WriteFile(filepath.Join(annFolder, "Far Away.flp"), []byte("v3 from ann"), 0o644)
	eventually(t, "ann's save at bob's after his restart", func() bool {
		data, _ := os.ReadFile(bobFile)
		return string(data) == "v3 from ann"
	})
}

// TestStartANewSession: leaving and pressing Start used to reopen the old
// session silently, adding an account per new name. The start screen now
// shows the session this computer holds, and a new one can be started.
func TestStartANewSession(t *testing.T) {
	ann, bob := newDesk(t, "ann"), newDesk(t, "bob")
	if ann.state().Previous != nil {
		t.Fatal("a fresh computer claims to hold a session")
	}
	ann.ok("/api/host/start", map[string]any{"name": "ann", "port": 0}, nil)
	oldCode := ann.state().Host.Code
	folder := filepath.Join(ann.home, "Beats", "Old Song")
	os.MkdirAll(folder, 0o755)
	os.WriteFile(filepath.Join(folder, "Old Song.flp"), []byte("old"), 0o644)
	ann.ok("/api/create", map[string]string{"folder": folder}, nil)
	bob.ok("/api/connect", map[string]string{"name": "bob", "code": oldCode}, nil)

	ann.ok("/api/logout", nil, nil)
	prev := ann.state().Previous
	if prev == nil || prev.Projects != 1 || strings.Join(prev.People, ",") != "ann,bob" && strings.Join(prev.People, ",") != "bob,ann" {
		t.Fatalf("the start screen's session: %+v", prev)
	}

	ann.ok("/api/host/start-new", map[string]any{"name": "ann"}, nil)
	st := ann.state()
	if !st.SignedIn || st.User != "ann" || len(st.Host.Accounts) != 1 || len(st.Projects) != 0 || st.Previous != nil {
		t.Fatalf("after starting a new session: signed in %v as %q, accounts %v, %d projects", st.SignedIn, st.User, st.Host.Accounts, len(st.Projects))
	}
	if st.Host.Code == oldCode {
		t.Fatal("the new session kept the old code")
	}
	if data, _ := os.ReadFile(filepath.Join(folder, "Old Song.flp")); string(data) != "old" {
		t.Fatal("the project's files were touched")
	}
	dirs, _ := filepath.Glob(filepath.Join(ann.home, "server-data-*"))
	if len(dirs) != 1 {
		t.Fatalf("the old session was not set aside: %v", dirs)
	}
	carl := newDesk(t, "carl")
	if code, text := carl.call("POST", "/api/connect", map[string]string{"name": "carl", "code": oldCode}, nil); code != 400 {
		t.Fatalf("the old code still lets people in: %d %s", code, text)
	}
	carl.ok("/api/connect", map[string]string{"name": "carl", "code": st.Host.Code}, nil)

	// Starting new while signed in is refused rather than pulling the rug.
	if code, _ := ann.call("POST", "/api/host/start-new", map[string]any{"name": "ann"}, nil); code != 400 {
		t.Fatal("start-new worked while signed in")
	}
}

// TestJoiningStartsClean: projects from a session you were in before are
// not carried into the next one as "No longer available" cards. Their
// files stay where they are.
func TestJoiningStartsClean(t *testing.T) {
	ann, bob := newDesk(t, "ann"), newDesk(t, "bob")
	// Bob had a session of his own, with a project.
	bob.ok("/api/host/start", map[string]any{"name": "bob", "port": 0}, nil)
	old := filepath.Join(bob.home, "Beats", "Roskirt")
	os.MkdirAll(old, 0o755)
	os.WriteFile(filepath.Join(old, "Roskirt.flp"), []byte("mine"), 0o644)
	bob.ok("/api/create", map[string]string{"folder": old}, nil)
	bob.ok("/api/host/stop", nil, nil)
	bob.ok("/api/logout", nil, nil)

	// Now he joins Ann's.
	ann.ok("/api/host/start", map[string]any{"name": "ann", "port": 0}, nil)
	song := filepath.Join(ann.home, "Beats", "Sacred")
	os.MkdirAll(song, 0o755)
	os.WriteFile(filepath.Join(song, "Sacred.flp"), []byte("v1"), 0o644)
	ann.ok("/api/create", map[string]string{"folder": song}, nil)
	bob.ok("/api/connect", map[string]string{"name": "bob", "code": ann.state().Host.Code}, nil)
	eventually(t, "ann's project at bob's", func() bool {
		s := bob.state()
		return len(s.Projects) == 1 && s.Projects[0].Name == "Sacred"
	})
	for _, p := range bob.state().Projects {
		if p.Name == "Roskirt" {
			t.Fatal("the project from bob's own session followed him in")
		}
	}
	if data, _ := os.ReadFile(filepath.Join(old, "Roskirt.flp")); string(data) != "mine" {
		t.Fatal("the old project's files were touched")
	}
}
