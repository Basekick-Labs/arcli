// auth subcommand: token administration against Arc's /api/v1/auth/*.
//
// Everything under `auth token` needs an admin token server-side
// (RequireAdmin middleware); `auth whoami` works with any token. The
// server's permission enum is read / write / delete / admin.
//
// Secret handling: `token create` and `token rotate` are the ONLY two
// places a plaintext token reaches stdout, and they print nothing else
// there in table mode so `T=$(arcli auth token create --name ci)` is
// scriptable. Every other command handles TokenInfo, which never
// carries the secret.
package commands

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/basekick-labs/arcli/internal/client"
	"github.com/basekick-labs/arcli/internal/config"
	"github.com/basekick-labs/arcli/internal/output"
)

// connFlags bundles the connection-selection flags every networked
// command carries, so the auth subcommands don't repeat six locals each.
type connFlags struct {
	connectionName string
	endpoint       string
	token          string
	insecure       bool
	timeout        time.Duration
}

func (f *connFlags) add(c *cobra.Command) {
	addCommonConnectionFlags(c, &f.connectionName, &f.endpoint, &f.token, &f.insecure)
	addTimeoutFlag(c, &f.timeout)
}

// ctx returns a fresh per-request deadline. Commands that make several
// calls use one per call so a slow first request can't eat the budget
// of the next and misattribute the failure.
func (f *connFlags) ctx(cmd *cobra.Command) (context.Context, context.CancelFunc) {
	return context.WithTimeout(cmd.Context(), f.timeout)
}

func (f *connFlags) client(cmd *cobra.Command) (*client.Client, string, error) {
	if f.timeout <= 0 {
		return nil, "", fmt.Errorf("--timeout must be > 0 (got %s)", f.timeout)
	}
	return buildClient(cmd.ErrOrStderr(), f.connectionName, f.endpoint, f.token, f.insecure, f.timeout)
}

func newAuthCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "auth",
		Short: "Inspect the current token and administer API tokens",
		Long: `Inspect the current token and administer API tokens.

"whoami" works with any token. Everything under "token" requires an
admin token; the server refuses with "Permission denied: admin required"
otherwise. Permissions are: read, write, delete, admin.`,
	}
	c.AddCommand(newAuthWhoamiCmd(), newAuthTokenCmd())
	return c
}

func newAuthTokenCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "token",
		Short: "Manage API tokens (admin)",
		Long: `Manage API tokens (admin). Maps onto Arc's /api/v1/auth/tokens routes.

Every subcommand that takes a token accepts either its numeric id or its
exact name. Ids are tried first, so a token literally named "42" must be
addressed by its id.`,
	}
	c.AddCommand(
		newAuthTokenListCmd(),
		newAuthTokenShowCmd(),
		newAuthTokenPermissionsCmd(),
		newAuthTokenCreateCmd(),
		newAuthTokenUpdateCmd(),
		newAuthTokenRotateCmd(),
		newAuthTokenRevokeCmd(),
		newAuthTokenDeleteCmd(),
	)
	return c
}

// ---- whoami ----------------------------------------------------------------

func newAuthWhoamiCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
	)
	c := &cobra.Command{
		Use:   "whoami",
		Short: "Show the token the current connection authenticates with",
		Long: `Show the token the current connection authenticates with
(GET /api/v1/auth/verify). Works with any valid token.

Table output also names the connection that was resolved (a profile
name, or "(flags)" / "(env)" for ad-hoc use).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validObjectFormat(outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", outputFormat)
			}
			cli, connName, err := f.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := f.ctx(cmd)
			defer cancel()
			me, err := cli.Verify(ctx)
			if err != nil {
				return err
			}
			if outputFormat == output.FormatJSON {
				return writeJSON(cmd.OutOrStdout(), struct {
					Connection string           `json:"connection"`
					Endpoint   string           `json:"endpoint"`
					Token      client.TokenInfo `json:"token"`
				}{connName, cli.Endpoint(), *me})
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "connection:  %s\n", connName)
			fmt.Fprintf(w, "endpoint:    %s\n", cli.Endpoint())
			writeTokenInfo(w, me)
			return nil
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	return c
}

// ---- list ------------------------------------------------------------------

func newAuthTokenListCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
		noHeader     bool
	)
	c := &cobra.Command{
		Use:   "list",
		Short: "List all API tokens (admin)",
		Long: `List all API tokens (GET /api/v1/auth/tokens). Revoked tokens are
included with ENABLED=false; the server keeps them until deleted.

Output formats: table (default) | json | csv`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validListFormat(outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json, csv)", outputFormat)
			}
			cli, _, err := f.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := f.ctx(cmd)
			defer cancel()
			tokens, err := cli.ListTokens(ctx)
			if err != nil {
				return err
			}
			return renderTokenList(cmd, tokens, outputFormat, noHeader)
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json|csv")
	c.Flags().BoolVar(&noHeader, "no-header", false, "suppress column header row (table + csv)")
	return c
}

// ---- show ------------------------------------------------------------------

func newAuthTokenShowCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
	)
	c := &cobra.Command{
		Use:   "show <id|name>",
		Short: "Show one API token (admin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validObjectFormat(outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", outputFormat)
			}
			cli, _, err := f.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := f.ctx(cmd)
			defer cancel()
			t, err := resolveTokenRef(ctx, cli, args[0])
			if err != nil {
				return err
			}
			if outputFormat == output.FormatJSON {
				return writeJSON(cmd.OutOrStdout(), t)
			}
			writeTokenInfo(cmd.OutOrStdout(), t)
			return nil
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	return c
}

// ---- permissions -----------------------------------------------------------

func newAuthTokenPermissionsCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
	)
	c := &cobra.Command{
		Use:   "permissions <id|name>",
		Short: "Show a token's effective permissions (admin)",
		Long: `Show a token's effective permissions
(GET /api/v1/auth/tokens/:id/permissions).

On an OSS server every token has one row: database "*" with the token's
own permission list, source "token". With Arc Enterprise RBAC enabled,
team roles add per-database / per-measurement rows with source "rbac".`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validObjectFormat(outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", outputFormat)
			}
			cli, _, err := f.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := f.ctx(cmd)
			defer cancel()
			t, err := resolveTokenRef(ctx, cli, args[0])
			if err != nil {
				return err
			}
			ctx, cancel = f.ctx(cmd)
			defer cancel()
			perms, err := cli.GetTokenPermissions(ctx, t.ID)
			if err != nil {
				return err
			}
			if outputFormat == output.FormatJSON {
				return writeJSON(cmd.OutOrStdout(), struct {
					ID          int64                        `json:"id"`
					Name        string                       `json:"name"`
					RBACEnabled bool                         `json:"rbac_enabled"`
					Permissions []client.EffectivePermission `json:"permissions"`
				}{t.ID, t.Name, perms.RBACEnabled, perms.Permissions})
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "token:        %s (id %d)\n", clean(t.Name), t.ID)
			fmt.Fprintf(w, "rbac_enabled: %t\n", perms.RBACEnabled)
			if len(perms.Permissions) == 0 {
				_, err := fmt.Fprintln(w, "(no permissions)")
				return err
			}
			rows := make([][]string, 0, len(perms.Permissions))
			for _, p := range perms.Permissions {
				m := p.Measurement
				if m == "" {
					m = "*"
				}
				rows = append(rows, []string{p.Database, m, strings.Join(p.Permissions, ","), p.Source})
			}
			return output.Table(w, []string{"DATABASE", "MEASUREMENT", "PERMISSIONS", "SOURCE"}, rows)
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	return c
}

// ---- create ----------------------------------------------------------------

func newAuthTokenCreateCmd() *cobra.Command {
	var (
		f             connFlags
		outputFormat  string
		name          string
		description   string
		permissions   []string
		noPermissions bool
		expiresIn     string
	)
	c := &cobra.Command{
		Use:   "create",
		Short: "Create an API token and print its secret once (admin)",
		Long: `Create an API token (POST /api/v1/auth/tokens).

The secret is printed to stdout exactly once and cannot be retrieved
again; the token's id and a reminder go to stderr, so
  T=$(arcli auth token create --name ci --permission read)
captures only the secret.

--permission may be repeated or comma-joined. Omit it for the server
default (read,write). --no-permissions creates a token with no OSS
permissions at all (only useful with Arc Enterprise RBAC roles).
--expires-in takes a Go duration (24h, 90m) or a day count (7d) and is
relative to now; expiry cannot be removed later.`,
		Example: `  arcli auth token create --name grafana --permission read
  arcli auth token create --name ingest --permission read,write --expires-in 30d
  arcli auth token create --name ops --permission admin --description "on-call" -o json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validObjectFormat(outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", outputFormat)
			}
			if cmd.Flags().Changed("permission") && noPermissions {
				return fmt.Errorf("--permission and --no-permissions are mutually exclusive")
			}
			opts := client.CreateTokenOptions{Name: name, Description: description, ExpiresIn: expiresIn}
			if noPermissions {
				opts.Permissions = &[]string{}
			} else if cmd.Flags().Changed("permission") {
				perms := normalisePermissions(permissions)
				if len(perms) == 0 {
					return fmt.Errorf("--permission given but no permission names (use --no-permissions for an RBAC-only token)")
				}
				opts.Permissions = &perms
			}
			// Validate before touching the network (mirrors the client's
			// own checks so the error text is identical either way).
			if opts.Permissions != nil {
				for _, p := range *opts.Permissions {
					if !client.IsValidPermission(p) {
						return fmt.Errorf("invalid permission %q (valid: read, write, delete, admin)", p)
					}
				}
			}
			if expiresIn != "" {
				if _, err := client.ParseExpiresIn(expiresIn); err != nil {
					return err
				}
			}
			cli, _, err := f.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := f.ctx(cmd)
			defer cancel()
			secret, err := cli.CreateToken(ctx, opts)
			if err != nil {
				return err
			}
			ctx, cancel = f.ctx(cmd)
			defer cancel()
			// The create response carries no id. Look it up by name so
			// the operator can address the token afterwards. Best effort:
			// the secret is already minted, so a lookup failure must not
			// turn into a non-zero exit that hides it.
			var id int64 = -1
			if created, lerr := findTokenByName(ctx, cli, name); lerr == nil {
				id = created.ID
			} else {
				fmt.Fprintf(cmd.ErrOrStderr(), "note: token created but id lookup failed: %v\n", lerr)
			}
			if outputFormat == output.FormatJSON {
				return writeJSON(cmd.OutOrStdout(), struct {
					Token string `json:"token"`
					ID    int64  `json:"id"`
					Name  string `json:"name"`
				}{secret, id, name})
			}
			fmt.Fprintln(cmd.OutOrStdout(), secret)
			if id >= 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "Created token %q (id %d). Store the secret securely; it cannot be retrieved again.\n", name, id)
			} else {
				fmt.Fprintf(cmd.ErrOrStderr(), "Created token %q. Store the secret securely; it cannot be retrieved again.\n", name)
			}
			return nil
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	c.Flags().StringVar(&name, "name", "", "token name (required, unique)")
	c.Flags().StringVar(&description, "description", "", "free-text description")
	c.Flags().StringSliceVar(&permissions, "permission", nil, "permission to grant (read|write|delete|admin); repeat or comma-join")
	c.Flags().BoolVar(&noPermissions, "no-permissions", false, "grant no OSS permissions (RBAC-only token)")
	c.Flags().StringVar(&expiresIn, "expires-in", "", "relative expiry: Go duration (24h, 90m) or days (7d)")
	_ = c.MarkFlagRequired("name")
	return c
}

