package main

import (
	"os"
	"os/exec"
	"testing"

	"henosis/agent/relay/relaytest"
)

// The check passes against a relay that behaves like the real one.
func TestRelayCheckPassesAgainstAGoodRelay(t *testing.T) {
	srv := relaytest.NewServer()
	defer srv.Close()
	if os.Getenv("RELAYCHECK_CHILD") == "" {
		exe, _ := os.Executable()
		cmd := exec.Command(exe, "-test.run", "TestRelayCheckPassesAgainstAGoodRelay")
		cmd.Env = append(os.Environ(), "RELAYCHECK_CHILD="+srv.URL)
		out, err := cmd.CombinedOutput()
		t.Logf("%s", out)
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	os.Args = []string{"relaycheck", "-mb", "3", os.Getenv("RELAYCHECK_CHILD")}
	main()
}
