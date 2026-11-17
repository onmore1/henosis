// Package cli is the henosis command line. The same code is the background
// app: "henosis run" starts it.
package cli
//govno pazl
import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"henosis/agent"
	"henosis/client"
	"henosis/config"
	"henosis/core"
	"henosis/engine"
	"henosis/fl"
)

const usage = `henosis %s - sync FL Studio projects with a partner

Getting started
  henosis login SERVER TOKEN      sign in on this computer
  henosis run                     start the app (syncs in the background, opens the status page)
  henosis open                    open the status page of the running app

Projects
  henosis create NAME [FOLDER]    make a project from a folder (default: the current folder)
  henosis invite [FOLDER]         print an invite code for a partner
  henosis join CODE [FOLDER]      join a partner's project
  henosis projects                list your projects on the server
  henosis link PROJECT [FOLDER]   put one of your projects on this computer
  henosis unlink [FOLDER]         stop syncing a folder (files stay)

One folder, by hand
  henosis status [FOLDER]         what changed here and on the server
  henosis sync [--yes-delete] [FOLDER]
  henosis log [FOLDER]            list the versions
  henosis restore VERSION [FOLDER]   bring back a version (its number from "log")

Other
  henosis whoami | logout | doctor | version
  henosis autostart on|off        start the app when you sign in to Windows
`

// SelfArgs is what follows the program's own path when Windows starts it at
// sign-in. The windowless app sets it to its own flags.
var SelfArgs = "run --background"

type env struct {
	out, err io.Writer
	cfg      *config.Config
}

// Main runs one command and returns the process exit code.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintf(stdout, usage, agent.Version)
		return 0
	}
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	e := &env{out: stdout, err: stderr, cfg: cfg}
	commands := map[string]func(context.Context, []string) error{
		"version": e.version, "login": e.login, "logout": e.logout, "whoami": e.whoami,
		"projects": e.projects, "create": e.create, "join": e.join, "link": e.link, "unlink": e.unlink,
		"invite": e.invite, "status": e.status, "sync": e.sync, "log": e.log, "restore": e.restore,
		"run": e.run, "open": e.open, "doctor": e.doctor, "autostart": e.autostart,
	}
	command, ok := commands[args[0]]
	if !ok {
		fmt.Fprintf(stderr, "unknown command %q; run \"henosis help\"\n", args[0])
		return 2
	}
	if err := command(ctx, args[1:]); err != nil {
		var u usageError
		if errors.As(err, &u) {
			fmt.Fprintln(stderr, "usage:", string(u))
			return 2
		}
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return 0
}

type usageError string

func (u usageError) Error() string { return string(u) }

func (e *env) say(format string, args ...any) { fmt.Fprintf(e.out, format+"\n", args...) }

// --- talking to a running app ------------------------------------------------

func (e *env) pageURL() string { return "http://127.0.0.1:" + strconv.Itoa(e.cfg.StatusPort()) }

// runningVersion returns the version of the app answering on this computer,
// or "" when none does.
func (e *env) runningVersion() string {
	c := http.Client{Timeout: 700 * time.Millisecond}
	resp, err := c.Get(e.pageURL() + "/api/state")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var st agent.AppState
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&st) != nil {
		return ""
	}
	return st.Version
}

// appRunning reports whether the background app answers on this computer.
func (e *env) appRunning() bool { return e.runningVersion() != "" }

