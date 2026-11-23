package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"henosis/agent/relay"
	"henosis/client"
	"henosis/config"
	"henosis/core"
	"henosis/fl"
	"henosis/store"
)

// Version is the release of the app. It is a variable so a test can build
// an "older" app.
var Version = "0.7.5"

// Agent runs one worker per linked project and carries out what the user
// asks for on the status page.
type Agent struct {
	// Log, when set, also receives every line of the debug console.
	Log    func(format string, args ...any)
	Timing Timing
	// Events holds the lines the debug console shows.
	Events *LogBuffer
	// AutostartCommand is what Windows runs at sign-in when autostart is on.
	AutostartCommand string
	// LocalIPs, when set, replaces looking up this computer's addresses.
	LocalIPs func() []string
	// ProjectRoots, when set, replaces the folders searched for FL Studio
	// projects to suggest.
	ProjectRoots []string
	// SearchRoots, when set, replaces the folders searched for a project
	// folder that was dragged into the window.
	SearchRoots []string
	// BaseFolder, when set, replaces Documents\Henosis as the place where
	// projects are put when no folder is named.
	BaseFolder string
	// AutoLinkEvery is how often the session is checked for new projects.
	AutoLinkEvery time.Duration
	// TickEvery is how often each worker looks around.
	TickEvery time.Duration

	mu      sync.Mutex
	cfg     *config.Config
	det     fl.Detector
	cl      *client.Client
	me      core.UserView
	runners map[string]*runner
	ctx     context.Context
	stopAll context.CancelFunc
	wg      sync.WaitGroup

	host hosting

	autoMu    sync.Mutex
	autoState string

	listMu   sync.Mutex
	listAt   time.Time
	listed   []core.ProjectView
	quitOnce sync.Once
	quit     chan struct{}
}

type runner struct {
	w      *Worker
	cancel context.CancelFunc
	kick   chan struct{}
	done   chan struct{}
}

// cachedDetector lets many workers share one look at FL Studio.
type cachedDetector struct {
	inner fl.Detector
	mu    sync.Mutex
	at    time.Time
	state fl.State
	err   error
}

func (c *cachedDetector) Detect() (fl.State, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.at) > 400*time.Millisecond {
		c.state, c.err = c.inner.Detect()
		c.at = time.Now()
	}
	return c.state, c.err
}

// New makes an agent for a loaded config.
func New(cfg *config.Config, det fl.Detector) *Agent {
	a := &Agent{
		Log: func(string, ...any) {}, Timing: DefaultTiming(), TickEvery: time.Second, Events: NewLogBuffer(2000), AutoLinkEvery: 4 * time.Second,
		cfg: cfg, det: &cachedDetector{inner: det}, runners: map[string]*runner{}, quit: make(chan struct{}),
	}
	a.connect() // no worker runs before Start
	return a
}

// logf writes one line to the debug console and to the log file.
func (a *Agent) logf(level, format string, args ...any) {
	entry := a.Events.Add(level, format, args...)
	if a.Log != nil {
		a.Log("[%s] %s", entry.Level, entry.Text)
	}
}

// Start begins syncing every linked project. It returns at once; the work
// stops when ctx ends or Close is called.
func (a *Agent) Start(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	// Close must stop this agent's own work even when the caller's ctx runs on.
	ctx, a.stopAll = context.WithCancel(ctx)
	a.ctx = ctx
	a.logf("info", "Henosis %s started. Settings: %s", Version, a.cfg.Path())
	if a.cfg.Host.Enabled {
		if err := a.startHosting(a.cfg.Host.Port); err != nil {
			a.logf("error", "the server on this computer could not start: %v", err)
		}
	}
	if !a.cfg.SignedIn() {
		a.logf("info", "not signed in yet")
	}
	a.connect()
	a.wg.Add(1)
	go func() { // projects of the session appear on this computer by themselves
		defer a.wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(a.AutoLinkEvery):
				a.autoLink()
			}
		}
	}()
}

