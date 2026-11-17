package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"henosis/client"
	"henosis/engine"
)

// syncBuffer is a bytes.Buffer safe to read while the server writes to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func command(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errs bytes.Buffer
	code := run(context.Background(), args, &out, &errs)
	return code, out.String(), errs.String()
}

var tokenLine = regexp.MustCompile(`Token: (hn_[0-9a-f]{64})`)

func addUser(t *testing.T, data, name string) string {
	t.Helper()
	code, out, errs := command(t, "adduser", "--data", data, name)
	match := tokenLine.FindStringSubmatch(out)
	if code != 0 || match == nil {
		t.Fatalf("adduser %s: exit %d\n%s\n%s", name, code, out, errs)
	}
	return match[1]
}

func TestCommands(t *testing.T) {
	data := filepath.Join(t.TempDir(), "data")
	if code, _, errs := command(t); code != 2 || !strings.Contains(errs, "henosis-server serve") {
		t.Errorf("no arguments: exit %d %q", code, errs)
	}
	if code, _, _ := command(t, "frobnicate"); code != 2 {
		t.Errorf("unknown command: exit %d", code)
	}
	if code, out, _ := command(t, "version"); code != 0 || strings.TrimSpace(out) != Version {
		t.Errorf("version: exit %d %q", code, out)
	}
	addUser(t, data, "ann")
	addUser(t, data, "bob")
	if code, _, errs := command(t, "adduser", "--data", data, "ann"); code != 1 || !strings.Contains(errs, "taken") {
		t.Errorf("duplicate account: exit %d %q", code, errs)
	}
	if code, _, _ := command(t, "adduser", "--data", data, "bad name!"); code != 1 {
		t.Errorf("invalid name: exit %d", code)
	}
	if code, _, _ := command(t, "adduser", "--data", data); code != 2 {
		t.Errorf("adduser without a name: exit %d", code)
	}
	if code, out, _ := command(t, "users", "--data", data); code != 0 || out != "ann\nbob\n" {
		t.Errorf("users: exit %d %q", code, out)
	}
	if code, out, _ := command(t, "cleanup", "--data", data); code != 0 || !strings.Contains(out, "Deleted 0 expired versions") {
		t.Errorf("cleanup: exit %d %q", code, out)
	}
	if code, _, _ := command(t, "serve", "--data", data, "--cert", "only-one"); code != 2 {
		t.Errorf("--cert without --key: exit %d", code)
	}
	// The token itself is never written to disk.
	users, _ := os.ReadFile(filepath.Join(data, "users.json"))
	if strings.Contains(string(users), "hn_") {
		t.Error("users.json contains a token")
	}
}

// TestServeEndToEnd runs the real server loop on a real port, adds accounts
// while it runs, syncs through it, stops it, and starts it again on the same
// data folder.
func TestServeEndToEnd(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "data")

	start := func() (url string, stop func() string) {
		ctx, cancel := context.WithCancel(context.Background())
		out := &syncBuffer{}
		done := make(chan int, 1)
		go func() { done <- run(ctx, []string{"serve", "--data", data, "--addr", "127.0.0.1:0"}, out, out) }()
		listening := regexp.MustCompile(`listening on (http://127\.0\.0\.1:\d+)`)
		deadline := time.Now().Add(10 * time.Second)
		for url == "" {
			if match := listening.FindStringSubmatch(out.String()); match != nil {
				url = match[1]
			} else if time.Now().After(deadline) {
				t.Fatalf("the server did not start:\n%s", out.String())
			} else {
				time.Sleep(10 * time.Millisecond)
			}
		}
		return url, func() string {
			cancel()
			select {
			case code := <-done:
				if code != 0 {
					t.Errorf("serve exited with %d:\n%s", code, out.String())
				}
			case <-time.After(15 * time.Second):
				t.Fatal("the server did not stop")
			}
			return out.String()
		}
	}

	url, stop := start()
	// Accounts are added while the server is already running.
	annToken := addUser(t, data, "ann")
	bobToken := addUser(t, data, "bob")
	ann := client.New(url, annToken, "ann-pc")
	bob := client.New(url, bobToken, "bob-pc")
	project, err := ann.CreateProject("My Song")
	if err != nil {
		t.Fatal(err)
	}
	code, err := ann.CreateInvite(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bob.AcceptInvite(code); err != nil {
		t.Fatal(err)
	}

	annFolder, bobFolder := filepath.Join(dir, "ann"), filepath.Join(dir, "bob")
	os.MkdirAll(annFolder, 0o755)
	os.MkdirAll(bobFolder, 0o755)
	os.WriteFile(filepath.Join(annFolder, "My Song.flp"), []byte("version 1"), 0o644)
	annRemote, bobRemote := ann.Remote(project.ID), bob.Remote(project.ID)
	annEngine := engine.Open(annFolder, engine.AutoLock{Remote: annRemote, Locker: annRemote}, "ann")
	if res, err := annEngine.Sync(engine.PushOptions{}); err != nil || !res.Pushed {
		t.Fatalf("push: %v %+v", err, res)
	}
	log := stop()
	for _, want := range []string{"ann POST /v1/projects -> 201", "bob POST /v1/invites/{code}/accept -> 200", "stopped"} {
		if !strings.Contains(log, want) {
			t.Errorf("the log lacks %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, code) || strings.Contains(log, annToken) {
		t.Error("the log contains an invite code or a token")
	}

	// A restart on the same data folder keeps accounts, projects and versions.
	url, stop = start()
	defer stop()
	bob = client.New(url, bobToken, "bob-pc")
	bobRemote = bob.Remote(project.ID)
	bobEngine := engine.Open(bobFolder, engine.AutoLock{Remote: bobRemote, Locker: bobRemote}, "bob")
	if res, err := bobEngine.Sync(engine.PushOptions{}); err != nil || !res.Pulled {
		t.Fatalf("pull after restart: %v %+v", err, res)
	}
	if got, _ := os.ReadFile(filepath.Join(bobFolder, "My Song.flp")); string(got) != "version 1" {
		t.Fatalf("bob's file after restart: %q", got)
	}
}
