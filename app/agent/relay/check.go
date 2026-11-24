package relay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"
)

// CheckPong opens a waiting socket on the relay, pings it and reports how
// long the answer took. The ping watchdog relies on the relay answering
// pings, so a release should be checked against the real relay with this.
func CheckPong(ctx context.Context, relayURL string) (time.Duration, error) {
	secret := make([]byte, 16)
	rand.Read(secret)
	ws, err := dialWS(ctx, buildURL(relayURL, "/accept", RouteCode(hex.EncodeToString(secret))))
	if err != nil {
		return 0, err
	}
	defer ws.close(1000, "")
	start := time.Now()
	if err := ws.writeFrame(opPing, []byte("henosis"), true); err != nil {
		return 0, err
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(10 * time.Second)
	}
	ws.raw.SetReadDeadline(deadline)
	for {
		op, _, _, err := ws.readFrame()
		if err != nil {
			return 0, err
		}
		if op == opPong {
			return time.Since(start), nil
		}
		if op == opClose {
			return 0, errors.New("the relay closed the socket instead of answering")
		}
	}
}