// ---- update ----------------------------------------------------------------

func newAuthTokenUpdateCmd() *cobra.Command {
	var (
		f             connFlags
		name          string
		description   string
		permissions   []string
		noPermissions bool
		expiresIn     string
	)
	c := &cobra.Command{
		Use:   "update <id|name>",
		Short: "Update a token's name, description, permissions, or expiry (admin)",
		Long: `Update a token (PATCH /api/v1/auth/tokens/:id). Only the flags you pass
change; --description "" clears the description. --permission replaces
the whole permission list; --no-permissions clears it.

Server limitation: once a token has an expiry it cannot be made
non-expiring again, only moved with a new --expires-in.`,
		Example: `  arcli auth token update grafana --description "dashboards"
  arcli auth token update 7 --permission read,write --expires-in 90d`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			fl := cmd.Flags()
			if fl.Changed("permission") && noPermissions {
				return fmt.Errorf("--permission and --no-permissions are mutually exclusive")
			}
			var opts client.UpdateTokenOptions
			if fl.Changed("name") {
				if name == "" {
					return fmt.Errorf("--name must not be empty")
				}
				opts.Name = &name
			}
			if fl.Changed("description") {
				opts.Description = &description
			}
			if noPermissions {
				opts.Permissions = &[]string{}
			} else if fl.Changed("permission") {
				perms := normalisePermissions(permissions)
				if len(perms) == 0 {
					return fmt.Errorf("--permission given but no permission names (use --no-permissions to clear)")
				}
				for _, p := range perms {
					if !client.IsValidPermission(p) {
						return fmt.Errorf("invalid permission %q (valid: read, write, delete, admin)", p)
					}
				}
				opts.Permissions = &perms
			}
			if fl.Changed("expires-in") {
				if _, err := client.ParseExpiresIn(expiresIn); err != nil {
					return err
				}
				opts.ExpiresIn = &expiresIn
			}
			if opts.IsEmpty() {
				return fmt.Errorf("nothing to update (pass at least one of --name, --description, --permission, --no-permissions, --expires-in)")
			}
			cli, _, err := f.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := f.ctx(cmd)
			defer cancel()
			t, err := resolveTokenRef(ctx, cli, args[0])
			if err != nil {
				return err
			}
			ctx, cancel = f.ctx(cmd)
			defer cancel()
			if err := cli.UpdateToken(ctx, t.ID, opts); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Updated token %q (id %d)\n", t.Name, t.ID)
			return nil
		},
	}
	f.add(c)
	c.Flags().StringVar(&name, "name", "", "new token name")
	c.Flags().StringVar(&description, "description", "", "new description (\"\" clears)")
	c.Flags().StringSliceVar(&permissions, "permission", nil, "replacement permission list; repeat or comma-join")
	c.Flags().BoolVar(&noPermissions, "no-permissions", false, "clear all OSS permissions (RBAC-only token)")
	c.Flags().StringVar(&expiresIn, "expires-in", "", "new relative expiry: Go duration (24h) or days (7d)")
	return c
}

