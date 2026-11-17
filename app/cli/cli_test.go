package cli_test

// These tests build the real programs and run them as separate processes:
// one server and two computers, each with its own settings and its own app.

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

var bin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "henosis-bin")
	if err != nil {
		panic(err)
	}
	for _, name := range []string{"henosis", "henosis-server", "henosis-agent"} {
		out, err := exec.Command("go", "build", "-o", filepath.Join(dir, name), "henosis/cmd/"+name).CombinedOutput()
		if err != nil {
			panic(string(out))
		}
	}
	bin = dir
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

type computer struct {
	t      *testing.T
	config string
	port   int
	home   string
}

// run executes "henosis ARGS" on this computer, in dir.
func (c *computer) run(dir string, args ...string) (string, error) {
	cmd := exec.Command(filepath.Join(bin, "henosis"), args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HENOSIS_CONFIG="+c.config, "HOME="+c.home, "USERPROFILE="+c.home)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (c *computer) must(dir string, args ...string) string {
	c.t.Helper()
	out, err := c.run(dir, args...)
	if err != nil {
		c.t.Fatalf("henosis %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// start launches a program in the background and stops it with the test.
func start(t *testing.T, env []string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(filepath.Join(bin, name), args...)
	cmd.Env = append(os.Environ(), env...)
	logPath := filepath.Join(t.TempDir(), name+".log")
	out, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { out.Close() })
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			cmd.Process.Kill()
			t.Errorf("%s did not stop when asked", name)
		}
	})
	return logPath
}

func waitFor(t *testing.T, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func fileIs(path, content string) func() bool {
	return func() bool {
		data, err := os.ReadFile(path)
		return err == nil && string(data) == content
	}
}

func TestRealProgramsEndToEnd(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "server-data")
	serverPort := freePort(t)
	url := "http://127.0.0.1:" + strconv.Itoa(serverPort)
	start(t, nil, "henosis-server", "serve", "--data", data, "--addr", "127.0.0.1:"+strconv.Itoa(serverPort))

	token := func(name string) string {
		out, err := exec.Command(filepath.Join(bin, "henosis-server"), "adduser", "--data", data, name).CombinedOutput()
		match := regexp.MustCompile(`hn_[0-9a-f]{64}`).FindString(string(out))
		if err != nil || match == "" {
			t.Fatalf("adduser %s: %v %s", name, err, out)
		}
		return match
	}
	newComputer := func(name string) *computer {
		home := filepath.Join(root, name)
		os.MkdirAll(home, 0o755)
		return &computer{t: t, config: filepath.Join(home, "settings", "config.json"), port: freePort(t), home: home}
	}
	ann, bob := newComputer("ann"), newComputer("bob")
	waitFor(t, "the server to answer", func() bool {
		_, err := ann.run(root, "login", url, "hn_bad")
		out, _ := ann.run(root, "login", url, "hn_bad")
		return err != nil && strings.Contains(out, "did not accept")
	})
	annToken, bobToken := token("ann"), token("bob")

	// Before signing in, commands explain what to do.
	if out, err := ann.run(root, "projects"); err == nil || !strings.Contains(out, "henosis login") {
		t.Fatalf("projects before login: %v %s", err, out)
	}
	if out := ann.must(root, "login", url, annToken); !strings.Contains(out, "Signed in as ann") {
		t.Fatalf("login: %s", out)
	}
	bob.must(root, "login", url, bobToken)
	if out := ann.must(root, "whoami"); !strings.HasPrefix(out, "ann on "+url) {
		t.Fatalf("whoami: %s", out)
	}

	// Ann works by hand first: create from the current folder, sync, log.
	annFolder := filepath.Join(ann.home, "My Song")
	os.MkdirAll(annFolder, 0o755)
	os.WriteFile(filepath.Join(annFolder, "My Song.flp"), []byte("v1"), 0o644)
	if out := ann.must(annFolder, "create", "My Song"); !strings.Contains(out, "is linked to") {
		t.Fatalf("create: %s", out)
	}
	if out := ann.must(annFolder, "status"); !strings.Contains(out, "new here      My Song.flp") {
		t.Fatalf("status: %s", out)
	}
	if out := ann.must(annFolder, "sync"); !strings.Contains(out, "uploaded your changes") {
		t.Fatalf("sync: %s", out)
	}
	if out := ann.must(annFolder, "sync"); !strings.Contains(out, "already up to date") {
		t.Fatalf("second sync: %s", out)
	}
	if out := ann.must(filepath.Join(annFolder), "log"); !strings.Contains(out, "   1  ") || !strings.Contains(out, "Added My Song.flp") {
		t.Fatalf("log: %s", out)
	}
	if out, err := ann.run(root, "status"); err == nil || !strings.Contains(out, "not a Henosis project folder") {
		t.Fatalf("status outside a project: %v %s", err, out)
	}
	invite := regexp.MustCompile(`HN-[A-Z0-9]{4}-[A-Z0-9]{4}-[A-Z0-9]{4}`).FindString(ann.must(annFolder, "invite"))
	if invite == "" {
		t.Fatal("no invite code printed")
	}

	// Both start the background app. Bob joins through his running app.
	annLog := start(t, []string{"HENOSIS_CONFIG=" + ann.config, "HOME=" + ann.home}, "henosis", "run", "--background", "--port", strconv.Itoa(ann.port))
	start(t, []string{"HENOSIS_CONFIG=" + bob.config, "HOME=" + bob.home}, "henosis-agent", "--background", "--port", strconv.Itoa(bob.port))
	for _, c := range []*computer{ann, bob} {
		c := c
		waitFor(t, "the app to answer", func() bool {
			out, _ := c.run(root, "doctor")
			return strings.Contains(out, "app:      running")
		})
	}

	bobFolder := filepath.Join(bob.home, "Shared", "My Song")
	if out := bob.must(root, "join", invite, bobFolder); !strings.Contains(out, "is now synced in") {
		t.Fatalf("join through the running app: %s", out)
	}
	waitFor(t, "bob's first download", fileIs(filepath.Join(bobFolder, "My Song.flp"), "v1"))

	// Files travel both ways with nobody running a command.
	os.WriteFile(filepath.Join(bobFolder, "bass.wav"), []byte("bass"), 0o644)
	waitFor(t, "bob's file at ann's", fileIs(filepath.Join(annFolder, "bass.wav"), "bass"))
	os.WriteFile(filepath.Join(annFolder, "My Song.flp"), []byte("v2"), 0o644)
	waitFor(t, "ann's save at bob's", fileIs(filepath.Join(bobFolder, "My Song.flp"), "v2"))

	// Restore through the running app.
	if out := ann.must(annFolder, "restore", "1"); !strings.Contains(out, "version 1 is the newest version again") {
		t.Fatalf("restore: %s", out)
	}
	waitFor(t, "the restored version at bob's", fileIs(filepath.Join(bobFolder, "My Song.flp"), "v1"))
	if _, err := os.Stat(filepath.Join(bobFolder, "bass.wav")); !os.IsNotExist(err) {
		t.Fatal("the restore did not remove the later file")
	}
	if out, err := ann.run(annFolder, "restore", "99"); err == nil || !strings.Contains(out, "no version 99") {
		t.Fatalf("restoring an unknown version: %v %s", err, out)
	}
	if out := bob.must(root, "projects"); !strings.Contains(out, "My Song") || !strings.Contains(out, "editor") || !strings.Contains(out, bobFolder) {
		t.Fatalf("projects: %s", out)
	}
	if out := ann.must(root, "doctor"); !strings.Contains(out, "ok, signed in as ann") || !strings.Contains(out, "app:      running") {
		t.Fatalf("doctor: %s", out)
	}
	// Starting the app a second time does not start a second one.
	if out := ann.must(root, "run", "--background"); !strings.Contains(out, "already running") {
		t.Fatalf("second run: %s", out)
	}
	if text, _ := os.ReadFile(annLog); !strings.Contains(string(text), "status page: http://127.0.0.1:"+strconv.Itoa(ann.port)) {
		t.Fatalf("ann's app log: %s", text)
	}

	// Unlink keeps the files.
	bob.must(bobFolder, "unlink")
	os.WriteFile(filepath.Join(annFolder, "late.wav"), []byte("late"), 0o644)
	time.Sleep(3 * time.Second)
	if _, err := os.Stat(filepath.Join(bobFolder, "late.wav")); err == nil {
		t.Fatal("an unlinked folder still synced")
	}
	if _, err := os.Stat(filepath.Join(bobFolder, "My Song.flp")); err != nil {
		t.Fatal("unlinking removed files")
	}
	if out, code := ann.run(root, "frobnicate"); code == nil || !strings.Contains(out, "unknown command") {
		t.Fatalf("unknown command: %s", out)
	}
}