// autoLink puts projects of the session that are not on this computer yet
// into Documents\Henosis\<name>, unless the user took them off on purpose.
func (a *Agent) autoLink() {
	a.mu.Lock()
	cl, on := a.cl, a.cfg.AutoLink && a.cfg.SignedIn()
	a.mu.Unlock()
	if !on || cl == nil {
		return
	}
	list, err := cl.Projects()
	if err != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cl != cl {
		return
	}
	for _, p := range list {
		if a.cfg.LinkByProject(p.ID) != nil || contains(a.cfg.Skipped, p.ID) {
			continue
		}
		for n := 1; n <= 5; n++ {
			name := p.Name
			if n > 1 {
				name = fmt.Sprintf("%s (%d)", p.Name, n)
			}
			link, err := a.attach(p, a.folderFor(name))
			if err == nil {
				a.logf("info", "%s: new in the session, downloading to %s", p.Name, link.Folder)
				break
			}
			if n == 5 {
				a.logf("error", "%s: could not be put on this computer: %v", p.Name, err)
				a.cfg.Skipped = append(a.cfg.Skipped, p.ID)
			}
		}
	}
}

func contains(list []string, item string) bool {
	for _, s := range list {
		if s == item {
			return true
		}
	}
	return false
}

// Suggestion is an FL Studio project found on this computer that is not in
// the session yet.
type Suggestion struct {
	Name   string `json:"name"`
	Folder string `json:"folder"` // the project's own folder; "" for a loose file
	File   string `json:"file"`   // the .flp
	Loose  bool   `json:"loose"`  // the .flp sits directly in the projects folder
}

// Suggestions looks through FL Studio's projects folder for projects that
// could be added with one click, most recently saved first.
func (a *Agent) Suggestions() []Suggestion {
	roots := a.ProjectRoots
	if roots == nil {
		if home, err := os.UserHomeDir(); err == nil {
			for _, docs := range []string{"Documents", filepath.Join("OneDrive", "Documents")} {
				roots = append(roots, filepath.Join(home, docs, "Image-Line", "FL Studio", "Projects"))
			}
		}
	}
	type found struct {
		Suggestion
		saved time.Time
	}
	byKey := map[string]*found{}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			depth := strings.Count(filepath.ToSlash(rel), "/")
			if d.IsDir() {
				lower := strings.ToLower(d.Name())
				if path != root && (depth >= 3 || lower == "backup" || lower == "templates" || strings.HasPrefix(lower, ".") || a.cfg.LinkFor(path) != nil) {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.EqualFold(filepath.Ext(path), ".flp") {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			dir := filepath.Dir(path)
			s := found{saved: info.ModTime()}
			key := dir
			if dir == root {
				s.Suggestion = Suggestion{Name: strings.TrimSuffix(d.Name(), filepath.Ext(d.Name())), File: path, Loose: true}
				key = path
				if a.cfg.LinkFor(a.folderFor(s.Name)) != nil {
					return nil // already added as a copy
				}
			} else {
				s.Suggestion = Suggestion{Name: filepath.Base(dir), Folder: dir, File: path}
			}
			if old := byKey[key]; old == nil || s.saved.After(old.saved) {
				byKey[key] = &s
			}
			return nil
		})
	}
	list := make([]*found, 0, len(byKey))
	for _, s := range byKey {
		list = append(list, s)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].saved.After(list[j].saved) })
	out := []Suggestion{}
	for i, s := range list {
		if i == 10 {
			break
		}
		out = append(out, s.Suggestion)
	}
	return out
}

// CreateFromFile makes a project out of a single .flp that has no folder of
// its own: the file is copied into Documents\Henosis\<name>, and that
// folder becomes the project. The original file is left where it is.
func (a *Agent) CreateFromFile(file string) (config.Link, error) {
	file = strings.Trim(strings.TrimSpace(file), `"`)
	info, err := os.Stat(file)
	if err != nil || info.IsDir() || !strings.EqualFold(filepath.Ext(file), ".flp") {
		return config.Link{}, errors.New("that is not an FL Studio project file")
	}
	name := strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
	folder := a.folderFor(name)
	target := filepath.Join(folder, filepath.Base(file))
	if _, err := os.Stat(target); err == nil {
		return config.Link{}, fmt.Errorf("%s already exists", target)
	}
	if err := os.MkdirAll(folder, 0o755); err != nil {
		return config.Link{}, err
	}
	if err := copyInto(file, target); err != nil {
		return config.Link{}, err
	}
	_ = os.Chtimes(target, info.ModTime(), info.ModTime())
	link, err := a.CreateProject(name, folder)
	if err == nil {
		a.logf("info", "%s: copied to %s. Open the project from there from now on.", name, target)
	}
	return link, err
}