// ---- rotate ----------------------------------------------------------------

func newAuthTokenRotateCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
		yes          bool
		save         bool
	)
	c := &cobra.Command{
		Use:   "rotate <id|name>",
		Short: "Replace a token's secret and print the new one once (admin)",
		Long: `Replace a token's secret (POST /api/v1/auth/tokens/:id/rotate). The old
secret stops working immediately; id, name, permissions, and expiry are
kept. The new secret is printed to stdout exactly once.

Rotating the token arcli itself is using cuts off every connection
profile that holds it. Pass --save to write the new secret into every
profile in the config file that held the old one. --save is refused
up front (before anything is rotated) unless the connection came from a
named profile, the server confirms (via /verify) that the target is the
token in use, and the config directory is writable. The names of the
updated profiles are reported on stderr; -o json carries only the
secret, id, and name.

Revoked and expired tokens are refused: a rotated secret for them could
never authenticate.`,
		Example: `  arcli auth token rotate ingest --yes
  arcli auth token rotate admin --save`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validObjectFormat(outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", outputFormat)
			}
			if f.timeout <= 0 {
				return fmt.Errorf("--timeout must be > 0 (got %s)", f.timeout)
			}
			stderr := cmd.ErrOrStderr()

			// Own Load/Resolve so cfg stays in hand for --save.
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			conn, connName, err := cfg.Resolve(config.ResolveOptions{
				ConnectionName: f.connectionName, Endpoint: f.endpoint, Token: f.token,
			})
			if err != nil {
				return err
			}
			if save {
				profile, ok := cfg.Connections[connName]
				if !ok {
					return fmt.Errorf("--save requires a named connection profile; this connection came from %s", connName)
				}
				if profile.Token != conn.Token {
					return fmt.Errorf("--save refused: profile %q's stored token is not the token in use", connName)
				}
			}
			cli, err := buildClientFrom(stderr, conn, cfg.OutboundInstallationID(), f.insecure, f.timeout)
			if err != nil {
				return err
			}
			ctx, cancel := f.ctx(cmd)
			defer cancel()
			target, err := resolveTokenRef(ctx, cli, args[0])
			if err != nil {
				return err
			}
			if !target.Enabled {
				return fmt.Errorf("token %q (id %d) is revoked; a rotated secret could never authenticate. Delete it instead", target.Name, target.ID)
			}
			if target.IsExpired(time.Now()) {
				return fmt.Errorf("token %q (id %d) expired at %s; run `arcli auth token update %d --expires-in ...` first", target.Name, target.ID, fmtTime(target.ExpiresAt, "-"), target.ID)
			}

			ctx, cancel = f.ctx(cmd)
			defer cancel()
			me, verr := cli.Verify(ctx)
			isSelf := verr == nil && me.ID == target.ID
			if verr != nil && !save {
				fmt.Fprintf(stderr, "note: could not confirm whether this is the token in use (%v)\n", verr)
			}
			if save {
				if verr != nil {
					return fmt.Errorf("--save refused: cannot confirm which token is in use: %w", verr)
				}
				if !isSelf {
					return fmt.Errorf("--save only applies when rotating the token in use (id %d); target is id %d", me.ID, target.ID)
				}
			}

			q := fmt.Sprintf("Rotate token %q (id %d, %s)? The current secret stops working immediately.", target.Name, target.ID, permsOrNone(target.Permissions))
			if isSelf {
				fmt.Fprintf(stderr, "WARNING: token %q (id %d) is the token this connection is using; arcli will lose access unless the new secret is saved.\n", target.Name, target.ID)
			}
			if err := confirmOrAbort(cmd, q, yes); err != nil {
				return err
			}
			if save {
				// After the prompt (so "N" leaves no directory behind),
				// before the rotate (so an unwritable dir never strands
				// a freshly minted secret).
				if err := preflightConfigWritable(); err != nil {
					return fmt.Errorf("--save refused before rotating: %w", err)
				}
			}

			ctx, cancel = f.ctx(cmd)
			defer cancel()
			secret, err := cli.RotateToken(ctx, target.ID)
			if err != nil {
				return err
			}
			// Print the secret BEFORE touching the config file, in both
			// output modes: if Save fails or the process dies inside it,
			// the old secret is already dead server-side and this write
			// is the only copy of the new one. The list of updated
			// profiles therefore goes to stderr, not into the JSON.
			if outputFormat == output.FormatJSON {
				if err := writeJSON(cmd.OutOrStdout(), struct {
					Token string `json:"token"`
					ID    int64  `json:"id"`
					Name  string `json:"name"`
				}{secret, target.ID, target.Name}); err != nil {
					return err
				}
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), secret)
			}

			var saved []string
			var saveErr error
			if save {
				// Re-load: the prompt may have sat open while another
				// shell edited the file, and writing the pre-prompt copy
				// back would silently revert those edits.
				old := conn.Token
				fresh, lerr := config.Load()
				if lerr != nil {
					saveErr = lerr
				} else {
					for n, p := range fresh.Connections {
						if p.Token == old {
							p.Token = secret
							fresh.Connections[n] = p
							saved = append(saved, n+" ("+p.Endpoint+")")
						}
					}
					sort.Strings(saved)
					if len(saved) == 0 {
						saveErr = fmt.Errorf("no profile in %s holds the old token any more", configPathForDisplay())
					} else {
						saveErr = fresh.Save()
					}
				}
			}
			if saveErr != nil {
				fmt.Fprintf(stderr, "ERROR: config NOT updated (%v). The new secret is printed above; the old secret is now invalid.\n", saveErr)
				return fmt.Errorf("rotated token %q but failed to save config", target.Name)
			}
			switch {
			case save:
				fmt.Fprintf(stderr, "Rotated token %q (id %d). Updated connection(s): %s\n", target.Name, target.ID, strings.Join(saved, ", "))
			case isSelf:
				if _, named := cfg.Connections[connName]; named {
					fmt.Fprintf(stderr, "Rotated token %q (id %d). Profile %q now holds an invalid token; run `arcli config update %s --token <new>` or re-run with --save next time.\n", target.Name, target.ID, connName, connName)
				} else {
					fmt.Fprintf(stderr, "Rotated token %q (id %d). The token you passed via %s is now invalid; use the new secret from now on.\n", target.Name, target.ID, connName)
				}
			default:
				fmt.Fprintf(stderr, "Rotated token %q (id %d). Store the new secret securely; it cannot be retrieved again.\n", target.Name, target.ID)
			}
			return nil
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	c.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	c.Flags().BoolVar(&save, "save", false, "write the new secret into every config profile that held the old one (own token only)")
	return c
}

