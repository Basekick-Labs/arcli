// Command arcli is the Arc command-line client.
//
// Version is injected at build time via:
//
//	go build -ldflags "-X main.version=1.0.0" ./cmd/arcli
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"

	"github.com/basekick-labs/arcli/internal/commands"
)

// version is injected by the release-build workflow via -ldflags. The
// default 'dev' is what `go build` produces for local development.
var version = "dev"

func main() {
	// A SIGINT/SIGTERM cancels the root context, which every command
	// derives its per-request deadlines and polling loops from, so a
	// ctrl-C during `--wait` or a long synchronous call stops cleanly
	// instead of by process death. A second signal restores the default
	// disposition and kills the process.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	var exitCode atomic.Int32 // 128+signal, set before ctx is cancelled
	go func() {
		s := <-sigs
		if s == syscall.SIGTERM {
			exitCode.Store(143)
		} else {
			exitCode.Store(130)
		}
		cancel()
		// Restore the default disposition so a second signal terminates
		// even a command blocked on a prompt or on reading stdin (paths
		// that cannot observe the context).
		signal.Stop(sigs)
	}()

	root := commands.NewRoot(version)
	root.SilenceErrors = true
	err := root.ExecuteContext(ctx)
	if err == nil {
		return
	}
	if code := exitCode.Load(); code != 0 || errors.Is(err, context.Canceled) {
		// The one line that matters: the server does not stop because
		// the client did. 130/143 are the conventional SIGINT/SIGTERM
		// exit statuses.
		if code == 0 {
			code = 130
		}
		fmt.Fprintln(os.Stderr, "interrupted; any operation already accepted by the server continues there")
		os.Exit(int(code))
	}
	fmt.Fprintln(os.Stderr, "Error:", err)
	os.Exit(1)
}