// ask sends a request to the running app, like the status page does.
func (e *env) ask(method, path string, in, out any) error {
	var body io.Reader
	if method == "POST" {
		if in == nil {
			in = map[string]string{}
		}
		data, _ := json.Marshal(in)
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, e.pageURL()+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("X-Henosis", "1")
	resp, err := (&http.Client{Timeout: 30 * time.Minute}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		var failure struct{ Error string }
		_ = json.Unmarshal(data, &failure)
		if failure.Error == "" {
			failure.Error = "the app answered " + resp.Status
		}
		return errors.New(failure.Error)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// offline returns an agent that is not started: it changes the config and
// the server, and the app picks the change up when it starts.
func (e *env) offline() *agent.Agent {
	return agent.New(e.cfg, &fl.Fake{})
}

// --- account -----------------------------------------------------------------

func (e *env) version(context.Context, []string) error {
	e.say("%s", agent.Version)
	return nil
}

func (e *env) login(_ context.Context, args []string) error {
	if len(args) != 2 {
		return usageError("henosis login SERVER TOKEN")
	}
	if e.appRunning() {
		if err := e.ask("POST", "/api/login", map[string]string{"server": args[0], "token": args[1]}, nil); err != nil {
			return err
		}
	} else {
		a := e.offline()
		defer a.Close()
		if err := a.Login(args[0], args[1]); err != nil {
			return err
		}
	}
	cfg, err := config.Load(e.cfg.Path())
	if err != nil {
		return err
	}
	e.say("Signed in as %s on %s. This computer is called %s.", cfg.User, cfg.Server, cfg.Device)
	return nil
}

func (e *env) logout(context.Context, []string) error {
	if e.appRunning() {
		return e.ask("POST", "/api/logout", nil, nil)
	}
	e.cfg.Token = ""
	return e.cfg.Save()
}

func (e *env) client() (*client.Client, error) {
	if !e.cfg.SignedIn() {
		return nil, errors.New("not signed in; run \"henosis login SERVER TOKEN\"")
	}
	return client.New(e.cfg.Server, e.cfg.Token, e.cfg.Device), nil
}

func (e *env) whoami(context.Context, []string) error {
	cl, err := e.client()
	if err != nil {
		return err
	}
	me, err := cl.Me()
	if err != nil {
		return err
	}
	e.say("%s on %s (this computer: %s)", me.Name, e.cfg.Server, e.cfg.Device)
	return nil
}

// --- projects ----------------------------------------------------------------

func (e *env) projects(context.Context, []string) error {
	cl, err := e.client()
	if err != nil {
		return err
	}
	list, err := cl.Projects()
	if err != nil {
		return err
	}
	if len(list) == 0 {
		e.say("No projects yet. Make one with \"henosis create NAME [FOLDER]\".")
	}
	for _, p := range list {
		where := "not on this computer"
		if link := e.cfg.LinkByProject(p.ID); link != nil {
			where = link.Folder
		}
		people := "only you"
		if p.MemberCount > 1 {
			people = strconv.Itoa(p.MemberCount) + " members"
		}
		e.say("%-28s %-8s %-11s %s\n  id %s", p.Name, p.Role, people, where, p.ID)
	}
	return nil
}

func folderArg(args []string, at int) string {
	if len(args) > at {
		return args[at]
	}
	return "."
}

func (e *env) attach(path string, in map[string]string, direct func(*agent.Agent) (config.Link, error)) error {
	if in["folder"] != "" {
		clean, err := config.CleanFolder(in["folder"])
		if err != nil {
			return err
		}
		in["folder"] = clean
	}
	var link config.Link
	if e.appRunning() {
		if err := e.ask("POST", path, in, &link); err != nil {
			return err
		}
		e.say("%q is now synced in %s", link.Name, link.Folder)
		return nil
	}
	a := e.offline()
	defer a.Close()
	link, err := direct(a)
	if err != nil {
		return err
	}
	e.say("%q is linked to %s. Start the app with \"henosis run\" to sync it.", link.Name, link.Folder)
	return nil
}

func (e *env) create(_ context.Context, args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return usageError("henosis create NAME [FOLDER]")
	}
	in := map[string]string{"name": args[0], "folder": folderArg(args, 1)}
	return e.attach("/api/create", in, func(a *agent.Agent) (config.Link, error) { return a.CreateProject(in["name"], in["folder"]) })
}

func (e *env) join(_ context.Context, args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return usageError("henosis join CODE [FOLDER]")
	}
	in := map[string]string{"code": args[0], "folder": ""}
	if len(args) == 2 {
		in["folder"] = args[1]
	}
	return e.attach("/api/join", in, func(a *agent.Agent) (config.Link, error) { return a.Join(in["code"], in["folder"]) })
}

func (e *env) link(_ context.Context, args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return usageError("henosis link PROJECT [FOLDER]   (PROJECT is a name or an id from \"henosis projects\")")
	}
	cl, err := e.client()
	if err != nil {
		return err
	}
	list, err := cl.Projects()
	if err != nil {
		return err
	}
	var found []core.ProjectView
	for _, p := range list {
		if p.ID == args[0] || strings.EqualFold(p.Name, args[0]) {
			found = append(found, p)
		}
	}
	if len(found) != 1 {
		return fmt.Errorf("%d projects match %q; use the id from \"henosis projects\"", len(found), args[0])
	}
	in := map[string]string{"project_id": found[0].ID, "folder": ""}
	if len(args) == 2 {
		in["folder"] = args[1]
	}
	return e.attach("/api/link", in, func(a *agent.Agent) (config.Link, error) { return a.LinkProject(in["project_id"], in["folder"]) })
}

