package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"henosis/agent/relay"
	"henosis/client"
	"henosis/config"
	"henosis/core"
	"henosis/server"
	"henosis/store"
)

// hosting is the server running inside the app, when this computer hosts.
type hosting struct {
	st        *store.Store
	srv       *http.Server
	port      int
	problem   string
	stop      chan struct{}
	relayStop context.CancelFunc // cancels the relay acceptor, if running
	relayURL  string             // the relay the acceptor is talking to
}

// HostState is what the status page shows about the built-in server.
type HostState struct {
	Running   bool     `json:"running"`
	Port      int      `json:"port"`
	Addresses []string `json:"addresses"` // what a partner types as the server address
	Accounts  []string `json:"accounts"`
	DataDir   string   `json:"data_dir"`
	Problem   string   `json:"problem,omitempty"`
	Code      string   `json:"code"`  // the session code to give to a friend
	Extra     string   `json:"extra"` // an address the host added by hand
	Relay     string   `json:"relay"` // the relay URL, if one is set
}

func (a *Agent) hostDir() string { return filepath.Join(a.cfg.Dir(), "server-data") }

// startHosting runs the server inside the app. Port 0 picks a free port.
// The caller holds a.mu.
func (a *Agent) startHosting(port int) error {
	if a.host.srv != nil {
		return nil
	}
	if port < 0 || port > 65535 {
		return errors.New("the port must be between 1 and 65535")
	}
	// 8787 is the port the instructions and the firewall rule talk about. A
	// port the system handed out once (49152 and up) was never a choice, so
	// it is not kept.
	if port == 0 || port >= 49152 {
		port = config.DefaultServerPort
	}
	st, err := store.Open(a.hostDir())
	if err != nil {
		a.host.problem = err.Error()
		return err
	}
	// A session is a group of friends: everyone sees every project, and
	// whoever has the session code may join.
	if a.cfg.Host.JoinSecret == "" {
		raw := make([]byte, 16)
		if _, err := rand.Read(raw); err != nil {
			return err
		}
		a.cfg.Host.JoinSecret = hex.EncodeToString(raw)
	}
	if err := st.SetShareAll(true); err != nil {
		return err
	}
	if err := st.SetJoinSecret(a.cfg.Host.JoinSecret); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil && port == config.DefaultServerPort {
		// Something else has 8787. Any port works; the code carries it.
		listener, err = net.Listen("tcp", ":0")
	}
	if err != nil {
		a.host.problem = fmt.Sprintf("port %d is in use by another program", port)
		return errors.New(a.host.problem)
	}
	api := server.New(st)
	api.Log = func(format string, args ...any) { a.logf("debug", "server: "+format, args...) }
	a.host = hosting{st: st, port: listener.Addr().(*net.TCPAddr).Port, stop: make(chan struct{}),
		srv: &http.Server{Handler: api, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}}
	go func(h hosting) { _ = h.srv.Serve(listener) }(a.host)
	go func(h hosting) { // old versions are deleted at start and then every hour
		for {
			if snapshots, blobs := h.st.Cleanup(); snapshots+blobs > 0 {
				a.logf("info", "server: deleted %d expired versions and %d unused files", snapshots, blobs)
			}
			select {
			case <-h.stop:
				return
			case <-time.After(time.Hour):
			}
		}
	}(a.host)
	a.logf("info", "server: running on this computer, port %d, data in %s", a.host.port, a.hostDir())
	// If a relay is configured, keep a small pool of accept sockets open to
	// it so a friend's Hénōsis can reach us when no direct address works.
	if url := a.cfg.RelayURL(); url != "" {
		relayCtx, cancel := context.WithCancel(context.Background())
		a.host.relayStop = cancel
		a.host.relayURL = url
		acc := &relay.HostAcceptor{
			RelayURL:  url,
			Code:      a.cfg.Host.JoinSecret,
			LocalAddr: "127.0.0.1:" + strconv.Itoa(a.host.port),
			Logf:      func(f string, args ...any) { a.logf("debug", f, args...) },
		}
		go acc.Run(relayCtx)
		a.logf("info", "relay: on, pairing through %s", url)
	}
	return nil
}

// stopHosting stops the built-in server. The caller holds a.mu.
func (a *Agent) stopHosting() {
	if a.host.srv == nil {
		return
	}
	if a.host.relayStop != nil {
		a.host.relayStop()
	}
	close(a.host.stop)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = a.host.srv.Shutdown(ctx)
	_ = a.host.srv.Close()
	a.host = hosting{}
	a.logf("info", "server: stopped")
}

