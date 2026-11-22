package relay

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"henosis/agent/relay/relaytest"
)

func newFakeRelay() *relaytest.Server { return relaytest.NewServer() }

const testCode = "00112233445566778899aabbccddeeff"

// startHost runs an HTTP server standing in for the host's Henosis server
// and a HostAcceptor that answers relay dials with it.
func startHost(t *testing.T, relayURL string, handler http.Handler) {
	t.Helper()
	backend := httptest.NewServer(handler)
	t.Cleanup(backend.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	acc := &HostAcceptor{RelayURL: relayURL, Code: testCode,
		LocalAddr: strings.TrimPrefix(backend.URL, "http://"), Logf: t.Logf}
	go acc.Run(ctx)
}

func TestRelayCarriesHTTP(t *testing.T) {
	relaySrv := newFakeRelay()
	defer relaySrv.Close()
	big := make([]byte, 3<<20) // 3 MB: several frames, 64-bit length headers
	rand.Read(big)
	startHost(t, relaySrv.URL, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/health":
			w.Write([]byte(`{"ok":true}`))
		case "/big":
			w.Write(big)
		case "/echo":
			io.Copy(w, r.Body)
		default:
			http.NotFound(w, r)
		}
	}))

	client := NewHTTPClient(relaySrv.URL, testCode)

	// Health check, as Connect does it.
	resp, err := client.Get("http://relay/v1/health")
	if err != nil {
		t.Fatalf("health through relay: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != `{"ok":true}` {
		t.Fatalf("health body %q", body)
	}

	// A big download arrives byte for byte.
	resp, err = client.Get("http://relay/big")
	if err != nil {
		t.Fatalf("big download: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Equal(got, big) {
		t.Fatalf("big download: got %d bytes, want %d, equal=%v", len(got), len(big), bytes.Equal(got, big))
	}

	// A big upload is echoed back intact.
	resp, err = client.Post("http://relay/echo", "application/octet-stream", bytes.NewReader(big))
	if err != nil {
		t.Fatalf("big upload: %v", err)
	}
	got, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Equal(got, big) {
		t.Fatalf("echo mismatch: %d bytes", len(got))
	}

	// Several requests at once each get their own tunnel.
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			msg := fmt.Sprintf("hello %d", i)
			resp, err := client.Post("http://relay/echo", "text/plain", strings.NewReader(msg))
			if err != nil {
				errs <- err
				return
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if string(b) != msg {
				errs <- fmt.Errorf("request %d got %q", i, b)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// Keep-alive pings must not stall a tunnel that stays open and in use.
func TestRelayPingsDoNotStall(t *testing.T) {
	old := pingEvery.Load()
	pingEvery.Store(int64(30 * time.Millisecond))
	defer pingEvery.Store(old)

	relaySrv := newFakeRelay()
	defer relaySrv.Close()
	startHost(t, relaySrv.URL, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(w, r.Body)
	}))
	client := NewHTTPClient(relaySrv.URL, testCode)
	deadline := time.Now().Add(1500 * time.Millisecond)
	n := 0
	for time.Now().Before(deadline) {
		done := make(chan error, 1)
		go func() {
			resp, err := client.Post("http://relay/", "text/plain", strings.NewReader("x"))
			if err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("request %d: %v", n, err)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("request %d hung: the tunnel stalled", n)
		}
		n++
		time.Sleep(100 * time.Millisecond) // let pings fire between requests
	}
	if n < 5 {
		t.Fatalf("only %d requests finished", n)
	}
}

// With no host listening, a dial fails instead of hanging forever.
func TestRelayNoHost(t *testing.T) {
	relaySrv := newFakeRelay()
	defer relaySrv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	if _, err := Dial(ctx, relaySrv.URL, testCode); err == nil {
		t.Fatal("dial succeeded with no host")
	}
	if time.Since(start) > 4*time.Second {
		t.Fatal("dial took too long to give up")
	}
}

// A relay that forgets the host's waiting sockets (as happened when it was
// redeployed) must not leave the host unreachable for long.
func TestHostRecoversFromAForgetfulRelay(t *testing.T) {
	old := renewEvery.Load()
	renewEvery.Store(int64(200 * time.Millisecond))
	defer renewEvery.Store(old)

	relaySrv := newFakeRelay()
	defer relaySrv.Close()
	startHost(t, relaySrv.URL, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hi"))
	}))
	deadline := time.Now().Add(3 * time.Second)
	for relaySrv.Waiting("accept") < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	relaySrv.Forget()
	if relaySrv.Waiting("accept") != 0 {
		t.Fatal("forget did not forget")
	}
	start := time.Now()
	resp, err := NewHTTPClient(relaySrv.URL, testCode).Get("http://relay/")
	if err != nil {
		t.Fatalf("after the relay forgot the host: %v", err)
	}
	resp.Body.Close()
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("reaching the host took %v", took)
	}
}
