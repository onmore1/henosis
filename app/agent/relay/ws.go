// Package relay implements a minimal WebSocket client (RFC 6455 client side
// only, binary frames, stdlib-only) and uses it to tunnel Hénōsis HTTP
// traffic through a Cloudflare-hosted rendezvous Worker.
//
// This file is the WebSocket plumbing. host.go and dial.go use it.
package relay

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// WebSocket opcodes we care about.
const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

// wsConn is a client-side WebSocket connection over one underlying net.Conn.
// Writes from client to server are masked, as RFC 6455 requires.
type wsConn struct {
	raw     net.Conn
	br      *bufio.Reader
	writeMu sync.Mutex
	readMu  sync.Mutex
	closed  bool
	closeMu sync.Mutex
}

// dialWS opens a WebSocket to urlStr. The accepted subprotocol, if any, is
// whatever the server picked; we don't send one.
func dialWS(ctx context.Context, urlStr string) (*wsConn, error) {
	u, err := url.Parse(urlStr)
	if err != nil {
		return nil, err
	}
	var host string
	var dialNet string
	var useTLS bool
	switch u.Scheme {
	case "ws":
		dialNet = "tcp"
		host = u.Host
		if !strings.Contains(host, ":") {
			host += ":80"
		}
	case "wss":
		dialNet = "tcp"
		useTLS = true
		host = u.Host
		if !strings.Contains(host, ":") {
			host += ":443"
		}
	default:
		return nil, fmt.Errorf("relay: scheme %q is not ws or wss", u.Scheme)
	}
	d := &net.Dialer{Timeout: 15 * time.Second}
	raw, err := d.DialContext(ctx, dialNet, host)
	if err != nil {
		return nil, err
	}
	if useTLS {
		tc := tls.Client(raw, &tls.Config{ServerName: hostOnly(host)})
		if err := tc.HandshakeContext(ctx); err != nil {
			raw.Close()
			return nil, err
		}
		raw = tc
	}
	// Send the HTTP/1.1 upgrade request.
	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		raw.Close()
		return nil, err
	}
	keyB64 := base64.StdEncoding.EncodeToString(key)
	req := "GET " + u.RequestURI() + " HTTP/1.1\r\n" +
		"Host: " + u.Host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + keyB64 + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if deadline, ok := ctx.Deadline(); ok {
		raw.SetDeadline(deadline)
	} else {
		raw.SetDeadline(time.Now().Add(20 * time.Second))
	}
	if _, err := raw.Write([]byte(req)); err != nil {
		raw.Close()
		return nil, err
	}
	br := bufio.NewReader(raw)
	resp, err := http.ReadResponse(br, &http.Request{Method: "GET"})
	if err != nil {
		raw.Close()
		return nil, err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		raw.Close()
		return nil, fmt.Errorf("relay: upgrade failed: %s", resp.Status)
	}
	// Verify the server's Sec-WebSocket-Accept.
	h := sha1.New()
	h.Write([]byte(keyB64))
	h.Write([]byte("258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	want := base64.StdEncoding.EncodeToString(h.Sum(nil))
	got := resp.Header.Get("Sec-WebSocket-Accept")
	if got != want {
		raw.Close()
		return nil, errors.New("relay: bad Sec-WebSocket-Accept from server")
	}
	raw.SetDeadline(time.Time{})
	return &wsConn{raw: raw, br: br}, nil
}

func hostOnly(hostPort string) string {
	if h, _, err := net.SplitHostPort(hostPort); err == nil {
		return h
	}
	return hostPort
}

// readFrame reads one WebSocket frame. Returns opcode, payload bytes, fin.
func (c *wsConn) readFrame() (byte, []byte, bool, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	var h [2]byte
	if _, err := io.ReadFull(c.br, h[:]); err != nil {
		return 0, nil, false, err
	}
	fin := h[0]&0x80 != 0
	opcode := h[0] & 0x0F
	masked := h[1]&0x80 != 0
	plen := int64(h[1] & 0x7F)
	switch plen {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return 0, nil, false, err
		}
		plen = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return 0, nil, false, err
		}
		plen = int64(binary.BigEndian.Uint64(ext[:]))
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(c.br, mask[:]); err != nil {
			return 0, nil, false, err
		}
	}
	if plen < 0 || plen > 1<<24 {
		return 0, nil, false, fmt.Errorf("relay: frame too large (%d)", plen)
	}
	payload := make([]byte, plen)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return 0, nil, false, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return opcode, payload, fin, nil
}

// writeFrame writes one WebSocket frame. Client-to-server frames must be
// masked.
func (c *wsConn) writeFrame(opcode byte, payload []byte, fin bool) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	var hdr [14]byte
	hdr[0] = opcode
	if fin {
		hdr[0] |= 0x80
	}
	hdr[1] = 0x80 // masked
	n := 2
	l := len(payload)
	switch {
	case l < 126:
		hdr[1] |= byte(l)
	case l < 1<<16:
		hdr[1] |= 126
		binary.BigEndian.PutUint16(hdr[2:4], uint16(l))
		n = 4
	default:
		hdr[1] |= 127
		binary.BigEndian.PutUint64(hdr[2:10], uint64(l))
		n = 10
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	copy(hdr[n:n+4], mask[:])
	n += 4
	masked := make([]byte, l)
	for i := 0; i < l; i++ {
		masked[i] = payload[i] ^ mask[i%4]
	}
	if _, err := c.raw.Write(hdr[:n]); err != nil {
		return err
	}
	if l > 0 {
		if _, err := c.raw.Write(masked); err != nil {
			return err
		}
	}
	return nil
}

// close closes the underlying connection once.
func (c *wsConn) close(code uint16, reason string) {
	c.closeMu.Lock()
	if c.closed {
		c.closeMu.Unlock()
		return
	}
	c.closed = true
	c.closeMu.Unlock()
	payload := make([]byte, 2+len(reason))
	binary.BigEndian.PutUint16(payload[:2], code)
	copy(payload[2:], reason)
	_ = c.writeFrame(opClose, payload, true)
	c.raw.Close()
}
