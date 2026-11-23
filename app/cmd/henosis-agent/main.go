// henosis-agent is the Henosis app (shipped as Henosis.exe): double-click it
// and its window opens. Syncing, the optional built-in server and the debug
// console all live in it. It is "henosis run" with a log file; build it with
// -ldflags "-H=windowsgui" on Windows so no console window appears.
package main

import (
	"context"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"

	"henosis/cli"
	"henosis/config"
)

func main() {
	// The window has no console, so a crash would vanish without a word.
	// Keep its report next to the log instead.
	dir := filepath.Dir(config.DefaultPath())
	if os.MkdirAll(dir, 0o700) == nil {
		if f, err := os.OpenFile(filepath.Join(dir, "crash.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			debug.SetCrashOutput(f, debug.CrashOptions{})
			f.Close()
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	cli.SelfArgs = "--background"
	args := []string{"run", "--log", filepath.Join(filepath.Dir(config.DefaultPath()), "agent.log")}
	os.Exit(cli.Main(ctx, append(args, os.Args[1:]...), io.Discard, io.Discard))
}
