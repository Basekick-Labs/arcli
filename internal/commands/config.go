// Connection management commands. Mirrors the InfluxDB v2 CLI's
// `influx config` UX deliberately so operators get a familiar workflow.
package commands

import (
	"bufio"
	"fmt"
	"io"
	"net/url"
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
		newConfigUpdateCmd(),
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
		tokenStdin      bool
	)
	c := &cobra.Command{
		Use:   "create",
		Short: "Add a new connection profile",
		Example: `  arcli config create --name local --endpoint http://localhost:8000 --token ABC --activate
  arcli config create --name prod  --endpoint https://arc.prod.example.com --token XYZ --default-database metrics`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// --token / --token-stdin exclusivity is enforced by cobra
			// before RunE. Validate the other flags before draining stdin
			// so a typo does not consume (and discard) the piped secret.
			if name == "" || endpoint == "" || (token == "" && !tokenStdin) {
				return fmt.Errorf("--name, --endpoint, and --token (or --token-stdin) are required")
			}
			if err := validateEndpoint(endpoint); err != nil {
				return err
			}
			if strings.HasPrefix(name, "(") {
				// "(flags)" / "(env)" are Resolve's ad-hoc sentinels.
				return fmt.Errorf("connection name must not start with \"(\"")
			}
			if tokenStdin {
				t, err := readTokenFromStdin(cmd)
				if err != nil {
					return err
				}
				token = t
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
			minted := cfg.InstallationID == ""
			if err := cfg.Save(); err != nil {
				return err
			}
			path, _ := config.ConfigPath()
			fmt.Fprintf(cmd.OutOrStdout(), "Created connection %q at %s\n", name, path)
			if minted {
				if cfg.OutboundInstallationID() == "" {
					fmt.Fprintf(cmd.OutOrStdout(), "Generated installation id %s (not sent: DO_NOT_TRACK or send_installation_id = false is in effect)\n", cfg.InstallationID)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "Generated installation id %s; arcli sends it to the Arc servers you connect to (see README, Privacy). Disable with DO_NOT_TRACK=1 or send_installation_id = false.\n", cfg.InstallationID)
				}
			}
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
	c.Flags().BoolVar(&tokenStdin, "token-stdin", false, "read the token from the first line of stdin (keeps it out of shell history and ps)")
	c.MarkFlagsMutuallyExclusive("token", "token-stdin")
	return c
}

// readTokenFromStdin reads one line from stdin as the token. It is for
// pipes and files (`pass show arc | arcli config create --token-stdin
// ...`); a terminal is refused because the token would be echoed into
// the scrollback, which defeats the point of the flag. CR/LF and
// surrounding whitespace are trimmed; an empty line is an error.
func readTokenFromStdin(cmd *cobra.Command) (string, error) {
	if f, ok := cmd.InOrStdin().(*os.File); ok && !isPipe(f) && !isDevNull(f) {
		return "", fmt.Errorf("--token-stdin: stdin is a terminal; pipe the token in (e.g. `pass show arc | arcli config create --token-stdin ...`)")
	}
	line, err := bufio.NewReader(io.LimitReader(cmd.InOrStdin(), 4096)).ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("--token-stdin: no token on stdin")
	}
	if len(line) >= 4096 && !strings.HasSuffix(line, "\n") {
		return "", fmt.Errorf("--token-stdin: line exceeds 4096 bytes; not a token")
	}
	t := strings.TrimSpace(line)
	if t == "" {
		return "", fmt.Errorf("--token-stdin: empty token on stdin")
	}
	// Arc tokens are URL-safe base64, so anything outside visible ASCII
	// is a paste error. Refusing it here also keeps C1/bidi runes out of
	// the config file, where RedactToken would print the first and last
	// characters unscrubbed.
	for i := 0; i < len(t); i++ {
		if t[i] < 0x21 || t[i] > 0x7e {
			return "", fmt.Errorf("--token-stdin: token must be printable ASCII without spaces")
		}
	}
	return t, nil
}

// ---- update ----------------------------------------------------------------