func (a *Agent) hostState() HostState {
	st := HostState{Running: a.host.srv != nil, Port: a.host.port, DataDir: a.hostDir(), Problem: a.host.problem,
		Addresses: []string{}, Accounts: []string{}}
	if !st.Running {
		return st
	}
	for _, user := range a.host.st.Users() {
		st.Accounts = append(st.Accounts, user.Name)
	}
	ips := a.localIPs()
	for _, ip := range ips {
		st.Addresses = append(st.Addresses, "http://"+ip+":"+strconv.Itoa(st.Port))
	}
	st.Extra = a.cfg.Host.Extra
	st.Relay = a.cfg.RelayURL()
	if code, err := EncodeCode(ips, st.Port, st.Extra, a.cfg.Host.JoinSecret); err == nil {
		st.Code = code
	}
	return st
}

// localIPs lists this computer's IPv4 addresses on its networks (Wi-Fi,
// cable, Tailscale and the like). Tests replace it through LocalIPs.
func (a *Agent) localIPs() []string {
	if a.LocalIPs != nil {
		return a.LocalIPs()
	}
	var ips []string
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok && ipnet.IP.To4() != nil && !ipnet.IP.IsLoopback() && !ipnet.IP.IsLinkLocalUnicast() {
				ips = append(ips, ipnet.IP.String())
			}
		}
	}
	return ips
}

// HostNewCode makes a new session code. The old one stops working for new
// friends; everyone already connected stays connected.
func (a *Agent) HostNewCode() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.host.srv == nil {
		return errors.New("this computer is not hosting a session")
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	a.cfg.Host.JoinSecret = hex.EncodeToString(raw)
	if err := a.host.st.SetJoinSecret(a.cfg.Host.JoinSecret); err != nil {
		return err
	}
	return a.cfg.Save()
}

// HostSetExtra adds an address to the session code by hand, for example a
// public IP or a domain when port 8787 is forwarded. "" removes it.
func (a *Agent) HostSetExtra(address string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	address = strings.TrimSpace(address)
	if len(address) > 200 || strings.ContainsAny(address, " \t\n") {
		return errors.New("that is not an address")
	}
	a.cfg.Host.Extra = address
	return a.cfg.Save()
}