// linked finds the link for a folder argument.
func (e *env) linked(folder string) (config.Link, error) {
	clean, err := config.CleanFolder(folder)
	if err != nil {
		return config.Link{}, err
	}
	link := e.cfg.LinkFor(clean)
	if link == nil {
		return config.Link{}, fmt.Errorf("%s is not a Henosis project folder", clean)
	}
	return *link, nil
}

func (e *env) unlink(_ context.Context, args []string) error {
	link, err := e.linked(folderArg(args, 0))
	if err != nil {
		return err
	}
	if e.appRunning() {
		err = e.ask("POST", "/api/projects/"+link.ProjectID+"/unlink", nil, nil)
	} else {
		a := e.offline()
		defer a.Close()
		err = a.Unlink(link.ProjectID)
	}
	if err == nil {
		e.say("%s is no longer synced. The files are still there.", link.Folder)
	}
	return err
}

func (e *env) invite(_ context.Context, args []string) error {
	link, err := e.linked(folderArg(args, 0))
	if err != nil {
		return err
	}
	cl, err := e.client()
	if err != nil {
		return err
	}
	code, err := cl.CreateInvite(link.ProjectID)
	if errors.Is(err, core.ErrForbidden) {
		return errors.New("only the project's owner can invite")
	}
	if err != nil {
		return err
	}
	e.say("Invite code for %q: %s\nIt works once, for 7 days. Your partner runs: henosis join %s", link.Name, code, code)
	return nil
}

// --- one folder, by hand -------------------------------------------------------

func (e *env) engine(folder string) (*engine.Engine, *client.ProjectRemote, config.Link, error) {
	link, err := e.linked(folder)
	if err != nil {
		return nil, nil, link, err
	}
	cl, err := e.client()
	if err != nil {
		return nil, nil, link, err
	}
	remote := cl.Remote(link.ProjectID)
	return engine.Open(link.Folder, engine.AutoLock{Remote: remote, Locker: remote}, e.cfg.User), remote, link, nil
}

func (e *env) status(_ context.Context, args []string) error {
	eng, remote, link, err := e.engine(folderArg(args, 0))
	if err != nil {
		return err
	}
	st, err := eng.Status()
	if err != nil {
		return err
	}
	e.say("%s  (%s)", link.Name, link.Folder)
	if !st.RemoteReachable {
		e.say("  the server cannot be reached")
	} else if view, err := remote.View(); err == nil && view.Lock != nil && !view.Lock.Stale {
		e.say("  %s is editing on %s", view.Lock.User.Name, view.Lock.Device)
	}
	for _, f := range st.Local.Added {
		e.say("  new here      %s", f.Path)
	}
	for _, f := range st.Local.Changed {
		e.say("  changed here  %s", f.Path)
	}
	for _, path := range st.Local.Removed {
		e.say("  removed here  %s", path)
	}
	switch {
	case st.RemoteAhead:
		e.say("  a newer version is on the server")
	case st.Local.Empty() && st.RemoteReachable:
		e.say("  up to date")
	}
	return nil
}

