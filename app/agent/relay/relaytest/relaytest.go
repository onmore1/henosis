// Package relaytest runs a stand-in for the Cloudflare relay Worker
// (worker/src/index.js) inside a test: it pairs an /accept WebSocket with a
// /dial WebSocket for the same code, sends "OK" to both, then forwards every
// message from one side to the other, re-framed as a server sends it.
package relaytest

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// Server is the stand-in relay. Its URL is what Henosis takes as the relay
// URL. Close it when the test ends.
type Server struct {
	*httptest.Server
	r *relay
}

// NewServer starts the stand-in relay.
func NewServer() *Server {
	f := &relay{waiting: map[string]map[string][]*sock{}}
	return &Server{Server: httptest.NewServer(http.HandlerFunc(f.serve)), r: f}
}

// Forget makes the relay lose every waiting socket without closing it, the
// way a redeployed or crashed relay can strand them.
func (s *Server) Forget() {
	s.r.mu.Lock()
	s.r.waiting = map[string]map[string][]*sock{}
	s.r.mu.Unlock()
}

// Waiting counts the sockets waiting on a side ("accept" or "dial").
func (s *Server) Waiting(side string) int {
	s.r.mu.Lock()
	defer s.r.mu.Unlock()
	n := 0
	for _, sides := range s.r.waiting {
		n += len(sides[side])
	}
	return n
}

type relay struct {
	mu      sync.Mutex
	waiting map[string]map[string][]*sock // code -> side -> waiting sockets
}

type sock struct {
	conn net.Conn
	br   *bufio.Reader
	wmu  sync.Mutex
}

func (f *relay) serve(w http.ResponseWriter, r *http.Request) {
	side := strings.TrimPrefix(r.URL.Path, "/")
	code := strings.ToLower(r.URL.Query().Get("code"))
	if (side != "accept" && side != "dial") || len(code) != 32 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	h := sha1.New()
	h.Write([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	conn, brw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	fmt.Fprintf(brw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n",
		base64.StdEncoding.EncodeToString(h.Sum(nil)))
	brw.Flush()
	me := &sock{conn: conn, br: brw.Reader}

	other := map[string]string{"accept": "dial", "dial": "accept"}[side]
	f.mu.Lock()
	if f.waiting[code] == nil {
		f.waiting[code] = map[string][]*sock{}
	}
	if q := f.waiting[code][other]; len(q) > 0 {
		peer := q[0]
		f.waiting[code][other] = q[1:]
		f.mu.Unlock()
		me.send(0x1, []byte("OK"))
		peer.send(0x1, []byte("OK"))
		go forward(me, peer)
		go forward(peer, me)
		return
	}
	f.waiting[code][side] = append(f.waiting[code][side], me)
	f.mu.Unlock()
}

func (s *sock) send(op byte, payload []byte) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	hdr := []byte{0x80 | op}
	switch l := len(payload); {
	case l < 126:
		hdr = append(hdr, byte(l))
	case l < 1<<16:
		hdr = append(hdr, 126)
		hdr = binary.BigEndian.AppendUint16(hdr, uint16(l))
	default:
		hdr = append(hdr, 127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(l))
	}
	_, err := s.conn.Write(append(hdr, payload...))
	return err
}

// read reads one (masked) client frame.
func (s *sock) read() (byte, []byte, error) {
	var h [2]byte
	if _, err := io.ReadFull(s.br, h[:]); err != nil {
		return 0, nil, err
	}
	op, n := h[0]&0x0F, uint64(h[1]&0x7F)
	switch n {
	case 126:
		var e [2]byte
		if _, err := io.ReadFull(s.br, e[:]); err != nil {
			return 0, nil, err
		}
		n = uint64(binary.BigEndian.Uint16(e[:]))
	case 127:
		var e [8]byte
		if _, err := io.ReadFull(s.br, e[:]); err != nil {
			return 0, nil, err
		}
		n = binary.BigEndian.Uint64(e[:])
	}
	var mask [4]byte
	if h[1]&0x80 != 0 {
		if _, err := io.ReadFull(s.br, mask[:]); err != nil {
			return 0, nil, err
		}
	}
	data := make([]byte, n)
	if _, err := io.ReadFull(s.br, data); err != nil {
		return 0, nil, err
	}
	if h[1]&0x80 != 0 {
		for i := range data {
			data[i] ^= mask[i%4]
		}
	}
	return op, data, nil
}

// forward passes frames from src to dst, answering pings itself the way
// Cloudflare does, and closes both when either side goes.
func forward(src, dst *sock) {
	defer src.conn.Close()
	defer dst.conn.Close()
	for {
		op, data, err := src.read()
		if err != nil {
			return
		}
		switch op {
		case 0x9: // ping
			src.send(0xA, data)
		case 0xA: // pong
		case 0x8: // close
			dst.send(0x8, data)
			return
		default:
			if dst.send(op, data) != nil {
				return
			}
		}
	}
}
