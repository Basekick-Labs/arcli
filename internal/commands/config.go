// Connection management commands. Mirrors the InfluxDB v2 CLI's
// `influx config` UX deliberately so operators get a familiar workflow.
package commands

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/basekick-labs/arcli/internal/config"
	"github.com/basekick-labs/arcli/internal/output"
)

func newConfigCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "config",
		Short: "Manage arcli connection profiles (~/.arcli/config.toml)",
		Long: `Connections are named Arc endpoints + credentials. One is marked active and used
by default; override per-command with -c/--connection.

Stored in ~/.arcli/config.toml (mode 0600, plaintext tokens — same posture as
~/.aws/credentials). Honors ARCLI_CONFIG env var for CI/test overrides.`,
	}
	c.AddCommand(
		newConfigCreateCmd(),
		newConfigListCmd(),
		newConfigSetActiveCmd(),
		newConfigDeleteCmd(),
		newConfigCurrentCmd(),
	)
	return c
}

// ---- create ----------------------------------------------------------------

func newConfigCreateCmd() *cobra.Command {
	var (
		name            string
		endpoint        string
		token           string
		defaultDatabase string
		insecure        bool
		activate        bool
	)
	c := &cobra.Command{
		Use:   "create",
		Short: "Add a new connection profile",
		Example: `  arcli config create --name local --endpoint http://localhost:8000 --token ABC --activate
  arcli config create --name prod  --endpoint https://arc.prod.example.com --token XYZ --default-database metrics`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" || endpoint == "" || token == "" {
				return fmt.Errorf("--name, --endpoint, and --token are required")
			}
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if _, exists := cfg.Connections[name]; exists {
				return fmt.Errorf("connection %q already exists (use `arcli config delete %s` first, or pick a different name)", name, name)
			}
			cfg.Connections[name] = config.Connection{
				Endpoint:        endpoint,
				Token:           token,
				DefaultDatabase: defaultDatabase,
				InsecureTLS:     insecure,
			}
			// First-ever connection becomes active automatically — saves
			// the operator one command on first run. Otherwise honor --activate.
			if cfg.Active == "" || activate {
				cfg.Active = name
			}
			if err := cfg.Save(); err != nil {
				return err
			}
			path, _ := config.ConfigPath()
			fmt.Fprintf(cmd.OutOrStdout(), "Created connection %q at %s\n", name, path)
			if cfg.Active == name {
				fmt.Fprintf(cmd.OutOrStdout(), "Active connection is now %q\n", name)
			}
			return nil
		},
	}
	c.Flags().StringVar(&name, "name", "", "connection name (required)")
	c.Flags().StringVar(&endpoint, "endpoint", "", "Arc HTTP endpoint URL (required, e.g. http://localhost:8000)")
	c.Flags().StringVar(&token, "token", "", "API token from Arc's first-run banner (required)")
	c.Flags().StringVar(&defaultDatabase, "default-database", "", "default database for query/write commands (optional)")
	c.Flags().BoolVar(&insecure, "insecure", false, "skip TLS certificate verification for this connection")
	c.Flags().BoolVar(&activate, "activate", false, "make this the active connection")
	return c
}

// ---- list ------------------------------------------------------------------

func newConfigListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List configured connections",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if len(cfg.Connections) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No connections configured. Run `arcli config create --help`.")
				return nil
			}

			// Deterministic order so tests + screenshots match across runs.
			names := make([]string, 0, len(cfg.Connections))
			for n := range cfg.Connections {
				names = append(names, n)
			}
			sort.Strings(names)

			rows := make([][]string, 0, len(names))
			for _, n := range names {
				c := cfg.Connections[n]
				active := ""
				if n == cfg.Active {
					active = "*"
				}
				db := c.DefaultDatabase
				if db == "" {
					db = "-"
				}
				rows = append(rows, []string{active, n, c.Endpoint, config.RedactToken(c.Token), db})
			}
			return output.Table(cmd.OutOrStdout(),
				[]string{"ACTIVE", "NAME", "ENDPOINT", "TOKEN", "DEFAULT_DB"},
				rows,
			)
		},
	}
}

// ---- set-active ------------------------------------------------------------

func newConfigSetActiveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set-active <name>",
		Short: "Switch the active connection",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if _, ok := cfg.Connections[name]; !ok {
				return fmt.Errorf("connection %q not found (run `arcli config list`)", name)
			}
			if cfg.Active == name {
				fmt.Fprintf(cmd.OutOrStdout(), "Already active: %q\n", name)
				return nil
			}
			cfg.Active = name
			if err := cfg.Save(); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Active connection: %q\n", name)
			return nil
		},
	}
}

// ---- delete ----------------------------------------------------------------

func newConfigDeleteCmd() *cobra.Command {
	var yes bool
	c := &cobra.Command{
		Use:   "delete <name>",
		Short: "Remove a connection profile",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if _, ok := cfg.Connections[name]; !ok {
				return fmt.Errorf("connection %q not found", name)
			}
			if !yes {
				// Read one line of confirmation from stdin. Use os.Stdin
				// directly (not cmd.InOrStdin) so test scripts can also
				// pre-fill via t.Setenv-style stdin redirection.
				fmt.Fprintf(cmd.OutOrStdout(), "Delete connection %q? [y/N] ", name)
				var resp string
				_, _ = fmt.Fscanln(os.Stdin, &resp)
				if !strings.EqualFold(strings.TrimSpace(resp), "y") && !strings.EqualFold(strings.TrimSpace(resp), "yes") {
					fmt.Fprintln(cmd.OutOrStdout(), "Aborted.")
					return nil
				}
			}
			delete(cfg.Connections, name)
			// If we just deleted the active one, clear active so the
			// next command produces a clear "no active connection" error
			// rather than silently falling back to an unrelated entry.
			if cfg.Active == name {
				cfg.Active = ""
			}
			if err := cfg.Save(); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted connection %q\n", name)
			return nil
		},
	}
	c.Flags().BoolVarP(&yes, "yes", "y", false, "skip confirmation prompt")
	return c
}

// ---- current ---------------------------------------------------------------

func newConfigCurrentCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "current",
		Short: "Show the active connection (token redacted)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if cfg.Active == "" {
				return fmt.Errorf("no active connection (run `arcli config create --name NAME --endpoint URL --token TOKEN --activate`)")
			}
			c, ok := cfg.Connections[cfg.Active]
			if !ok {
				return fmt.Errorf("active connection %q referenced in config but not defined", cfg.Active)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "name:             %s\n", cfg.Active)
			fmt.Fprintf(cmd.OutOrStdout(), "endpoint:         %s\n", c.Endpoint)
			fmt.Fprintf(cmd.OutOrStdout(), "token:            %s\n", config.RedactToken(c.Token))
			if c.DefaultDatabase != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "default_database: %s\n", c.DefaultDatabase)
			}
			if c.InsecureTLS {
				fmt.Fprintf(cmd.OutOrStdout(), "insecure_tls:     true\n")
			}
			return nil
		},
	}
}