// connect (re)creates the client and the workers from the config.
func (a *Agent) connect() {
	if !a.cfg.SignedIn() {
		return
	}
	a.cl = a.clientFor(a.cfg.Server, a.cfg.Token)
	a.me = core.UserView{ID: a.cfg.UserID, Name: a.cfg.User}
	for _, link := range a.cfg.Links {
		a.startRunner(link)
	}
}

// clientFor builds a client for the stored server URL. A URL beginning with
// "relay://" means the friend signed in through the relay; the hex code
// after the scheme is the session secret used to dial through it.
func (a *Agent) clientFor(server, token string) *client.Client {
	if strings.HasPrefix(server, "relay://") {
		code := strings.TrimPrefix(server, "relay://")
		rc := relay.NewHTTPClient(a.cfg.RelayURL(), code)
		return client.NewWith("http://relay", token, a.cfg.Device, rc)
	}
	return client.New(server, token, a.cfg.Device)
}

func (a *Agent) startRunner(link config.Link) {
	if a.ctx == nil || a.runners[link.ProjectID] != nil {
		return
	}
	w := NewWorker(link, a.cl.Remote(link.ProjectID), a.me, a.cfg.Device, a.det, a.Timing, time.Now)
	w.log = a.logf
	ctx, cancel := context.WithCancel(a.ctx)
	r := &runner{w: w, cancel: cancel, kick: make(chan struct{}, 1), done: make(chan struct{})}
	a.runners[link.ProjectID] = r
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer close(r.done)
		for {
			w.Tick()
			select {
			case <-ctx.Done():
				w.Stop()
				return
			case <-r.kick:
			case <-time.After(a.TickEvery):
			}
		}
	}()
}

func (a *Agent) stopRunner(projectID string) {
	r := a.runners[projectID]
	if r == nil {
		return
	}
	delete(a.runners, projectID)
	r.cancel()
	<-r.done
}

// Close stops all workers; held locks are given back.
func (a *Agent) Close() {
	a.mu.Lock()
	ids := make([]string, 0, len(a.runners))
	for id := range a.runners {
		ids = append(ids, id)
	}
	for _, id := range ids {
		a.stopRunner(id)
	}
	a.stopHosting()
	if a.stopAll != nil {
		a.stopAll()
	}
	a.mu.Unlock()
	a.wg.Wait()
}

// Quit asks the program around the agent to end.
func (a *Agent) Quit() { a.quitOnce.Do(func() { close(a.quit) }) }

// Done is closed once Quit was called.
func (a *Agent) Done() <-chan struct{} { return a.quit }

// AppState is everything the status page shows.
type AppState struct {
	Version  string             `json:"version"`
	SignedIn bool               `json:"signed_in"`
	Server   string             `json:"server"`
	User     string             `json:"user"`
	Device   string             `json:"device"`
	Projects []Status           `json:"projects"`
	Unlinked []core.ProjectView `json:"unlinked"` // on the server, not on this computer yet
	Suggest  string             `json:"suggest"`  // where new project folders go by default
	Host     HostState          `json:"host"`
	// Autostart: "on", "off", or "" where it does not apply.
	Autostart string `json:"autostart"`
	// Previous is the session this computer holds, shown on the start screen
	// so "Start a session" does not silently reopen it.
	Previous *PreviousSession `json:"previous_session,omitempty"`
}

// State reports the app's current state.
func (a *Agent) State() AppState {
	a.mu.Lock()
	st := AppState{Version: Version, SignedIn: a.cfg.SignedIn(), Server: a.cfg.Server, User: a.cfg.User,
		Device: a.cfg.Device, Projects: []Status{}, Unlinked: []core.ProjectView{}, Suggest: a.folderFor("")}
	linked := map[string]bool{}
	for _, link := range a.cfg.Links {
		linked[link.ProjectID] = true
		if r := a.runners[link.ProjectID]; r != nil {
			st.Projects = append(st.Projects, r.w.Status())
		}
	}
	cl := a.cl
	st.Host = a.hostState()
	var running *store.Store
	if a.host.srv != nil {
		running = a.host.st
	}
	dir := a.hostDir()
	a.mu.Unlock()
	if !st.SignedIn {
		st.Previous = previousSession(running, dir)
	}
	st.Autostart = a.autostartState()
	sort.Slice(st.Projects, func(i, j int) bool {
		return strings.ToLower(st.Projects[i].Name) < strings.ToLower(st.Projects[j].Name)
	})
	if cl != nil {
		for _, p := range a.serverProjects(cl) {
			if !linked[p.ID] {
				st.Unlinked = append(st.Unlinked, p)
			}
		}
	}
	return st
}