// ---- revoke / delete -------------------------------------------------------

func newAuthTokenRevokeCmd() *cobra.Command {
	var (
		f     connFlags
		yes   bool
		force bool
	)
	c := &cobra.Command{
		Use:   "revoke <id|name>",
		Short: "Disable a token permanently (admin)",
		Long: `Disable a token (POST /api/v1/auth/tokens/:id/revoke). The token stops
authenticating immediately but stays listed with ENABLED=false. Arc has
no API to re-enable a revoked token; if you need it back, create a new
one. Use "delete" to remove the row entirely.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTokenDestructive(cmd, &f, args[0], yes, force, "Revoke", func(ctx context.Context, cli *client.Client, id int64) error {
				return cli.RevokeToken(ctx, id)
			})
		},
	}
	f.add(c)
	c.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	c.Flags().BoolVar(&force, "force", false, "allow revoking the last enabled admin token")
	return c
}

func newAuthTokenDeleteCmd() *cobra.Command {
	var (
		f     connFlags
		yes   bool
		force bool
	)
	c := &cobra.Command{
		Use:   "delete <id|name>",
		Short: "Delete a token (admin)",
		Long: `Delete a token row (DELETE /api/v1/auth/tokens/:id). The server does
not stop you deleting the token you are using or the last admin token;
arcli warns when the target is the token in use.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTokenDestructive(cmd, &f, args[0], yes, force, "Delete", func(ctx context.Context, cli *client.Client, id int64) error {
				return cli.DeleteToken(ctx, id)
			})
		},
	}
	f.add(c)
	c.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	c.Flags().BoolVar(&force, "force", false, "allow deleting the last enabled admin token")
	return c
}