func newConfigUpdateCmd() *cobra.Command {
	var (
		endpoint        string
		token           string
		defaultDatabase string
		insecure        bool
		tokenStdin      bool
	)
	c := &cobra.Command{
		Use:   "update <name>",
		Short: "Change fields of an existing connection profile",
		Long: `Change one or more fields of an existing connection profile. Only the
flags you pass are changed; --default-database "" clears the default.

Typical use is refreshing a stored token after "arcli auth token rotate".`,
		Example: `  arcli config update prod --token NEW-TOKEN
  arcli config update local --default-database metrics --insecure=false`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			fl := cmd.Flags()
			if !fl.Changed("endpoint") && !fl.Changed("token") && !tokenStdin && !fl.Changed("default-database") && !fl.Changed("insecure") {
				return fmt.Errorf("nothing to update (pass at least one of --endpoint, --token, --token-stdin, --default-database, --insecure)")
			}
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			conn, ok := cfg.Connections[name]
			if !ok {
				return fmt.Errorf("connection %q not found (run `arcli config list`)", name)
			}
			if tokenStdin {
				// Exclusivity with --token is enforced by cobra before RunE.
				t, err := readTokenFromStdin(cmd)
				if err != nil {
					return err
				}
				token = t
			}
			var changed []string
			if fl.Changed("endpoint") {
				if err := validateEndpoint(endpoint); err != nil {
					return err
				}
				conn.Endpoint = endpoint
				changed = append(changed, "endpoint")
			}
			if fl.Changed("token") || tokenStdin {
				if token == "" {
					return fmt.Errorf("--token must not be empty")
				}
				conn.Token = token
				changed = append(changed, "token")
			}
			if fl.Changed("default-database") {
				conn.DefaultDatabase = defaultDatabase
				changed = append(changed, "default_database")
			}
			if fl.Changed("insecure") {
				conn.InsecureTLS = insecure
				changed = append(changed, "insecure_tls")
			}
			cfg.Connections[name] = conn
			if err := cfg.Save(); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Updated connection %q (%s)\n", name, strings.Join(changed, ", "))
			return nil
		},
	}
	c.Flags().StringVar(&endpoint, "endpoint", "", "new Arc HTTP endpoint URL")
	c.Flags().StringVar(&token, "token", "", "new API token")
	c.Flags().StringVar(&defaultDatabase, "default-database", "", "new default database (\"\" clears)")
	c.Flags().BoolVar(&insecure, "insecure", false, "skip TLS certificate verification for this connection (use --insecure=false to re-enable)")
	c.Flags().BoolVar(&tokenStdin, "token-stdin", false, "read the new token from the first line of stdin")
	c.MarkFlagsMutuallyExclusive("token", "token-stdin")
	return c
}

// validateEndpoint rejects values that cannot be an Arc base URL. The
// check is deliberately minimal (scheme + host); the first request
// against a wrong endpoint produces the definitive error.
func validateEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("--endpoint must be an http:// or https:// URL (got %q)", endpoint)
	}
	if u.User != nil {
		// Credentials belong in --token; a userinfo component would be
		// echoed raw by every command that prints the endpoint.
		return fmt.Errorf("--endpoint must not contain credentials (user:password@)")
	}
	return nil
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
				fmt.Fprintf(cmd.ErrOrStderr(), "Delete connection %q? [y/N] ", name)
				var resp string
				_, _ = fmt.Fscanln(os.Stdin, &resp)
				if !strings.EqualFold(strings.TrimSpace(resp), "y") && !strings.EqualFold(strings.TrimSpace(resp), "yes") {
					fmt.Fprintln(cmd.ErrOrStderr(), "Aborted.")
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
			if cfg.InstallationID != "" {
				sent := "yes"
				switch {
				case config.DoNotTrack():
					sent = "no (DO_NOT_TRACK is set)"
				case !cfg.SendsInstallationID():
					sent = "no (send_installation_id = false)"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "installation_id:  %s (sent to Arc servers: %s)\n", cfg.InstallationID, sent)
			}
			return nil
		},
	}
}
