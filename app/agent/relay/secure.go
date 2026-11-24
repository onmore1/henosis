package relay

// End-to-end encryption for relayed connections.
//
// The relay only ever sees two things: a route code, which is a hash of the
// session secret (so the secret itself never leaves the two computers), and
// encrypted bytes. Both apps know the secret; each sends a random nonce, and
// the keys for this one connection come from the secret and both nonces.
// After that every chunk is sealed with AES-256-GCM, so the relay can neither
// read nor change what passes through it, and a connection from someone who
// does not know the secret fails at once.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// protocolVersion is the first byte each side sends. Apps that speak a
// different version say so instead of failing with garbage.
const protocolVersion = 1

// maxChunk is the most plaintext sealed into one frame (and so one relay
// message). Bigger messages mean fewer billed relay requests.
const maxChunk = 256 << 10

// ErrWrongSecret means the far side does not know this session's secret.
var ErrWrongSecret = errors.New("relay: the other side has a different session code")

// ErrOtherVersion means the far side speaks another relay protocol.
var ErrOtherVersion = errors.New("relay: the other computer runs a different version of Hénōsis; update both to the same version")

// RouteCode is what the relay sees instead of the session secret: the first
// 16 bytes of a SHA-256 of it, in hex. Knowing it does not let anyone join.
func RouteCode(secret string) string {
	sum := sha256.Sum256([]byte("henosis relay route v1\x00" + secret))
	return hex.EncodeToString(sum[:16])
}

// secureConn seals everything written to it and opens everything read.
type secureConn struct {
	raw net.Conn

	wmu  sync.Mutex
	seal cipher.AEAD
	wseq uint64
	wbuf []byte

	rmu   sync.Mutex
	open  cipher.AEAD
	rseq  uint64
	plain []byte // opened bytes not yet handed to Read
}

// secureClient runs the dialing side of the handshake.
func secureClient(raw net.Conn, secret string, timeout time.Duration) (net.Conn, error) {
	return handshake(raw, secret, true, timeout)
}

// secureServer runs the accepting (host) side of the handshake.
func secureServer(raw net.Conn, secret string, timeout time.Duration) (net.Conn, error) {
	return handshake(raw, secret, false, timeout)
}

func handshake(raw net.Conn, secret string, dialer bool, timeout time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(timeout)
	raw.SetDeadline(deadline)
	defer raw.SetDeadline(time.Time{})

	mine := make([]byte, 1+32)
	mine[0] = protocolVersion
	if _, err := rand.Read(mine[1:]); err != nil {
		return nil, err
	}
	// Both sides send their hello at once, then read the other's.
	werr := make(chan error, 1)
	go func() { _, err := raw.Write(mine); werr <- err }()
	theirs := make([]byte, len(mine))
	if _, err := io.ReadFull(raw, theirs[:1]); err != nil {
		return nil, fmt.Errorf("relay: handshake: %w", err)
	}
	if theirs[0] != protocolVersion {
		return nil, ErrOtherVersion
	}
	if _, err := io.ReadFull(raw, theirs[1:]); err != nil {
		return nil, fmt.Errorf("relay: handshake: %w", err)
	}
	if err := <-werr; err != nil {
		return nil, fmt.Errorf("relay: handshake: %w", err)
	}

	dialNonce, hostNonce := mine[1:], theirs[1:]
	if !dialer {
		dialNonce, hostNonce = theirs[1:], mine[1:]
	}
	toHost, toDialer := deriveKeys(secret, dialNonce, hostNonce)
	sealKey, openKey := toHost, toDialer
	if !dialer {
		sealKey, openKey = toDialer, toHost
	}
	seal, err := newGCM(sealKey)
	if err != nil {
		return nil, err
	}
	open, err := newGCM(openKey)
	if err != nil {
		return nil, err
	}
	c := &secureConn{raw: raw, seal: seal, open: open}

	// Prove the keys match before any real data: each side sends a sealed
	// greeting and checks the other's.
	go func() { _, err := c.Write([]byte(greeting)); werr <- err }()
	got := make([]byte, len(greeting))
	if _, err := io.ReadFull(c, got); err != nil {
		if errors.Is(err, errBadFrame) {
			return nil, ErrWrongSecret
		}
		return nil, fmt.Errorf("relay: handshake: %w", err)
	}
	if string(got) != greeting {
		return nil, ErrWrongSecret
	}
	if err := <-werr; err != nil {
		return nil, fmt.Errorf("relay: handshake: %w", err)
	}
	return c, nil
}

