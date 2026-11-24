package agent_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"henosis/agent"
	"henosis/agent/relay/relaytest"
	"henosis/config"
)

func TestMain(m *testing.M) {
	// Tests never ask GitHub for releases; the update tests point the check
	// at a fake.
	agent.ReleasesURL = ""
	// ... nor use the real relay; relay tests bring their own.
	config.DefaultRelay = ""
	os.Exit(m.Run())
}

func fakeReleases(t *testing.T, tag, url string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			http.Error(w, "GitHub wants a User-Agent", 403)
			return
		}
		w.Write([]byte(`{"tag_name":"` + tag + `","html_url":"` + url + `","draft":false,"prerelease":false}`))
	}))
	oldURL, oldDelay := agent.ReleasesURL, agent.UpdateCheckDelay
	agent.ReleasesURL, agent.UpdateCheckDelay = srv.URL, 10*time.Millisecond
	t.Cleanup(func() { agent.ReleasesURL, agent.UpdateCheckDelay = oldURL, oldDelay; srv.Close() })
}

func stateUpdate(t *testing.T, d *desk, wait time.Duration) *agent.UpdateInfo {
	t.Helper()
	deadline := time.Now().Add(wait)
	for {
		var st agent.AppState
		d.call("GET", "/api/state", nil, &st)
		if st.Update != nil || time.Now().After(deadline) {
			if st.Repo == "" {
				t.Error("the state does not carry the project page for Report a problem")
			}
			return st.Update
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestANewerReleaseIsShown(t *testing.T) {
	oldV := agent.Version
	agent.Version = "0.8.0"
	defer func() { agent.Version = oldV }()
	fakeReleases(t, "v0.10.1", agent.Repo+"/releases/tag/v0.10.1")
	d := newDesk(t, "ann")
	up := stateUpdate(t, d, 3*time.Second)
	if up == nil || up.Version != "0.10.1" || up.URL != agent.Repo+"/releases/tag/v0.10.1" {
		t.Fatalf("update = %+v, want 0.10.1 with its page", up)
	}
}

func TestNoUpdateWhenUpToDateOrOlder(t *testing.T) {
	oldV := agent.Version
	agent.Version = "0.8.0"
	defer func() { agent.Version = oldV }()
	for _, tag := range []string{"v0.8.0", "v0.7.9", "nightly", "v1.0"} {
		t.Run(tag, func(t *testing.T) {
			fakeReleases(t, tag, agent.Repo+"/releases/tag/"+tag)
			d := newDesk(t, "ann")
			if up := stateUpdate(t, d, 300*time.Millisecond); up != nil {
				t.Fatalf("offered %+v while running 0.8.0", up)
			}
		})
	}
}

// A release page that points anywhere but this project is not trusted.
func TestUpdateLinkStaysOnTheProject(t *testing.T) {
	oldV := agent.Version
	agent.Version = "0.8.0"
	defer func() { agent.Version = oldV }()
	fakeReleases(t, "v0.9.0", "https://evil.example/henosis.exe")
	d := newDesk(t, "ann")
	up := stateUpdate(t, d, 3*time.Second)
	if up == nil || up.URL != agent.Repo+"/releases/latest" {
		t.Fatalf("update = %+v, want the project's own releases page", up)
	}
}

// When the relay is full or the host is not there, the friend is told which.
func TestJoiningSaysWhyTheRelayFailed(t *testing.T) {
	full := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte("error code: 1027"))
	}))
	defer full.Close()
	empty := relaytest.NewServer()
	defer empty.Close()

	ann, bob := newDesk(t, "ann"), newDesk(t, "bob")
	ann.a.LocalIPs = func() []string { return []string{"192.0.2.1"} } // unreachable
	ann.ok("/api/host/start", map[string]any{"name": "ann", "port": 0}, nil)
	code := ann.state().Host.Code

	bob.ok("/api/host/relay", map[string]string{"text": full.URL}, nil)
	if status, text := bob.call("POST", "/api/connect", map[string]string{"name": "bob", "code": code}, nil); status != 400 || !strings.Contains(text, "allowance") {
		t.Errorf("with a full relay: %d %s", status, text)
	}
	bob.ok("/api/host/relay", map[string]string{"text": empty.URL}, nil)
	if status, text := bob.call("POST", "/api/connect", map[string]string{"name": "bob", "code": code}, nil); status != 400 || !strings.Contains(text, "open on your friend's computer") || !strings.Contains(text, agent.Version) {
		t.Errorf("with nobody at the relay: %d %s", status, text)
	}
}

// HENOSIS_FORCE_RELAY makes a friend on the same network go through the
// relay, the way a friend far away would.
func TestForceRelay(t *testing.T) {
	relaySrv := relaytest.NewServer()
	defer relaySrv.Close()
	ann, bob := newDesk(t, "ann"), newDesk(t, "bob")
	ann.ok("/api/host/relay", map[string]string{"text": relaySrv.URL}, nil)
	ann.ok("/api/host/start", map[string]any{"name": "ann", "port": 0}, nil)
	bob.ok("/api/host/relay", map[string]string{"text": relaySrv.URL}, nil)
	t.Setenv("HENOSIS_FORCE_RELAY", "1")
	bob.ok("/api/connect", map[string]string{"name": "bob", "code": ann.state().Host.Code}, nil)
	if s := bob.state(); !strings.HasPrefix(s.Server, "relay://") {
		t.Fatalf("bob reached %q directly although the relay was forced", s.Server)
	}
}

// Report a problem and Download open in the default browser through the
// app, and only the project's own pages can be opened that way.
func TestProjectLinksOpenInTheBrowser(t *testing.T) {
	d := newDesk(t, "ann")
	var opened []string
	d.a.OpenURL = func(url string) error { opened = append(opened, url); return nil }
	report := agent.Repo + "/issues/new?body=What%20happened%3A"
	d.ok("/api/open-link", map[string]string{"text": report}, nil)
	for _, bad := range []string{"https://evil.example/", agent.Repo + "evil.example/", "file:///C:/Windows/System32/calc.exe", agent.Repo + "/x\" & calc"} {
		if code, _ := d.call("POST", "/api/open-link", map[string]string{"text": bad}, nil); code != 400 {
			t.Errorf("%q was opened", bad)
		}
	}
	if len(opened) != 1 || opened[0] != report {
		t.Fatalf("opened %q", opened)
	}
}

// A server that has gone slow (a friend's computer that left, reached
// through the relay) must not hold up the status page, nor a newer copy of
// the app asking which version runs.
func TestStatusPageDoesNotWaitForASlowServer(t *testing.T) {
	var slow atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/me"):
			w.Write([]byte(`{"name":"ann"}`))
		case strings.HasSuffix(r.URL.Path, "/projects"):
			if slow.Load() {
				time.Sleep(6 * time.Second)
			}
			w.Write([]byte(`{"projects":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	d := newDesk(t, "ann")
	d.ok("/api/login", map[string]string{"server": srv.URL, "token": "hn_test"}, nil)
	d.state()
	slow.Store(true)
	time.Sleep(5100 * time.Millisecond) // the cached list is now stale
	start := time.Now()
	d.state()
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("the status page took %v with a slow server", took)
	}
	start = time.Now()
	var v struct{ Version string }
	d.call("GET", "/api/version", nil, &v)
	if v.Version != agent.Version || time.Since(start) > time.Second {
		t.Fatalf("/api/version: %+v after %v", v, time.Since(start))
	}
}
