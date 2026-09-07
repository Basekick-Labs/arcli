// Package commands wires the arcli cobra command tree.
//
// Each top-level command lives in its own file. PR1 shipped `root` +
// `config`; PR2 added `query` + `write`; PR3 added `db` + `measurement`;
// PR4 added `import`; PR5 added `auth` + `ping`; PR6 added `cluster` +
// `compaction`; PR7 added `retention`, `cq`, `scheduler`; PR8 added
// `delete` and `backup`; PR9 added `logs` and `import stats`; PR10a added
// build metadata in --version and shell completion (completion.go).
package commands

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

// BuildInfo is what `arcli --version` reports. main() fills it from
// -ldflags or the toolchain's embedded build info.
type BuildInfo struct {
	Version string // "26.9.0", "dev"
	Commit  string // short SHA, "+dirty" suffix when the tree was modified
	Date    string // commit date, UTC RFC3339
}

// String renders "26.9.0 (commit 1a2b3c4 at 2026-09-07T12:00:00Z, go1.25 darwin/arm64)".
// Cobra prefixes it with "arcli version ".
func (b BuildInfo) String() string {
	var parts []string
	if b.Commit != "" {
		if b.Date != "" {
			parts = append(parts, "commit "+b.Commit+" at "+b.Date)
		} else {
			parts = append(parts, "commit "+b.Commit)
		}
	} else if b.Date != "" {
		parts = append(parts, b.Date)
	}
	parts = append(parts, fmt.Sprintf("%s %s/%s", runtime.Version(), runtime.GOOS, runtime.GOARCH))
	return b.Version + " (" + strings.Join(parts, ", ") + ")"
}

// NewRoot returns the arcli root command with all subcommands attached
// and shell completion wired.
func NewRoot(build BuildInfo) *cobra.Command {
	cliVersion = build.Version
	root := &cobra.Command{
		Use:   "arcli",
		Short: "Arc CLI — operator-facing client for Arc time-series databases",
		Long: `arcli talks to one or more Arc clusters via the HTTP API.

Manage multiple connections (dev/staging/prod) in ~/.arcli/config.toml
with one marked active. Override per-command with -c/--connection or the
ARC_CONNECTION / ARC_ENDPOINT / ARC_TOKEN env vars.

First-time setup:
    arcli config create --name local --endpoint http://localhost:8000 --token <T> --activate
    arcli config current
`,
		Version: build.String(),
		// Don't print usage on every error — most errors are runtime
		// (network, auth, server) where the usage text is noise.
		SilenceUsage: true,
	}
	// Declare --version ourselves, without a shorthand: cobra would
	// otherwise bind -v to it, and -v is reserved for a future verbose
	// mode (see the security checklist in .claude/CLAUDE.md).
	root.Flags().Bool("version", false, "version for arcli")

	root.AddCommand(
		newConfigCmd(),
		newQueryCmd(),
		newWriteCmd(),
		newDBCmd(),
		newMeasurementCmd(),
		newImportCmd(),
		newAuthCmd(),
		newPingCmd(),
		newClusterCmd(),
		newCompactionCmd(),
		newRetentionCmd(),
		newCQCmd(),
		newSchedulerCmd(),
		newDeleteCmd(),
		newBackupCmd(),
		newLogsCmd(),
	)
	installCompletions(root)
	return root
}
