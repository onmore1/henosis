package relay

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

// tunnelConn wraps a wsConn as a net.Conn. Writes send binary frames; Reads
// buffer the incoming frame payloads. Ping/pong/close are handled inline.
//
// Until the first non-control frame arrives, a "handshake" string (sent by
// the relay once pairing completes) is accepted and discarded, so the caller
// can tell pairing succeeded by doing a short Read before using the conn.
type tunnelConn struct {
	ws       *wsConn
	readBuf  bytes.Buffer
	readMu   sync.Mutex
	writeMu  sync.Mutex
	closeMu  sync.Mutex
	closed   bool
	rDeadl   time.Time
	wDeadl   time.Time
	pingTick *time.Ticker
	stop     chan struct{}
}

// newTunnelConn wraps ws and starts a background ping loop that keeps the
// relay's intermediate load balancers from reaping the socket.
func newTunnelConn(ws *wsConn) *tunnelConn {
	t := &tunnelConn{ws: ws, stop: make(chan struct{})}
	t.pingTick = time.NewTicker(25 * time.Second)
	go t.keepAlive()
	return t
}

func (t *tunnelConn) keepAlive() {
	for {
		select {
		case <-t.stop:
			return
		case <-t.pingTick.C:
			t.ws.writeMu.Lock()
			err := t.ws.writeFrame(opPing, nil, true)
			t.ws.writeMu.Unlock()
			if err != nil {
				return
			}
		}
	}
}

// Read fills p with bytes from the WebSocket's binary payload stream.
func (t *tunnelConn) Read(p []byte) (int, error) {
	t.readMu.Lock()
	defer t.readMu.Unlock()
	if t.readBuf.Len() > 0 {
		return t.readBuf.Read(p)
	}
	for {
		if !t.rDeadl.IsZero() {
			t.ws.raw.SetReadDeadline(t.rDeadl)
		}
		op, data, _, err := t.ws.readFrame()
		if err != nil {
			return 0, err
		}
		switch op {
		case opBinary, opContinuation:
			if len(data) == 0 {
				continue
			}
			n := copy(p, data)
			if n < len(data) {
				t.readBuf.Write(data[n:])
			}
			return n, nil
		case opText:
			// The relay sends "OK" as text once pairing happens; swallow it.
			if string(data) == "OK" {
				continue
			}
			// Any other text is ignored too.
			continue
		case opPing:
			t.ws.writeMu.Lock()
			_ = t.ws.writeFrame(opPong, data, true)
			t.ws.writeMu.Unlock()
		case opPong:
			// ignore
		case opClose:
			t.ws.raw.Close()
			return 0, io.EOF
		}
	}
}

// Write sends p as one binary frame. HTTP requests from net/http arrive in
// small-to-medium sized Write calls; we send each as its own frame.
func (t *tunnelConn) Write(p []byte) (int, error) {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	if !t.wDeadl.IsZero() {
		t.ws.raw.SetWriteDeadline(t.wDeadl)
	}
	if err := t.ws.writeFrame(opBinary, p, true); err != nil {
		return 0, err
	}
	return len(p), nil
}

// waitPaired reads one text frame; returns nil if it is "OK", else an error.
// Called once right after dialing to be sure the Durable Object paired us.
func (t *tunnelConn) waitPaired(timeout time.Duration) error {
	t.ws.raw.SetReadDeadline(time.Now().Add(timeout))
	defer t.ws.raw.SetReadDeadline(time.Time{})
	for {
		op, data, _, err := t.ws.readFrame()
		if err != nil {
			return err
		}
		if op == opText && string(data) == "OK" {
			return nil
		}
		if op == opClose {
			t.ws.raw.Close()
			return errors.New("relay: paired socket closed before use")
		}
		if op == opPing {
			t.ws.writeMu.Lock()
			_ = t.ws.writeFrame(opPong, data, true)
			t.ws.writeMu.Unlock()
			continue
		}
		// Ignore anything else until OK or close.
	}
}

func (t *tunnelConn) Close() error {
	t.closeMu.Lock()
	if t.closed {
		t.closeMu.Unlock()
		return nil
	}
	t.closed = true
	t.closeMu.Unlock()
	if t.pingTick != nil {
		t.pingTick.Stop()
	}
	close(t.stop)
	t.ws.close(1000, "")
	return nil
}

func (t *tunnelConn) LocalAddr() net.Addr                { return t.ws.raw.LocalAddr() }
func (t *tunnelConn) RemoteAddr() net.Addr               { return t.ws.raw.RemoteAddr() }
func (t *tunnelConn) SetDeadline(d time.Time) error      { t.rDeadl = d; t.wDeadl = d; return nil }
func (t *tunnelConn) SetReadDeadline(d time.Time) error  { t.rDeadl = d; return nil }
func (t *tunnelConn) SetWriteDeadline(d time.Time) error { t.wDeadl = d; return nil }
