package relay

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"
)

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
			defer func() { <-sem }()
			if err := h.acceptOnce(ctx); err != nil {
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

func (h *HostAcceptor) acceptOnce(ctx context.Context) error {
	dialCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	ws, err := dialWS(dialCtx, buildURL(h.RelayURL, "/accept", h.Code))
	cancel()
	if err != nil {
		return fmt.Errorf("accept dial: %w", err)
	}
	t := newTunnelConn(ws)
	// Wait up to 10 minutes to be paired. The worker closes unpaired
	// sockets at 10 minutes anyway. If ctx cancels first, close the socket.
	paired := make(chan error, 1)
	go func() { paired <- t.waitPaired(10 * time.Minute) }()
	select {
	case <-ctx.Done():
		t.Close()
		return nil
	case err := <-paired:
		if err != nil {
			t.Close()
			return nil // nothing paired; normal
		}
	}
	// Paired — connect to the local HTTP server and pipe.
	local, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp", h.LocalAddr)
	if err != nil {
		t.Close()
		return fmt.Errorf("dial local %s: %w", h.LocalAddr, err)
	}
	h.logf("relay: paired a friend, piping to %s", h.LocalAddr)
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
