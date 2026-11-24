package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	mrand "math/rand"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// renewEvery is how long a waiting accept socket lives before it is replaced
// by a fresh one. A variable so tests can make it short.
var renewEvery atomic.Int64

// Waiting sockets are replaced every renewEvery (plus some jitter), in case
// the relay lost one without closing it. Each replacement is a billed relay
// request, so this is slow; dead sockets are caught sooner by the ping
// watchdog in conn.go.
func init() { renewEvery.Store(int64(10 * time.Minute)) }

// HostAcceptor keeps a small pool of WebSockets open to the relay, each
// waiting to be paired with an incoming friend dial. When a pair happens,
// it opens a local TCP connection to localAddr (the Hénōsis HTTP server on
// 127.0.0.1:47800) and pipes bytes between the two.
//
// Start it with Run, cancel the context to stop. Safe for one instance per
// session code; Hénōsis creates one when hosting starts.
type HostAcceptor struct {
	RelayURL  string // "https://henosis-relay.xxx.workers.dev"
	Code      string // the session secret, 32 hex chars; the relay only sees RouteCode(Code)
	LocalAddr string // "127.0.0.1:47800"
	PoolSize  int    // how many acceptors to keep in flight (default 2)
	Logf      func(format string, args ...any)

	mu       sync.Mutex
	failures int
}

// retryAfter is how long the acceptor waits after the first failure; each
// failure in a row doubles it, up to maxRetryAfter. A relay that is down or
// over its daily allowance is not hammered.
var retryAfter, maxRetryAfter = 3 * time.Second, 5 * time.Minute

func (h *HostAcceptor) backoff(failed bool) time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !failed {
		h.failures = 0
		return 0
	}
	h.failures++
	wait := retryAfter
	for i := 1; i < h.failures && wait < maxRetryAfter; i++ {
		wait *= 2
	}
	if wait > maxRetryAfter {
		wait = maxRetryAfter
	}
	return wait
}

// Run blocks until ctx is cancelled. It maintains PoolSize goroutines, each
// holding one accept socket; as one gets paired and busy, it spins up a
// replacement so there is always an acceptor waiting.
func (h *HostAcceptor) Run(ctx context.Context) {
	if h.PoolSize == 0 {
		h.PoolSize = 2
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, h.PoolSize)
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			// The slot is given back as soon as this socket is paired (or
			// fails), so a fresh one starts waiting while this one pipes.
			var once sync.Once
			release := func() { once.Do(func() { <-sem }) }
			defer release()
			if err := h.acceptOnce(ctx, release); err != nil {
				// Back off before trying again, longer each time in a row,
				// so a relay that is down or full is not hammered.
				wait := h.backoff(true)
				h.logf("relay accept: %v (trying again in %s)", err, wait.Round(time.Second))
				select {
				case <-ctx.Done():
				case <-time.After(wait):
				}
				return
			}
			h.backoff(false)
		}()
	}
}

// acceptOnce waits on one accept socket. It calls paired as soon as a friend
// is paired with it, before piping, so the caller can open the next one.
func (h *HostAcceptor) acceptOnce(ctx context.Context, paired func()) error {
	dialCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	ws, err := dialWS(dialCtx, buildURL(h.RelayURL, "/accept", RouteCode(h.Code)))
	cancel()
	if err != nil {
		return fmt.Errorf("accept dial: %w", err)
	}
	t := newTunnelConn(ws)
	// A waiting socket is renewed every couple of minutes (with some jitter
	// so the pool's sockets do not all renew at once). A socket the relay
	// dropped, say because it was redeployed, is noticed within that time
	// at the latest, and usually at once.
	every := time.Duration(renewEvery.Load())
	wait := every + time.Duration(mrand.Int63n(int64(every/4)+1))
	result := make(chan error, 1)
	go func() { result <- t.waitPaired(wait) }()
	select {
	case <-ctx.Done():
		t.Close()
		return nil
	case err := <-result:
		if err != nil {
			t.Close()
			var ne net.Error
			if !errors.As(err, &ne) || !ne.Timeout() {
				h.logf("relay: the relay dropped a waiting connection (%v); opening a new one", err)
			}
			return nil
		}
	}
	paired()
	sc, err := secureServer(t, h.Code, 15*time.Second)
	if err != nil {
		t.Close()
		if errors.Is(err, ErrWrongSecret) || errors.Is(err, ErrOtherVersion) {
			h.logf("relay: turned away a connection: %v", err)
			return nil
		}
		return fmt.Errorf("handshake: %w", err)
	}
	// Paired — connect to the local HTTP server and pipe.
	local, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp", h.LocalAddr)
	if err != nil {
		sc.Close()
		return fmt.Errorf("dial local %s: %w", h.LocalAddr, err)
	}
	h.logf("relay: paired a friend, piping to %s", h.LocalAddr)
	stop := context.AfterFunc(ctx, func() { sc.Close(); local.Close() })
	defer stop()
	pipe(sc, local)
	return nil
}

// pipe shuttles bytes between a and b, closes both when either side ends.
func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	// Plain readers/writers, so io.CopyBuffer really uses the big buffer
	// (a big buffer makes big, and so fewer, relay messages).
	cp := func(dst io.Writer, src io.Reader) {
		io.CopyBuffer(struct{ io.Writer }{dst}, struct{ io.Reader }{src}, make([]byte, maxChunk))
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	a.Close()
	b.Close()
}

func (h *HostAcceptor) logf(format string, args ...any) {
	if h.Logf != nil {
		h.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}
