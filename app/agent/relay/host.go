package relay

import (
	"context"
	"errors"
	"fmt"
	mrand "math/rand"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// renewEvery is how long a waiting accept socket lives before it is replaced
// by a fresh one. A variable so tests can make it short.
var renewEvery atomic.Int64

func init() { renewEvery.Store(int64(2 * time.Minute)) }

// HostAcceptor keeps a small pool of WebSockets open to the relay, each
// waiting to be paired with an incoming friend dial. When a pair happens,
// it opens a local TCP connection to localAddr (the Hénōsis HTTP server on
// 127.0.0.1:47800) and pipes bytes between the two.
//
// Start it with Run, cancel the context to stop. Safe for one instance per
// session code; Hénōsis creates one when hosting starts.
type HostAcceptor struct {
	RelayURL  string // "https://henosis-relay.xxx.workers.dev"
	Code      string // 32-char hex, derived from the session secret
	LocalAddr string // "127.0.0.1:47800"
	PoolSize  int    // how many acceptors to keep in flight (default 2)
	Logf      func(format string, args ...any)
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
				h.logf("relay accept: %v", err)
				// Back off a little before trying again, so a misconfigured
				// relay does not spin us flat out.
				select {
				case <-ctx.Done():
				case <-time.After(3 * time.Second):
				}
			}
		}()
	}
}

// acceptOnce waits on one accept socket. It calls paired as soon as a friend
// is paired with it, before piping, so the caller can open the next one.
func (h *HostAcceptor) acceptOnce(ctx context.Context, paired func()) error {
	dialCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	ws, err := dialWS(dialCtx, buildURL(h.RelayURL, "/accept", h.Code))
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
	// Paired — connect to the local HTTP server and pipe.
	local, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp", h.LocalAddr)
	if err != nil {
		t.Close()
		return fmt.Errorf("dial local %s: %w", h.LocalAddr, err)
	}
	h.logf("relay: paired a friend, piping to %s", h.LocalAddr)
	stop := context.AfterFunc(ctx, func() { t.Close(); local.Close() })
	defer stop()
	pipe(t, local)
	return nil
}

// pipe shuttles bytes between a and b, closes both when either side ends.
func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { io.Copy(a, b); done <- struct{}{} }()
	go func() { io.Copy(b, a); done <- struct{}{} }()
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
