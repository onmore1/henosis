// henosis is the command line of the Henosis app. See "henosis help".
package main

import (
	"context"
	"os"
	"os/signal"

	"henosis/cli"
)
//allllllllllllllllllah
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(cli.Main(ctx, os.Args[1:], os.Stdout, os.Stderr))
}
