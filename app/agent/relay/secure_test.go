package relay

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The relay must learn neither the session secret nor what is sent.
func TestRelaySeesNoSecretAndNoData(t *testing.T) {
	relaySrv := newFakeRelay()
	defer relaySrv.Close()
	marker := strings.Repeat("kick-drum-sample-", 4000)
	startHost(t, relaySrv.URL, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Write(append([]byte(marker), body...))
	}))
	client := NewHTTPClient(relaySrv.URL, testCode)
	resp, err := client.Post("http://relay/v1/upload", "text/plain", strings.NewReader("snare-from-the-friend"))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != marker+"snare-from-the-friend" {
		t.Fatalf("the data did not come through intact (%d bytes)", len(got))
	}

	for _, c := range relaySrv.Codes() {
		if strings.Contains(c, testCode) {
			t.Errorf("the relay was told the session secret: %s", c)
		}
		if c != RouteCode(testCode) {
			t.Errorf("the relay was asked for %s, want the route code %s", c, RouteCode(testCode))
		}
	}
	seen := relaySrv.Carried()
	for _, plain := range []string{"kick-drum-sample", "snare-from-the-friend", "/v1/upload", "HTTP/1.1", testCode} {
		if bytes.Contains(seen, []byte(plain)) {
			t.Errorf("the relay could read %q", plain)
		}
	}
	if len(seen) == 0 {
		t.Fatal("the tap saw nothing; the test is not looking at the traffic")
	}
}

func TestRouteCodeIsStableAndDifferent(t *testing.T) {
	a := RouteCode(testCode)
	if a != RouteCode(testCode) {
		t.Fatal("RouteCode is not stable")
	}
	if err := ValidCode(a); err != nil {
		t.Fatalf("route code %q is not what the relay accepts: %v", a, err)
	}
	if a == testCode || a == RouteCode("ffeeddccbbaa99887766554433221100") {
		t.Fatal("route codes collide")
	}
}

// pair runs the two handshakes over an in-memory connection.
func pair(t *testing.T, dialSecret, hostSecret string, middle func(a, b net.Conn)) (net.Conn, net.Conn, error, error) {
	t.Helper()
	d, h := net.Pipe()
	if middle != nil {
		// dialer <-> h | middle | d2 <-> host
		d2, h2 := net.Pipe()
		go middle(h, d2)
		h = h2
	}
	type res struct {
		c   net.Conn
		err error
	}
	hc := make(chan res, 1)
	go func() { c, err := secureServer(h, hostSecret, 3*time.Second); hc <- res{c, err} }()
	dc, derr := secureClient(d, dialSecret, 3*time.Second)
	r := <-hc
	return dc, r.c, derr, r.err
}

func TestWrongSecretIsTurnedAway(t *testing.T) {
	_, _, derr, herr := pair(t, testCode, "ffeeddccbbaa99887766554433221100", nil)
	if !errors.Is(derr, ErrWrongSecret) || !errors.Is(herr, ErrWrongSecret) {
		t.Fatalf("want ErrWrongSecret on both sides, got dialer=%v host=%v", derr, herr)
	}
}

func TestOtherVersionIsNamed(t *testing.T) {
	d, h := net.Pipe()
	go func() {
		hello := make([]byte, 33)
		hello[0] = protocolVersion + 1
		h.Write(hello)
		io.Copy(io.Discard, h)
	}()
	_, err := secureClient(d, testCode, 2*time.Second)
	if !errors.Is(err, ErrOtherVersion) {
		t.Fatalf("got %v, want ErrOtherVersion", err)
	}
}

// A relay that changes one byte is caught: the reader gets an error, never
// the changed data.
func TestTamperingIsCaught(t *testing.T) {
	flip := make(chan struct{})
	middle := func(dialerSide, hostSide net.Conn) {
		// host -> dialer passes through; dialer -> host too, except that
		// after the handshake one bit is flipped.
		go io.Copy(dialerSide, hostSide)
		buf := make([]byte, 64<<10)
		for {
			n, err := dialerSide.Read(buf)
			if n > 0 {
				select {
				case <-flip:
					buf[n-1] ^= 1
				default:
				}
				if _, werr := hostSide.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
	dc, hc, derr, herr := pair(t, testCode, testCode, middle)
	if derr != nil || herr != nil {
		t.Fatalf("handshake: %v / %v", derr, herr)
	}
	close(flip)
	go dc.Write([]byte("a bar of drums"))
	buf := make([]byte, 64)
	n, err := hc.Read(buf)
	if err == nil {
		t.Fatalf("read changed data without an error: %q", buf[:n])
	}
}

// Large transfers arrive intact, in big but bounded relay messages.
func TestBigTransferInFewMessages(t *testing.T) {
	relaySrv := newFakeRelay()
	defer relaySrv.Close()
	big := make([]byte, 24<<20)
	rand.Read(big)
	startHost(t, relaySrv.URL, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			sum := sha256.New()
			io.Copy(sum, r.Body)
			w.Write(sum.Sum(nil))
			return
		}
		w.Write(big)
	}))
	client := NewHTTPClient(relaySrv.URL, testCode)

	resp, err := client.Get("http://relay/big")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || !bytes.Equal(got, big) {
		t.Fatalf("download: %d bytes, err %v", len(got), err)
	}
	resp, err = client.Post("http://relay/up", "application/octet-stream", bytes.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	sum, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	want := sha256.Sum256(big)
	if !bytes.Equal(sum, want[:]) {
		t.Fatal("upload arrived changed")
	}

	count, biggest := relaySrv.Messages()
	if biggest > maxChunk+64 {
		t.Errorf("a relay message was %d bytes, more than the %d limit", biggest, maxChunk)
	}
	// 48 MB moved; with the old 32 KB messages that was ~1500 messages.
	if count > 1100 {
		t.Errorf("%d relay messages for 48 MB; messages are too small", count)
	}
	t.Logf("48 MB in %d relay messages, biggest %d bytes", count, biggest)
}