const greeting = "henosis ok"

var errBadFrame = errors.New("relay: a message failed its check")

func deriveKeys(secret string, dialNonce, hostNonce []byte) (toHost, toDialer []byte) {
	// HKDF-SHA256 (RFC 5869), written out so it works on Go 1.23.
	salt := append(append([]byte{}, dialNonce...), hostNonce...)
	ext := hmac.New(sha256.New, salt)
	ext.Write([]byte(secret))
	prk := ext.Sum(nil)
	expand := func(info string) []byte {
		m := hmac.New(sha256.New, prk)
		m.Write([]byte(info))
		m.Write([]byte{1})
		return m.Sum(nil)
	}
	return expand("henosis relay v1 to host"), expand("henosis relay v1 to dialer")
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func nonceFor(seq uint64) []byte {
	n := make([]byte, 12)
	binary.BigEndian.PutUint64(n[4:], seq)
	return n
}

// Write seals p in chunks of at most maxChunk. Each chunk goes out in one
// write to the tunnel, which makes it one relay message.
func (c *secureConn) Write(p []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	written := 0
	for len(p) > 0 {
		n := len(p)
		if n > maxChunk {
			n = maxChunk
		}
		need := 4 + n + c.seal.Overhead()
		if cap(c.wbuf) < need {
			c.wbuf = make([]byte, need)
		}
		frame := c.wbuf[:4]
		frame = c.seal.Seal(frame, nonceFor(c.wseq), p[:n], nil)
		binary.BigEndian.PutUint32(frame[:4], uint32(len(frame)-4))
		if _, err := c.raw.Write(frame); err != nil {
			return written, err
		}
		c.wseq++
		written += n
		p = p[n:]
	}
	return written, nil
}

func (c *secureConn) Read(p []byte) (int, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	for len(c.plain) == 0 {
		var hdr [4]byte
		if _, err := io.ReadFull(c.raw, hdr[:]); err != nil {
			return 0, err
		}
		size := binary.BigEndian.Uint32(hdr[:])
		if size < uint32(c.open.Overhead()) || size > uint32(maxChunk+c.open.Overhead()) {
			return 0, errBadFrame
		}
		sealed := make([]byte, size)
		if _, err := io.ReadFull(c.raw, sealed); err != nil {
			return 0, unexpected(err)
		}
		plain, err := c.open.Open(sealed[:0], nonceFor(c.rseq), sealed, nil)
		if err != nil {
			return 0, errBadFrame
		}
		c.rseq++
		c.plain = plain
	}
	n := copy(p, c.plain)
	c.plain = c.plain[n:]
	return n, nil
}

func unexpected(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}

func (c *secureConn) Close() error                       { return c.raw.Close() }
func (c *secureConn) LocalAddr() net.Addr                { return c.raw.LocalAddr() }
func (c *secureConn) RemoteAddr() net.Addr               { return c.raw.RemoteAddr() }
func (c *secureConn) SetDeadline(t time.Time) error      { return c.raw.SetDeadline(t) }
func (c *secureConn) SetReadDeadline(t time.Time) error  { return c.raw.SetReadDeadline(t) }
func (c *secureConn) SetWriteDeadline(t time.Time) error { return c.raw.SetWriteDeadline(t) }
