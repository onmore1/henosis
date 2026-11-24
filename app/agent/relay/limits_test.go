package relay

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// A waiting socket whose relay went silent is noticed by the ping watchdog
// and closed, long before the slow renewal would replace it.
func TestSilentRelaySocketIsDropped(t *testing.T) {
	oldPing := pingEvery.Load()
	pingEvery.Store(int64(40 * time.Millisecond))
	defer pingEvery.Store(oldPing)

	relaySrv := newFakeRelay()
	defer relaySrv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, err := dialWS(ctx, buildURL(relaySrv.URL, "/accept", RouteCode(testCode)))
	if err != nil {
		t.Fatal(err)
	}
	tc := newTunnelConn(ws)
	defer tc.Close()

	// While the relay answers pings the socket stays up.
	if err := tc.waitPaired(400 * time.Millisecond); err == nil {
		t.Fatal("paired with nobody")
	} else {
		var ne interface{ Timeout() bool }
		if !errors.As(err, &ne) || !ne.Timeout() {
			t.Fatalf("a healthy waiting socket broke: %v", err)
		}
	}

	relaySrv.Mute()
	start := time.Now()
	err = tc.waitPaired(3 * time.Second)
	if err == nil {
		t.Fatal("paired with nobody")
	}
	var ne interface{ Timeout() bool }
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatalf("the silent socket was not dropped (still waiting after %v)", time.Since(start))
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("took %v to notice a silent relay", took)
	}
}

// A relay over its daily allowance is reported in words people understand,
// and the host does not hammer it.
func TestRelayOverItsAllowance(t *testing.T) {
	var hits atomic.Int32
	full := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte("error code: 1027"))
	}))
	defer full.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := Dial(ctx, full.URL, testCode); !errors.Is(err, ErrRelayBusy) {
		t.Fatalf("dial: got %v, want ErrRelayBusy", err)
	}

	oldRetry, oldMax := retryAfter, maxRetryAfter
	retryAfter, maxRetryAfter = 50*time.Millisecond, 400*time.Millisecond
	defer func() { retryAfter, maxRetryAfter = oldRetry, oldMax }()
	hits.Store(0)
	runCtx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		(&HostAcceptor{RelayURL: full.URL, Code: testCode, LocalAddr: "127.0.0.1:1", Logf: t.Logf}).Run(runCtx)
		close(done)
	}()
	time.Sleep(1500 * time.Millisecond)
	stop()
	<-done
	// Without backing off, two sockets every 50 ms would be ~60 tries.
	if n := hits.Load(); n > 16 || n < 3 {
		t.Fatalf("%d tries in 1.5 s against a full relay; want a few, backing off", n)
	}
}

func TestOverLimitIsRecognised(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{429, "", true},
		{500, "Exceeded allowed volume of requests in Durable Objects free tier.", true},
		{503, "error code: 1027", true},
		{400, "bad code", false},
		{500, "Internal Server Error", false},
		{404, "limit", false},
	}
	for _, c := range cases {
		if got := overLimit(c.status, c.body); got != c.want {
			t.Errorf("overLimit(%d, %q) = %v, want %v", c.status, c.body, got, c.want)
		}
	}
}

func TestCheckPong(t *testing.T) {
	relaySrv := newFakeRelay()
	defer relaySrv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := CheckPong(ctx, relaySrv.URL); err != nil {
		t.Fatalf("CheckPong against a relay that answers: %v", err)
	}
	relaySrv.Mute()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel2()
	if _, err := CheckPong(ctx2, relaySrv.URL); err == nil {
		t.Fatal("CheckPong passed against a relay that does not answer")
	}
}