// TestNewVersionReplacesTheRunningOne: an older app still running in the
// background must not keep answering when a newer one is started.
func TestNewVersionReplacesTheRunningOne(t *testing.T) {
	root := t.TempDir()
	old := filepath.Join(root, "henosis-old")
	if out, err := exec.Command("go", "build", "-ldflags", "-X henosis/agent.Version=0.1.0", "-o", old, "henosis/cmd/henosis").CombinedOutput(); err != nil {
		t.Fatalf("building the old version: %v %s", err, out)
	}
	c := &computer{t: t, config: filepath.Join(root, "settings", "config.json"), port: freePort(t), home: root}
	env := append(os.Environ(), "HENOSIS_CONFIG="+c.config, "HOME="+root)
	port := strconv.Itoa(c.port)

	oldApp := exec.Command(old, "run", "--background", "--port", port)
	oldApp.Env = env
	if err := oldApp.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { oldApp.Wait(); close(exited) }()
	defer oldApp.Process.Kill()
	version := func() string {
		out, _ := c.run(root, "doctor")
		return out
	}
	waitFor(t, "the old app to answer", func() bool { return strings.Contains(version(), "app:      running") })

	logPath := start(t, []string{"HENOSIS_CONFIG=" + c.config, "HOME=" + root}, "henosis", "run", "--background", "--port", port)
	select {
	case <-exited:
	case <-time.After(15 * time.Second):
		t.Fatal("the old app kept running")
	}
	waitFor(t, "the new app to take over", func() bool {
		text, _ := os.ReadFile(logPath)
		return strings.Contains(string(text), "Replaced the running Henosis 0.1.0") && strings.Contains(string(text), "is running; status page")
	})
	// Starting the same version again changes nothing.
	if out := c.must(root, "run", "--background"); !strings.Contains(out, "already running") {
		t.Fatalf("second start of the same version: %s", out)
	}
}