// SetRelay stores the relay URL in the config. An empty string turns the
// relay off. If hosting is running, the acceptor is restarted so the change
// takes effect right away.
func (a *Agent) SetRelay(url string) error {
	url = strings.TrimSpace(url)
	// Light validation: must start with https:// (or http:// for local dev).
	if url != "" && !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://") {
		return errors.New("the relay URL should start with https://")
	}
	if url != "" {
		u, err := neturl.Parse(url)
		host := ""
		if err == nil {
			host = u.Hostname()
		}
		if host == "" || strings.ContainsAny(host, "<>{} ") || (!strings.Contains(host, ".") && host != "localhost") {
			return errors.New("that is not a full relay address; paste the one wrangler deploy printed, like https://henosis-relay.yourname.workers.dev")
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cfg.Relay = url
	if err := a.cfg.Save(); err != nil {
		return err
	}
	// If hosting is up and the URL changed, restart the relay acceptor.
	if a.host.srv != nil && a.host.relayURL != a.cfg.RelayURL() {
		if a.host.relayStop != nil {
			a.host.relayStop()
			a.host.relayStop = nil
			a.host.relayURL = ""
		}
		if newURL := a.cfg.RelayURL(); newURL != "" {
			relayCtx, cancel := context.WithCancel(context.Background())
			a.host.relayStop = cancel
			a.host.relayURL = newURL
			acc := &relay.HostAcceptor{
				RelayURL:  newURL,
				Code:      a.cfg.Host.JoinSecret,
				LocalAddr: "127.0.0.1:" + strconv.Itoa(a.host.port),
				Logf:      func(f string, args ...any) { a.logf("debug", f, args...) },
			}
			go acc.Run(relayCtx)
			a.logf("info", "relay: on, pairing through %s", newURL)
		} else {
			a.logf("info", "relay: off")
		}
	}
	return nil
}

// Connect joins a friend's session: it finds their computer through the
// session code, creates an account with the given name and signs in.
func (a *Agent) Connect(name, code string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("type your name first")
	}
	addresses, secret, err := DecodeCode(code)
	if err != nil {
		return err
	}
	a.mu.Lock()
	if a.cfg.SignedIn() && len(a.cfg.Links) > 0 {
		a.mu.Unlock()
		return errors.New("this computer is already in a session; sign out first")
	}
	a.mu.Unlock()

	// HENOSIS_FORCE_RELAY=1 skips the direct addresses, to test the relay or
	// to get round a network that answers but misbehaves.
	tried := addresses
	if os.Getenv("HENOSIS_FORCE_RELAY") == "1" {
		a.logf("info", "connect: HENOSIS_FORCE_RELAY is set; going straight to the relay")
		addresses = nil
	}

	// Try every direct address at once; the first that answers wins.
	found := make(chan string, len(addresses))
	for _, address := range addresses {
		go func(address string) {
			if client.Reachable(address, 4*time.Second) {
				found <- address
			} else {
				a.logf("debug", "connect: no answer from %s", address)
				found <- ""
			}
		}(address)
	}
	server := ""
	for range addresses {
		if server = <-found; server != "" {
			break
		}
	}
	// No direct address worked — try the relay, if one is configured.
	var relayClient *http.Client
	var relayErr error
	if server == "" {
		if url := a.cfg.RelayURL(); url != "" {
			a.logf("info", "connect: no direct address worked; trying the relay")
			relayErr = probeRelay(url, secret)
			rc := relay.NewHTTPClient(url, secret)
			if relayErr == nil && client.ReachableWithClient("http://relay", rc) {
				server = "http://relay"
				relayClient = rc
			} else {
				a.logf("debug", "connect: the relay did not pair: %v", relayErr)
			}
		}
	}
	if server == "" {
		hint := "Is Henosis open there, and are you both on the same network or both on Tailscale?"
		switch {
		case a.cfg.RelayURL() == "":
			hint += " You can also set a relay URL under Advanced to connect from anywhere."
		case errors.Is(relayErr, relay.ErrRelayBusy):
			hint = "The relay: " + relayErr.Error() + "."
		case errors.Is(relayErr, relay.ErrOtherVersion):
			hint = fmt.Sprintf("Your friend's computer runs a different version of Hénōsis (this one is %s). Update both to the newest version from %s/releases/latest.", Version, Repo)
		default:
			hint = fmt.Sprintf("Is Hénōsis open on your friend's computer, and is it version %s like this one?", Version)
		}
		return fmt.Errorf("could not reach your friend's computer (tried %s). %s", strings.Join(tried, ", "), hint)
	}
	if relayClient != nil {
		a.logf("info", "connect: paired through the relay")
	} else {
		a.logf("info", "connect: found the session at %s", server)
	}
	var token string
	if relayClient != nil {
		_, token, err = client.JoinWithClient(server, secret, name, relayClient)
		relayClient.CloseIdleConnections() // the session gets its own client
	} else {
		_, token, err = client.Join(server, secret, name)
	}
	// If we went through the relay, store a sentinel server URL so later
	// sessions re-dial through the relay with the same secret.
	if relayClient != nil {
		server = "relay://" + secret
	}
	switch {
	case errors.Is(err, core.ErrNameTaken):
		return fmt.Errorf("someone in this session is already called %q; pick another name", name)
	case errors.Is(err, core.ErrJoinCode):
		return errors.New("this session code is not valid any more; ask your friend for the current one")
	case err != nil:
		return errors.New(strings.TrimPrefix(err.Error(), "bad request: "))
	}
	if err := a.Login(server, token); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cfg.AutoLink = true
	return a.cfg.Save()
}

// HostStart makes this computer the server and remembers that for the next
// start. With a name it also creates that account and signs this computer in,
// which is everything a first run needs.
func (a *Agent) HostStart(port int, name string) (token string, err error) {
	a.mu.Lock()
	if port == 0 && a.cfg.Host.Port != 0 {
		port = a.cfg.Host.Port
	}
	if err := a.startHosting(port); err != nil {
		a.mu.Unlock()
		return "", err
	}
	a.cfg.Host.Enabled, a.cfg.Host.Port = true, a.host.port
	a.cfg.AutoLink = true
	if err := a.cfg.Save(); err != nil {
		a.mu.Unlock()
		return "", err
	}
	local := "http://127.0.0.1:" + strconv.Itoa(a.host.port)
	a.mu.Unlock()
	if strings.TrimSpace(name) == "" {
		return "", nil
	}
	token, err = a.HostAddAccount(name)
	if errors.Is(err, core.ErrNameTaken) {
		// This computer holds the session's data, so an account of that name
		// here is the user's own: hand it back with a fresh token.
		a.mu.Lock()
		_, token, err = a.host.st.ReissueToken(name)
		a.mu.Unlock()
		if err == nil {
			a.logf("info", "server: signed back in as %q", strings.TrimSpace(name))
		}
	}
	if err != nil {
		return "", err
	}
	return token, a.Login(local, token)
}

// PreviousSession is a session this computer holds: who is in it and how
// many projects it has.
type PreviousSession struct {
	People   []string `json:"people"`
	Projects int      `json:"projects"`
}