func (e *env) sync(_ context.Context, args []string) error {
	flags := flag.NewFlagSet("sync", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	yes := flags.Bool("yes-delete", false, "")
	if err := flags.Parse(args); err != nil {
		return usageError("henosis sync [--yes-delete] [FOLDER]")
	}
	if e.appRunning() {
		link, err := e.linked(folderArg(flags.Args(), 0))
		if err != nil {
			return err
		}
		if *yes {
			if err := e.ask("POST", "/api/projects/"+link.ProjectID+"/confirm-delete", nil, nil); err != nil {
				return err
			}
		}
		if err := e.ask("POST", "/api/projects/"+link.ProjectID+"/sync", nil, nil); err != nil {
			return err
		}
		e.say("The running app is syncing %q now.", link.Name)
		return nil
	}
	eng, _, link, err := e.engine(folderArg(flags.Args(), 0))
	if err != nil {
		return err
	}
	res, err := eng.Sync(engine.PushOptions{AllowMassDelete: *yes})
	for _, copyName := range res.ConflictCopies {
		e.say("Kept your version as %s", copyName)
	}
	switch engine.KindOf(err) {
	case engine.KindNone:
	case engine.KindMassDelete:
		return fmt.Errorf("%v. If that is intended, run \"henosis sync --yes-delete\"", err)
	case engine.KindLockNotHeld:
		return errors.New("a partner is editing; your changes stay here until they are done")
	default:
		return err
	}
	switch {
	case res.Pulled && res.Pushed:
		e.say("%s: downloaded the newest version and uploaded your changes.", link.Name)
	case res.Pulled:
		e.say("%s: downloaded the newest version.", link.Name)
	case res.Pushed:
		e.say("%s: uploaded your changes.", link.Name)
	default:
		e.say("%s: already up to date.", link.Name)
	}
	return nil
}

func (e *env) log(_ context.Context, args []string) error {
	_, remote, _, err := e.engine(folderArg(args, 0))
	if err != nil {
		return err
	}
	history, err := remote.History()
	if err != nil {
		return err
	}
	if len(history) == 0 {
		e.say("No versions yet.")
	}
	for _, v := range history {
		when := v.CreatedAt
		if t, err := time.Parse(time.RFC3339, v.CreatedAt); err == nil {
			when = t.Local().Format("2006-01-02 15:04")
		}
		note := ""
		if v.IsConflict {
			note = "  [conflict copy kept]"
		}
		e.say("%4d  %s  %-12s %s%s", v.Seq, when, v.Author, v.Message, note)
	}
	return nil
}

func (e *env) restore(_ context.Context, args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return usageError("henosis restore VERSION [FOLDER]   (VERSION is the number shown by \"henosis log\")")
	}
	eng, remote, link, err := e.engine(folderArg(args, 1))
	if err != nil {
		return err
	}
	history, err := remote.History()
	if err != nil {
		return err
	}
	id := ""
	for _, v := range history {
		if strconv.Itoa(v.Seq) == args[0] || v.ID == args[0] {
			id = v.ID
		}
	}
	if id == "" {
		return fmt.Errorf("there is no version %s; see \"henosis log\"", args[0])
	}
	if e.appRunning() {
		if err := e.ask("POST", "/api/projects/"+link.ProjectID+"/restore", map[string]string{"snapshot_id": id}, nil); err != nil {
			return err
		}
	} else if _, err := eng.Restore(id); err != nil {
		return err
	}
	e.say("%s: version %s is the newest version again.", link.Name, args[0])
	return nil
}

// --- the app -------------------------------------------------------------------

type logger struct {
	mu  sync.Mutex
	out io.Writer
}

func (l *logger) printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.out, time.Now().Format("2006-01-02 15:04:05 ")+format+"\n", args...)
}

