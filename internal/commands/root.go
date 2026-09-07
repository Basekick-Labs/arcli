// Package commands wires the arcli cobra command tree.
//
// Each top-level command lives in its own file. PR1 shipped `root` +
// `config`; PR2 added `query` + `write`; PR3 added `db` + `measurement`;
// PR4 added `import`; PR5 added `auth` + `ping`; PR6 added `cluster` +
// `compaction`; PR7 added `retention`, `cq`, `scheduler`; PR8 added
// `delete` and `backup`.
package commands

import "github.com/spf13/cobra"

// NewRoot returns the arcli root command with all subcommands attached.
// The version string is injected by main() from a -ldflags-built var.
func NewRoot(version string) *cobra.Command {
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
		Version: version,
		// Don't print usage on every error — most errors are runtime
		// (network, auth, server) where the usage text is noise.
		SilenceUsage: true,
	}

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
	)
	return root
}
