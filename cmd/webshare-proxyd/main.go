// Command webshare-proxyd is the daemon for the Webshare Proxy Gateway.
// Phase 1 exposes only setup-side subcommands: version, add-key, sync.
// Listeners and the REST API arrive in later phases.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/guofan/webshare-proxy/internal/cli"
)

func main() {
	// Cancel ctx on SIGINT/SIGTERM so long-running subcommands (sync) can
	// shut down cleanly when the user hits Ctrl-C.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	os.Exit(cli.Run(ctx, os.Args[1:], cli.Deps{}))
}
