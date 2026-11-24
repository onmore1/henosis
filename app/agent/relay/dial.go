package relay

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Dial opens an encrypted, tunneled net.Conn to the host that waits on the
// relay for this session secret (32 hex characters). The relay is only told
// RouteCode(secret). Returns a net.Conn that can be handed to
// http.Transport.DialContext.
func Dial(ctx context.Context, relayURL, secret string) (net.Conn, error) {
	ws, err := dialWS(ctx, buildURL(relayURL, "/dial", RouteCode(secret)))
	if err != nil {
		return nil, fmt.Errorf("relay: dial: %w", err)
	}
	t := newTunnelConn(ws)
	wait := 20 * time.Second
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < wait {
		wait = time.Until(deadline)
	}
	if err := t.waitPaired(wait); err != nil {
		t.Close()
		return nil, fmt.Errorf("relay: no host listening for this code: %w", err)
	}
	sc, err := secureClient(t, secret, 15*time.Second)
	if err != nil {
		t.Close()
		return nil, err
	}
	return sc, nil
}

// NewHTTPClient returns an http.Client whose Transport dials a fresh
// WebSocket tunnel for every connection. The URL you give it can be any
// http:// URL; the host and path pass through to the far side's HTTP
// server. The URL host is ignored for routing (the code decides); it only
// serves as the Host: header.
func NewHTTPClient(relayURL, secret string) *http.Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return Dial(ctx, relayURL, secret)
		},
		DisableKeepAlives:     false,
		MaxIdleConns:          4,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		// Big buffers make big relay messages: fewer of them, fewer billed.
		WriteBufferSize: maxChunk,
		ReadBufferSize:  maxChunk,
	}
	return &http.Client{Transport: tr, Timeout: 60 * time.Second}
}

// buildURL turns "https://henosis-relay.xxx.workers.dev" + "/dial" + code
// into a wss:// URL with the ?code= query parameter.
func buildURL(relayURL, path, code string) string {
	base := strings.TrimRight(relayURL, "/")
	// Normalize scheme to ws/wss.
	switch {
	case strings.HasPrefix(base, "https://"):
		base = "wss://" + strings.TrimPrefix(base, "https://")
	case strings.HasPrefix(base, "http://"):
		base = "ws://" + strings.TrimPrefix(base, "http://")
	}
	u := base + path + "?code=" + url.QueryEscape(code)
	return u
}

// ValidCode reports whether s looks like the 32-char hex session secret the
// relay expects. (The outer HEN… session code decodes to a 16-byte secret,
// which we hex-encode when talking to the relay.)
func ValidCode(s string) error {
	if len(s) != 32 {
		return errors.New("relay code must be 32 hex characters")
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return errors.New("relay code must be hex")
		}
	}
	return nil
}