// previousSession describes the session held in dir, or in running when the
// server is up. Nil when there is none.
func previousSession(running *store.Store, dir string) *PreviousSession {
	st := running
	if st == nil {
		if _, err := os.Stat(filepath.Join(dir, "users.json")); err != nil {
			return nil
		}
		opened, err := store.Open(dir)
		if err != nil {
			return nil
		}
		st = opened
	}
	users := st.Users()
	if len(users) == 0 {
		return nil
	}
	ps := &PreviousSession{People: []string{}}
	for _, u := range users {
		ps.People = append(ps.People, u.Name)
	}
	ps.Projects = len(st.Projects(users[0].ID)) // everyone sees every project
	return ps
}

// HostStartNew starts an empty session. The old session's data is moved aside
// to a dated folder next to it, not deleted; the project folders on this
// computer are not touched, only unlinked from the old session. Friends need
// the new code.
func (a *Agent) HostStartNew(port int, name string) (string, error) {
	a.mu.Lock()
	if a.cfg.SignedIn() {
		a.mu.Unlock()
		return "", errors.New("leave the session first")
	}
	a.stopHosting()
	dir := a.hostDir()
	if _, err := os.Stat(dir); err == nil {
		aside := dir + "-" + time.Now().Format("2006-01-02-150405")
		if err := os.Rename(dir, aside); err != nil {
			a.mu.Unlock()
			return "", fmt.Errorf("could not set the old session aside: %w", err)
		}
		a.logf("info", "server: the previous session was set aside in %s", aside)
	}
	for id := range a.runners {
		a.stopRunner(id)
	}
	a.cfg.Host.JoinSecret = "" // a new session gets a new code
	a.cfg.Links, a.cfg.Skipped = nil, nil
	if err := a.cfg.Save(); err != nil {
		a.mu.Unlock()
		return "", err
	}
	a.mu.Unlock()
	return a.HostStart(port, name)
}

// HostStop stops being the server. The stored projects stay on disk.
func (a *Agent) HostStop() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stopHosting()
	a.cfg.Host.Enabled = false
	return a.cfg.Save()
}

// HostAddAccount creates an account on the built-in server and returns its
// token. The token is shown once; only its hash is stored.
func (a *Agent) HostAddAccount(name string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.host.srv == nil {
		return "", errors.New("this computer is not running the server")
	}
	user, token, err := a.host.st.AddUser(strings.TrimSpace(name))
	if errors.Is(err, core.ErrNameTaken) {
		return "", core.ErrNameTaken
	}
	if err != nil {
		return "", errors.New(strings.TrimPrefix(err.Error(), "bad request: "))
	}
	a.logf("info", "server: account %q created", user.Name)
	return token, nil
}

// --- start with Windows ------------------------------------------------------

const runKey = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`

// quiet prepares a helper program so that it never shows a console window.
func quiet(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	hideWindow(cmd)
	return cmd
}

// autostartState is asked from Windows once and then remembered: the status
// page reads it every two seconds, and it only changes through SetAutostart.
func (a *Agent) autostartState() string {
	if runtime.GOOS != "windows" || a.AutostartCommand == "" {
		return ""
	}
	a.autoMu.Lock()
	defer a.autoMu.Unlock()
	if a.autoState == "" {
		a.autoState = "off"
		if quiet("reg", "query", runKey, "/v", "Henosis").Run() == nil {
			a.autoState = "on"
		}
	}
	return a.autoState
}

// SetAutostart makes the app start (or not) when the user signs in to Windows.
func (a *Agent) SetAutostart(on bool) error {
	if runtime.GOOS != "windows" || a.AutostartCommand == "" {
		return errors.New("starting with Windows is only available in the Windows app")
	}
	a.autoMu.Lock()
	defer a.autoMu.Unlock()
	a.autoState = "" // ask Windows again next time
	if !on {
		_ = quiet("reg", "delete", runKey, "/v", "Henosis", "/f").Run()
		a.logf("info", "Henosis no longer starts with Windows")
		return nil
	}
	out, err := quiet("reg", "add", runKey, "/v", "Henosis", "/t", "REG_SZ", "/d", a.AutostartCommand, "/f").CombinedOutput()
	if err != nil {
		return fmt.Errorf("could not register: %s", strings.TrimSpace(string(out)))
	}
	a.logf("info", "Henosis now starts with Windows: %s", a.AutostartCommand)
	return nil
}

// SelfCommand is the command line that starts this program in the background.
func SelfCommand(args string) string {
	self, err := os.Executable()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(`"` + self + `" ` + args)
}

// probeRelay opens one tunnel to the host through the relay and closes it,
// so a failure can be told apart: a full relay, another version, or nobody
// there.
func probeRelay(url, secret string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	c, err := relay.Dial(ctx, url, secret)
	if err != nil {
		return err
	}
	return c.Close()
}
