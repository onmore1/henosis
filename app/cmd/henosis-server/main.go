// henosis-server is the Henosis server: one program, one data folder.
//
//	henosis-server serve   [--data DIR] [--addr :8787] [--cert FILE --key FILE]
//	henosis-server adduser [--data DIR] NAME
//	henosis-server users   [--data DIR]
//	henosis-server cleanup [--data DIR]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"time"

	"henosis/server"
	"henosis/store"
)

// Version is the release this binary was built from.
var Version = "0.5.0"

const usage = `henosis-server ` + "%s" + `

  henosis-server serve   [--data DIR] [--addr :8787] [--cert FILE --key FILE]
      Run the server. Everything it stores lives in the data folder.
  henosis-server adduser [--data DIR] NAME
      Create an account and print its token. The token is shown once.
      This also works while the server is running.
  henosis-server users   [--data DIR]
      List the accounts.
  henosis-server cleanup [--data DIR]
      Delete expired versions now (the running server does this every hour).

The default data folder is ./henosis-data.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintf(stderr, usage, Version)
		return 2
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(stderr)
	data := flags.String("data", "henosis-data", "the data folder")
	addr := flags.String("addr", ":8787", "the address to listen on")
	cert := flags.String("cert", "", "TLS certificate file (optional)")
	key := flags.String("key", "", "TLS key file (optional)")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}

	switch args[0] {
	case "version":
		fmt.Fprintln(stdout, Version)
		return 0
	case "help", "-h", "--help":
		fmt.Fprintf(stdout, usage, Version)
		return 0
	case "serve", "adduser", "users", "cleanup":
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", args[0])
		fmt.Fprintf(stderr, usage, Version)
		return 2
	}

	st, err := store.Open(*data)
	if err != nil {
		return fail(err)
	}
	switch args[0] {
	case "adduser":
		if flags.NArg() != 1 {
			fmt.Fprintln(stderr, "usage: henosis-server adduser [--data DIR] NAME")
			return 2
		}
		user, token, err := st.AddUser(flags.Arg(0))
		if err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "Account %q created.\nToken: %s\nGive this token to %s. It is not stored and cannot be shown again.\n", user.Name, token, user.Name)
		return 0
	case "users":
		for _, user := range st.Users() {
			fmt.Fprintln(stdout, user.Name)
		}
		return 0
	case "cleanup":
		snapshots, blobs := st.Cleanup()
		fmt.Fprintf(stdout, "Deleted %d expired versions and %d unused files.\n", snapshots, blobs)
		return 0
	}

	// serve
	if (*cert == "") != (*key == "") {
		fmt.Fprintln(stderr, "--cert and --key go together")
		return 2
	}
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		return fail(err)
	}
	var logMu sync.Mutex
	logf := func(format string, a ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		fmt.Fprintf(stdout, time.Now().Format("2006-01-02 15:04:05 ")+format+"\n", a...)
	}
	api := server.New(st)
	api.Log = logf
	httpServer := &http.Server{Handler: api, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}

	scheme := "http"
	if *cert != "" {
		scheme = "https"
	}
	logf("henosis-server %s, data folder %s, %d accounts", Version, *data, len(st.Users()))
	logf("listening on %s://%s", scheme, listener.Addr())
	if len(st.Users()) == 0 {
		logf("there are no accounts yet: run \"henosis-server adduser NAME\"")
	}

	go func() { // delete expired versions at start and then every hour
		for {
			if snapshots, blobs := st.Cleanup(); snapshots+blobs > 0 {
				logf("cleanup: deleted %d expired versions and %d unused files", snapshots, blobs)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Hour):
			}
		}
	}()

	served := make(chan error, 1)
	go func() {
		if *cert != "" {
			served <- httpServer.ServeTLS(listener, *cert, *key)
		} else {
			served <- httpServer.Serve(listener)
		}
	}()
	select {
	case err := <-served:
		return fail(err)
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdown); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return fail(err)
	}
	logf("stopped")
	return 0
}
