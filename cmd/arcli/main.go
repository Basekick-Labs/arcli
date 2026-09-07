// Command arcli is the Arc command-line client.
//
// PR1 ships the command scaffold + the `config` subcommand tree
// (multi-connection store at ~/.arcli/config.toml, InfluxDB-CLI-style
// connection management). Subsequent PRs add: query/write/import,
// database admin, auth admin, cluster + ops, additional output formats.
//
// Version is injected at build time via:
//
//	go build -ldflags "-X main.version=1.0.0" ./cmd/arcli
package main

import (
	"os"

	"github.com/basekick-labs/arcli/internal/commands"
)

// version is injected by the release-build workflow via -ldflags. The
// default 'dev' is what `go build` produces for local development.
var version = "dev"

func main() {
	root := commands.NewRoot(version)
	if err := root.Execute(); err != nil {
		// Cobra has already printed the error to stderr; we only need a
		// non-zero exit code so shell `&&` chains behave.
		os.Exit(1)
	}
}