// serverProjects lists the account's projects, asking the server at most
// every few seconds.
func (a *Agent) serverProjects(cl *client.Client) []core.ProjectView {
	a.listMu.Lock()
	defer a.listMu.Unlock()
	if time.Since(a.listAt) > 5*time.Second {
		if list, err := cl.Projects(); err == nil {
			a.listed = list
		}
		a.listAt = time.Now()
	}
	return a.listed
}

func (a *Agent) forgetList() {
	a.listMu.Lock()
	a.listAt = time.Time{}
	a.listMu.Unlock()
}

// Login checks a server and token and remembers them.
func (a *Agent) Login(server, token string) error {
	server, token = strings.TrimRight(strings.TrimSpace(server), "/"), strings.TrimSpace(token)
	if server == "" || token == "" {
		return errors.New("the server address and the token are both needed")
	}
	if !strings.Contains(server, "://") {
		server = "http://" + server
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	device := a.cfg.Device
	if device == "" {
		device = config.NewDevice()
	}
	// For a relay://<code> URL we build the client with the relay transport.
	oldDevice := a.cfg.Device
	a.cfg.Device = device
	cl := a.clientFor(server, token)
	a.cfg.Device = oldDevice
	me, err := cl.Me()
	if err != nil {
		if errors.Is(err, core.ErrSignIn) {
			return errors.New("the server did not accept that token")
		}
		return fmt.Errorf("could not reach %s: %w", server, err)
	}
	if a.cfg.SignedIn() && (a.cfg.Server != server || a.cfg.UserID != me.ID) && len(a.cfg.Links) > 0 {
		return errors.New("this computer has projects linked to another account; unlink them first")
	}
	for id := range a.runners {
		a.stopRunner(id)
	}
	// Projects linked here that this session does not have belong to an
	// earlier session: unlink them, so a new session starts clean. Their
	// folders and files stay where they are. Only when the list is known.
	if list, err := cl.Projects(); err == nil {
		here := map[string]bool{}
		for _, p := range list {
			here[p.ID] = true
		}
		var kept []config.Link
		for _, link := range a.cfg.Links {
			if here[link.ProjectID] {
				kept = append(kept, link)
			} else {
				a.logf("info", "%s is not in this session, so it is no longer synced here; its files stay in %s", link.Name, link.Folder)
			}
		}
		a.cfg.Links = kept
		var skipped []string
		for _, id := range a.cfg.Skipped {
			if here[id] {
				skipped = append(skipped, id)
			}
		}
		a.cfg.Skipped = skipped
	}
	a.cfg.Server, a.cfg.Token, a.cfg.User, a.cfg.UserID, a.cfg.Device = server, token, me.Name, me.ID, device
	if err := a.cfg.Save(); err != nil {
		return err
	}
	a.connect()
	a.forgetList()
	return nil
}

// Logout forgets the token. Linked folders and their files stay.
func (a *Agent) Logout() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for id := range a.runners {
		a.stopRunner(id)
	}
	a.cfg.Token, a.cl, a.cfg.AutoLink = "", nil, false
	return a.cfg.Save()
}