// runTokenDestructive is the shared show → last-admin guard →
// self-check → confirm → act path for revoke and delete.
func runTokenDestructive(cmd *cobra.Command, f *connFlags, ref string, yes, force bool, verb string, act func(context.Context, *client.Client, int64) error) error {
	cli, _, err := f.client(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := f.ctx(cmd)
	defer cancel()
	target, err := resolveTokenRef(ctx, cli, ref)
	if err != nil {
		return err
	}
	stderr := cmd.ErrOrStderr()
	// The server happily removes the last admin token, which leaves the
	// token-admin API unreachable. Refuse unless --force.
	if target.Enabled && hasPermission(target.Permissions, "admin") && !target.IsExpired(time.Now()) {
		ctx, cancel = f.ctx(cmd)
		defer cancel()
		all, lerr := cli.ListTokens(ctx)
		if lerr != nil {
			return fmt.Errorf("cannot check whether another admin token exists: %w", lerr)
		}
		others := 0
		for _, t := range all {
			if t.ID != target.ID && t.Enabled && hasPermission(t.Permissions, "admin") && !t.IsExpired(time.Now()) {
				others++
			}
		}
		if others == 0 && !force {
			return fmt.Errorf("token %q (id %d) is the last enabled admin token; %s it anyway with --force", target.Name, target.ID, strings.ToLower(verb))
		}
	}
	ctx, cancel = f.ctx(cmd)
	defer cancel()
	if me, verr := cli.Verify(ctx); verr != nil {
		fmt.Fprintf(stderr, "note: could not confirm whether this is the token in use (%v)\n", verr)
	} else if me.ID == target.ID {
		fmt.Fprintf(stderr, "WARNING: token %q (id %d) is the token this connection is using; arcli will lose access.\n", target.Name, target.ID)
	}
	state := "enabled"
	if !target.Enabled {
		state = "revoked"
	}
	q := fmt.Sprintf("%s token %q (id %d, %s, %s)?", verb, target.Name, target.ID, permsOrNone(target.Permissions), state)
	if err := confirmOrAbort(cmd, q, yes); err != nil {
		return err
	}
	ctx, cancel = f.ctx(cmd)
	defer cancel()
	if err := act(ctx, cli, target.ID); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%sd token %q (id %d)\n", verb, target.Name, target.ID)
	return nil
}

// ---- helpers ---------------------------------------------------------------

// resolveTokenRef turns "<id|name>" into a TokenInfo. Numeric first
// (one GET), else an exact-name match over the list.
func resolveTokenRef(ctx context.Context, cli *client.Client, ref string) (*client.TokenInfo, error) {
	if ref == "" {
		return nil, fmt.Errorf("token id or name is required")
	}
	if id, err := strconv.ParseInt(ref, 10, 64); err == nil {
		if id < 0 {
			return nil, fmt.Errorf("token id must be a non-negative integer (got %s)", ref)
		}
		return cli.GetToken(ctx, id)
	}
	return findTokenByName(ctx, cli, ref)
}

func findTokenByName(ctx context.Context, cli *client.Client, name string) (*client.TokenInfo, error) {
	tokens, err := cli.ListTokens(ctx)
	if err != nil {
		return nil, err
	}
	for i := range tokens {
		if tokens[i].Name == name {
			return &tokens[i], nil
		}
	}
	return nil, fmt.Errorf("no token named %q (run `arcli auth token list`)", name)
}

// normalisePermissions trims, lower-cases, de-duplicates, and drops
// empties so `--permission " Read,write "` behaves.
func normalisePermissions(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, p := range in {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// clean strips control characters from server-supplied text before it
// reaches a terminal (table mode and prompts). A token name is chosen by
// whoever holds an admin token; it must not be able to emit terminal
// escape sequences into another operator's session. JSON and CSV paths
// go through encoders and need no help.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if isTerminalUnsafe(r) || r == '\n' || r == '\t' {
			return -1
		}
		return r
	}, s)
}

