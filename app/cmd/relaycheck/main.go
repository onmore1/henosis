// relaycheck tests a relay end to end: it pings a waiting socket, then runs
// a host and a friend through the relay on this one computer and moves data
// both ways, encrypted, checking every byte.
//
//	relaycheck [-mb 8] [-idle 0s] https://henosis-relay.example.workers.dev
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"henosis/agent/relay"
	"henosis/config"
)

var exitFailed = new(int)
var failed bool

func main() {
	run()
	if failed {
		os.Exit(1)
	}
}

func run() {
	mb := flag.Int("mb", 8, "megabytes to send each way")
	idle := flag.Duration("idle", 0, "also keep a waiting socket idle this long and check it survives")
	report := flag.String("report", "", "also write the results to this file")
	flag.Parse()
	if *report != "" {
		if f, err := os.Create(*report); err == nil {
			defer f.Close()
			r, w, _ := os.Pipe()
			stdout := os.Stdout
			os.Stdout = w
			done := make(chan struct{})
			go func() { io.Copy(io.MultiWriter(stdout, f), r); close(done) }()
			defer func() { w.Close(); <-done }()
		}
	}
	url := flag.Arg(0)
	if url == "" {
		url = config.DefaultRelay
	}
	if url == "" {
		fmt.Println("usage: relaycheck <relay URL>")
		os.Exit(2)
	}
	fail := func(step string, err error) {
		fmt.Printf("FAIL  %s: %v\n", step, err)
		panic(exitFailed)
	}
	defer func() {
		if r := recover(); r == exitFailed {
			failed = true
		} else if r != nil {
			panic(r)
		}
	}()
	fmt.Println("relay:", url)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	rtt, err := relay.CheckPong(ctx, url)
	cancel()
	if err != nil {
		fail("ping a waiting socket", err)
	}
	fmt.Printf("ok    the relay answers pings (%v)\n", rtt.Round(time.Millisecond))

	payload := make([]byte, *mb<<20)
	rand.Read(payload)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fail("local server", err)
	}
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			h := sha256.New()
			io.Copy(h, r.Body)
			w.Write(h.Sum(nil))
			return
		}
		w.Write(payload)
	}))
	raw := make([]byte, 16)
	rand.Read(raw)
	secret := hex.EncodeToString(raw)
	hostCtx, stopHost := context.WithCancel(context.Background())
	defer stopHost()
	go (&relay.HostAcceptor{RelayURL: url, Code: secret, LocalAddr: ln.Addr().String(),
		Logf: func(f string, a ...any) { fmt.Printf("      host: "+f+"\n", a...) }}).Run(hostCtx)
	time.Sleep(2 * time.Second)

	client := relay.NewHTTPClient(url, secret)
	client.Timeout = 5 * time.Minute
	start := time.Now()
	resp, err := client.Get("http://relay/data")
	if err != nil {
		fail("download through the relay", err)
	}
	got, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || !bytes.Equal(got, payload) {
		fail("download through the relay", fmt.Errorf("%d of %d bytes arrived intact (%v)", len(got), len(payload), err))
	}
	took := time.Since(start)
	fmt.Printf("ok    %d MB down in %v (%.1f MB/s)\n", *mb, took.Round(time.Millisecond), float64(*mb)/took.Seconds())

	start = time.Now()
	resp, err = client.Post("http://relay/up", "application/octet-stream", bytes.NewReader(payload))
	if err != nil {
		fail("upload through the relay", err)
	}
	sum, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	want := sha256.Sum256(payload)
	if !bytes.Equal(sum, want[:]) {
		fail("upload through the relay", fmt.Errorf("the data arrived changed"))
	}
	took = time.Since(start)
	fmt.Printf("ok    %d MB up in %v (%.1f MB/s)\n", *mb, took.Round(time.Millisecond), float64(*mb)/took.Seconds())

	wrong := relay.NewHTTPClient(url, "ffffffffffffffffffffffffffffffff")
	wctx, wcancel := context.WithTimeout(context.Background(), 8*time.Second)
	req, _ := http.NewRequestWithContext(wctx, "GET", "http://relay/data", nil)
	if _, err := wrong.Do(req); err == nil {
		fail("a wrong session code", fmt.Errorf("it got in"))
	}
	wcancel()
	fmt.Println("ok    a wrong session code gets nowhere")

	if *idle > 0 {
		fmt.Printf("      leaving the host idle for %v...\n", *idle)
		time.Sleep(*idle)
		resp, err := client.Get("http://relay/data")
		if err != nil {
			fail("reaching an idle host", err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		fmt.Printf("ok    the host was still reachable after %v idle\n", *idle)
	}
	fmt.Println("PASS")
}