func (e *env) run(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	background := flags.Bool("background", false, "")
	logFile := flags.String("log", "", "")
	port := flags.Int("port", 0, "")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return usageError("henosis run [--background] [--log FILE] [--port N]")
	}
	if *port > 0 && *port != e.cfg.Port {
		e.cfg.Port = *port // remembered, so the other commands find the app
		if err := e.cfg.Save(); err != nil {
			return err
		}
	}
	address := "127.0.0.1:" + strconv.Itoa(e.cfg.StatusPort())
	listener, err := net.Listen("tcp", address)
	if err != nil {
		// An older version still running in the background makes way for
		// this one, so starting a new download really starts the new app.
		if running := e.runningVersion(); running != "" && running != agent.Version {
			_ = e.ask("POST", "/api/quit", nil, nil)
			for i := 0; i < 50 && err != nil; i++ {
				time.Sleep(200 * time.Millisecond)
				listener, err = net.Listen("tcp", address)
			}
			if err == nil {
				e.say("Replaced the running Henosis %s with %s.", running, agent.Version)
			}
		}
	}
	if err != nil {
		if e.appRunning() {
			// Starting the app twice just shows the one that runs.
			if !*background {
				_ = agent.OpenAppWindow(e.pageURL())
			}
			e.say("Henosis is already running: %s", e.pageURL())
			return nil
		}
		return fmt.Errorf("port %d is in use by another program; choose another with --port", e.cfg.StatusPort())
	}
	log := &logger{out: e.out}
	if *logFile != "" {
		if info, err := os.Stat(*logFile); err == nil && info.Size() > 2<<20 {
			_ = os.Rename(*logFile, *logFile+".old")
		}
		_ = os.MkdirAll(filepath.Dir(*logFile), 0o700)
		f, err := os.OpenFile(*logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		log.out = f
	}

	a := agent.New(e.cfg, fl.System())
	a.Log = log.printf
	a.AutostartCommand = agent.SelfCommand(SelfArgs)
	a.Start(ctx)
	srv := &http.Server{Handler: a.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(listener) }()
	url := "http://" + listener.Addr().String()
	log.printf("henosis %s is running; status page: %s", agent.Version, url)
	if !*background {
		_ = agent.OpenAppWindow(url)
	}
	select {
	case <-ctx.Done():
	case <-a.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	a.Close()
	log.printf("stopped")
	return nil
}

func (e *env) open(context.Context, []string) error {
	if !e.appRunning() {
		return errors.New("the app is not running; start it with \"henosis run\"")
	}
	return agent.OpenAppWindow(e.pageURL())
}

func (e *env) doctor(context.Context, []string) error {
	e.say("henosis %s on %s/%s", agent.Version, runtime.GOOS, runtime.GOARCH)
	e.say("settings: %s", e.cfg.Path())
	if e.cfg.SignedIn() {
		cl, _ := e.client()
		if me, err := cl.Me(); err != nil {
			e.say("server:   %s  NOT OK: %v", e.cfg.Server, err)
		} else {
			e.say("server:   %s  ok, signed in as %s", e.cfg.Server, me.Name)
		}
	} else {
		e.say("server:   not signed in")
	}
	if e.appRunning() {
		e.say("app:      running, %s", e.pageURL())
	} else {
		e.say("app:      not running")
	}
	for _, link := range e.cfg.Links {
		note := "ok"
		if info, err := os.Stat(link.Folder); err != nil || !info.IsDir() {
			note = "FOLDER MISSING"
		}
		e.say("project:  %s -> %s  %s", link.Name, link.Folder, note)
	}
	windows, running, err := fl.Windows()
	switch {
	case runtime.GOOS != "windows":
		e.say("FL Studio: detection works on Windows only")
	case err != nil:
		e.say("FL Studio: could not look for it: %v", err)
	case !running:
		e.say("FL Studio: not running")
	default:
		e.say("FL Studio: running, %d windows", len(windows))
		for _, w := range windows {
			kind, text := fl.ParseCaption(w.Caption)
			meaning := map[fl.CaptionKind]string{fl.CaptionOther: "other window", fl.CaptionNoProject: "no project",
				fl.CaptionFile: "project file " + text, fl.CaptionTitle: "project titled " + text}[kind]
			visible := "visible"
			if !w.Visible {
				visible = "hidden "
			}
			e.say("   %s  %-40q -> %s", visible, w.Caption, meaning)
		}
	}
	return nil
}

func (e *env) autostart(_ context.Context, args []string) error {
	if len(args) != 1 || (args[0] != "on" && args[0] != "off") {
		return usageError("henosis autostart on|off")
	}
	a := e.offline()
	defer a.Close()
	a.AutostartCommand = agent.SelfCommand(SelfArgs)
	// Prefer the windowless app when it sits next to this program.
	if self, err := os.Executable(); err == nil {
		for _, dir := range []string{filepath.Dir(self), filepath.Dir(filepath.Dir(self))} {
			if app := filepath.Join(dir, "Henosis.exe"); fileExists(app) && !strings.EqualFold(app, self) {
				a.AutostartCommand = `"` + app + `" --background`
				break
			}
		}
	}
	if err := a.SetAutostart(args[0] == "on"); err != nil {
		return err
	}
	if args[0] == "on" {
		e.say("Henosis now starts when you sign in to Windows:\n  %s", a.AutostartCommand)
	} else {
		e.say("Henosis no longer starts with Windows.")
	}
	return nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