// isTerminalUnsafe reports C0/C1 control characters (except newline and
// tab, which callers decide about) and Unicode bidi overrides, all of
// which can rewrite or reorder what a terminal shows.
func isTerminalUnsafe(r rune) bool {
	switch {
	case r < 0x20 && r != '\n' && r != '\t', r == 0x7f, r >= 0x80 && r <= 0x9f:
		return true
	case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
		return true
	}
	return false
}

func permsOrNone(p []string) string {
	if len(p) == 0 {
		return "no permissions"
	}
	return strings.Join(p, ",")
}

// fmtTime renders a nullable timestamp as UTC RFC3339, or `none` when nil.
func fmtTime(t *time.Time, none string) string {
	if t == nil || t.IsZero() {
		return none
	}
	return t.UTC().Format(time.RFC3339)
}

// validObjectFormat is table|json — the formats for single-object output.
func validObjectFormat(s string) bool {
	return s == output.FormatTable || s == output.FormatJSON
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// writeTokenInfo prints one TokenInfo as aligned key/value lines.
func writeTokenInfo(w io.Writer, t *client.TokenInfo) {
	fmt.Fprintf(w, "id:          %d\n", t.ID)
	fmt.Fprintf(w, "name:        %s\n", clean(t.Name))
	if t.Description != "" {
		fmt.Fprintf(w, "description: %s\n", clean(t.Description))
	}
	fmt.Fprintf(w, "permissions: %s\n", permsOrNone(t.Permissions))
	fmt.Fprintf(w, "enabled:     %t\n", t.Enabled)
	fmt.Fprintf(w, "expires:     %s\n", fmtTime(t.ExpiresAt, "never"))
	fmt.Fprintf(w, "last used:   %s\n", fmtTime(t.LastUsedAt, "never"))
	fmt.Fprintf(w, "created:     %s\n", t.CreatedAt.UTC().Format(time.RFC3339))
}

// renderTokenList writes the token list sorted by id in the chosen format.
func renderTokenList(cmd *cobra.Command, tokens []client.TokenInfo, format string, noHeader bool) error {
	list := make([]client.TokenInfo, len(tokens))
	copy(list, tokens)
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	for i := range list {
		if list[i].Permissions == nil {
			list[i].Permissions = []string{}
		}
	}
	switch format {
	case output.FormatJSON:
		return writeJSON(cmd.OutOrStdout(), struct {
			Tokens []client.TokenInfo `json:"tokens"`
			Count  int                `json:"count"`
		}{list, len(list)})
	case output.FormatCSV:
		w := csv.NewWriter(cmd.OutOrStdout())
		if !noHeader {
			if err := w.Write([]string{"id", "name", "permissions", "enabled", "expires_at", "last_used_at", "created_at", "description"}); err != nil {
				return err
			}
		}
		for _, t := range list {
			if err := w.Write([]string{
				strconv.FormatInt(t.ID, 10), t.Name, strings.Join(t.Permissions, ","), strconv.FormatBool(t.Enabled),
				fmtTime(t.ExpiresAt, ""), fmtTime(t.LastUsedAt, ""), t.CreatedAt.UTC().Format(time.RFC3339), t.Description,
			}); err != nil {
				return err
			}
		}
		w.Flush()
		return w.Error()
	default:
		if len(list) == 0 {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), "(no tokens)")
			return err
		}
		rows := make([][]string, 0, len(list))
		for _, t := range list {
			rows = append(rows, []string{
				strconv.FormatInt(t.ID, 10), clean(t.Name), permsOrNone(t.Permissions), strconv.FormatBool(t.Enabled),
				fmtTime(t.ExpiresAt, "-"), fmtTime(t.LastUsedAt, "never"), t.CreatedAt.UTC().Format(time.RFC3339),
			})
		}
		headers := []string{"ID", "NAME", "PERMISSIONS", "ENABLED", "EXPIRES", "LAST USED", "CREATED"}
		if noHeader {
			headers = nil
		}
		return output.Table(cmd.OutOrStdout(), headers, rows)
	}
}