var forbiddenInNames = strings.NewReplacer(`<`, "", `>`, "", `:`, "", `"`, "", `/`, "-", `\`, "-", `|`, "-", `?`, "", `*`, "")

// DefaultFolder is where a project's folder goes when the user names none:
// Documents\Henosis\<project name>.
func DefaultFolder(name string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	name = strings.TrimRight(strings.TrimSpace(forbiddenInNames.Replace(name)), ". ")
	return filepath.Join(home, "Documents", "Henosis", name)
}

// folderFor is where a project goes when the user names no folder.
func (a *Agent) folderFor(name string) string {
	if a.BaseFolder == "" {
		return DefaultFolder(name)
	}
	return filepath.Join(a.BaseFolder, filepath.Base(DefaultFolder(name)))
}

func (a *Agent) client() (*client.Client, error) {
	if a.cl == nil {
		return nil, errors.New("sign in first")
	}
	return a.cl, nil
}

// attach links a server project to a folder and starts syncing it.
func (a *Agent) attach(p core.ProjectView, folder string) (config.Link, error) {
	if strings.TrimSpace(folder) == "" {
		folder = a.folderFor(p.Name)
	}
	folder, err := config.CleanFolder(folder)
	if err != nil {
		return config.Link{}, err
	}
	link := config.Link{ProjectID: p.ID, Name: p.Name, Folder: folder}
	if a.cfg.LinkByProject(p.ID) != nil {
		return link, fmt.Errorf("%q is already on this computer", p.Name)
	}
	if err := a.cfg.CheckFolder(folder); err != nil {
		return link, err
	}
	if err := os.MkdirAll(folder, 0o755); err != nil {
		return link, fmt.Errorf("cannot create %s: %w", folder, err)
	}
	if err := a.cfg.AddLink(link); err != nil {
		return link, err
	}
	for i, id := range a.cfg.Skipped {
		if id == p.ID {
			a.cfg.Skipped = append(a.cfg.Skipped[:i], a.cfg.Skipped[i+1:]...)
			break
		}
	}
	if err := a.cfg.Save(); err != nil {
		a.cfg.RemoveLink(p.ID)
		return link, err
	}
	a.startRunner(link)
	a.forgetList()
	return link, nil
}

// CreateProject makes a new project from a folder on this computer.
func (a *Agent) CreateProject(name, folder string) (config.Link, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	cl, err := a.client()
	if err != nil {
		return config.Link{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" && strings.TrimSpace(folder) != "" {
		if clean, err := config.CleanFolder(folder); err == nil {
			name = filepath.Base(clean) // a project is called like its folder unless told otherwise
		}
	}
	if name == "" {
		return config.Link{}, errors.New("choose a project folder")
	}
	// Check the folder before anything is created on the server. A folder the
	// user typed must exist: a typo would otherwise make an empty project.
	check := folder
	typed := strings.TrimSpace(check) != ""
	if !typed {
		check = a.folderFor(name)
	}
	clean, err := config.CleanFolder(check)
	if err != nil {
		return config.Link{}, err
	}
	if typed {
		if info, err := os.Stat(clean); err != nil || !info.IsDir() {
			return config.Link{}, fmt.Errorf("there is no folder %s on this computer", clean)
		}
	}
	if err := a.cfg.CheckFolder(clean); err != nil {
		return config.Link{}, err
	}
	p, err := cl.CreateProject(name)
	if err != nil {
		return config.Link{}, err
	}
	return a.attach(p, folder)
}

// Join accepts an invite and puts the project into a folder.
func (a *Agent) Join(code, folder string) (config.Link, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	cl, err := a.client()
	if err != nil {
		return config.Link{}, err
	}
	if strings.TrimSpace(folder) != "" {
		clean, err := config.CleanFolder(folder)
		if err != nil {
			return config.Link{}, err
		}
		if err := a.cfg.CheckFolder(clean); err != nil {
			return config.Link{}, err
		}
	}
	p, err := cl.AcceptInvite(code)
	if err != nil {
		return config.Link{}, errors.New("that invite code is not valid (codes work once and for 7 days)")
	}
	return a.attach(p, folder)
}

// LinkProject puts a project this account already belongs to into a folder.
func (a *Agent) LinkProject(projectID, folder string) (config.Link, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	cl, err := a.client()
	if err != nil {
		return config.Link{}, err
	}
	p, err := cl.Project(projectID)
	if err != nil {
		return config.Link{}, err
	}
	return a.attach(p, folder)
}

// Unlink stops syncing a project on this computer. The files stay where
// they are, and the project stays on the server.
func (a *Agent) Unlink(projectID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cfg.LinkByProject(projectID) == nil {
		return errors.New("that project is not on this computer")
	}
	a.stopRunner(projectID)
	a.cfg.RemoveLink(projectID)
	if !contains(a.cfg.Skipped, projectID) {
		a.cfg.Skipped = append(a.cfg.Skipped, projectID) // do not download it again by itself
	}
	a.forgetList()
	return a.cfg.Save()
}

// DeleteProject removes a project from the session for everyone and unlinks
// it here. Nobody's files are deleted; the other members' apps say the
// project is no longer available.
func (a *Agent) DeleteProject(projectID string) error {
	a.mu.Lock()
	cl, err := a.client()
	name := projectID
	if link := a.cfg.LinkByProject(projectID); link != nil {
		name = link.Name
	}
	a.mu.Unlock()
	if err != nil {
		return err
	}
	if err := cl.DeleteProject(projectID); err != nil {
		if errors.Is(err, core.ErrForbidden) {
			return errors.New("only the member who added this project can delete it from the session")
		}
		return err
	}
	a.logf("info", "%s: deleted from the session; the files on this computer were kept", name)
	if a.cfg.LinkByProject(projectID) != nil {
		_ = a.Unlink(projectID)
	}
	a.forgetList()
	return nil
}

// Worker returns the worker of a linked project.
func (a *Agent) Worker(projectID string) (*Worker, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.runners[projectID]
	if r == nil {
		return nil, errors.New("that project is not on this computer")
	}
	return r.w, nil
}

// Kick makes a project's worker look around now instead of at its next tick.
func (a *Agent) Kick(projectID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r := a.runners[projectID]; r != nil {
		select {
		case r.kick <- struct{}{}:
		default:
		}
	}
}

// Invite makes a single-use invite code for a project.
func (a *Agent) Invite(projectID string) (string, error) {
	a.mu.Lock()
	cl, err := a.client()
	a.mu.Unlock()
	if err != nil {
		return "", err
	}
	code, err := cl.CreateInvite(projectID)
	if errors.Is(err, core.ErrForbidden) {
		return "", errors.New("only the project's owner can invite")
	}
	return code, err
}

// SetLifetime changes how many days old versions are kept.
func (a *Agent) SetLifetime(projectID string, days int) error {
	a.mu.Lock()
	cl, err := a.client()
	a.mu.Unlock()
	if err != nil {
		return err
	}
	_, err = cl.UpdateProject(projectID, nil, &days)
	if errors.Is(err, core.ErrForbidden) {
		return errors.New("only the project's owner can change this")
	}
	if err == nil {
		a.Kick(projectID)
	}
	return err
}

// SetAuto saves the auto-send and auto-update settings for a project.
func (a *Agent) SetAuto(projectID string, autoSend, autoUpdate bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	link := a.cfg.LinkByProject(projectID)
	if link == nil {
		return errors.New("project not found")
	}
	link.NoAutoSend = !autoSend
	link.NoAutoUpdate = !autoUpdate
	return a.cfg.Save()
}

// OpenProject opens the main .flp file of a project in FL Studio.
func (a *Agent) OpenProject(projectID string) error {
	w, err := a.Worker(projectID)
	if err != nil {
		return err
	}
	path := w.MainFLP()
	if path == "" {
		return errors.New("no .flp file found in the project folder")
	}
	return OpenFile(path)
}

// OpenPath shows a folder in the file manager. On Windows it uses
// "cmd /c start" so the window comes to the front.
func OpenPath(path string) error {
	switch runtime.GOOS {
	case "windows":
		return quiet("cmd", "/c", "start", "", path).Start()
	case "darwin":
		return exec.Command("open", path).Start()
	default:
		return exec.Command("xdg-open", path).Start()
	}
}

// OpenFile opens a file in its default application. On Windows a .flp
// file opens in FL Studio.
func OpenFile(path string) error {
	switch runtime.GOOS {
	case "windows":
		return quiet("cmd", "/c", "start", "", path).Start()
	case "darwin":
		return exec.Command("open", path).Start()
	default:
		return exec.Command("xdg-open", path).Start()
	}
}

// OpenAppWindow shows the status page in a window of its own, without tabs
// or an address bar, using Edge or Chrome's app mode. If neither is found
// the page opens in the default browser.
func OpenAppWindow(url string) error {
	if runtime.GOOS == "windows" {
		var candidates []string
		for _, root := range []string{os.Getenv("ProgramFiles(x86)"), os.Getenv("ProgramFiles"), os.Getenv("LocalAppData")} {
			if root != "" {
				candidates = append(candidates,
					filepath.Join(root, "Microsoft", "Edge", "Application", "msedge.exe"),
					filepath.Join(root, "Google", "Chrome", "Application", "chrome.exe"))
			}
		}
		for _, exe := range candidates {
			if info, err := os.Stat(exe); err == nil && !info.IsDir() {
				if exec.Command(exe, "--app="+url, "--window-size=980,900").Start() == nil {
					return nil
				}
			}
		}
	}
	return OpenBrowser(url)
}

// OpenBrowser opens a web address in the default browser.
func OpenBrowser(url string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		return exec.Command("open", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}