// errAborted is returned when the operator answers anything but yes.
var errAborted = errors.New("aborted")

// confirmOrAbort is the single confirmation gate for every destructive
// command. It returns an error (non-zero exit) on "no", and refuses
// outright when stdin is a pipe, a file or /dev/null and --yes was not
// given, so a script never gets exit 0 without the action having
// happened. The prompt goes to stderr so `$(...)` capture stays clean.
func confirmOrAbort(cmd *cobra.Command, question string, yes bool) error {
	if yes {
		return nil
	}
	if err := requireInteractiveStdin(cmd); err != nil {
		return err
	}
	in := cmd.InOrStdin()
	fmt.Fprintf(cmd.ErrOrStderr(), "%s [y/N] ", question)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		return errAborted
	}
	resp := strings.TrimSpace(line)
	if strings.EqualFold(resp, "y") || strings.EqualFold(resp, "yes") {
		return nil
	}
	return errAborted
}

// requireInteractiveStdin is the non-TTY guard confirmOrAbort applies,
// exposed so commands that do expensive work before their prompt (the
// retention execute dry-run preflight) can refuse up front instead.
//
// /dev/null is a character device, so the pipe heuristic alone would
// let a headless invocation (systemd StandardInput=null, nohup,
// `< /dev/null`) reach the prompt; it is refused explicitly.
func requireInteractiveStdin(cmd *cobra.Command) error {
	f, ok := cmd.InOrStdin().(*os.File)
	if !ok {
		return nil
	}
	if isPipe(f) || isDevNull(f) {
		return fmt.Errorf("confirmation required but stdin is not a terminal; pass --yes")
	}
	return nil
}

// isDevNull reports whether f is /dev/null. os.Stdin.Name() is always
// "/dev/stdin" regardless of redirection, so compare the underlying
// file identity instead of the name.
func isDevNull(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	nfi, err := os.Stat(os.DevNull)
	if err != nil {
		return false
	}
	return os.SameFile(fi, nfi)
}

// cleanMultiline strips control characters like clean but keeps
// newlines and tabs, for server-supplied text that is meant to span
// lines (SQL).
func cleanMultiline(s string) string {
	return strings.Map(func(r rune) rune {
		if isTerminalUnsafe(r) {
			return -1
		}
		return r
	}, s)
}

// preflightConfigWritable proves the config directory accepts a new
// file before an irreversible server call whose result must be saved
// there. Uses the same temp-file shape as config.Save.
func configPathForDisplay() string {
	p, err := config.ConfigPath()
	if err != nil {
		return "the config file"
	}
	return p
}

func preflightConfigWritable() error {
	path, err := config.ConfigPath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("config dir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "config.*.toml")
	if err != nil {
		return fmt.Errorf("config dir %s is not writable: %w", dir, err)
	}
	name := tmp.Name()
	_ = tmp.Close()
	return os.Remove(name)
}
