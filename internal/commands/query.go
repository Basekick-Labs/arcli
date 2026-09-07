// Query subcommand: runs SQL against an Arc cluster.
//
// Three input modes:
//  1. `arcli query "SELECT ..."` (positional arg)
//  2. `arcli query -f file.sql` (file containing the SQL)
//  3. `arcli query` reading SQL from stdin (when neither arg nor -f
//     is given and stdin is a pipe)
//
// Four output formats: table (default), json, csv, arrow. Arrow streams
// raw IPC bytes to stdout; the other three flow through internal/output.
package commands

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/basekick-labs/arcli/internal/client"
	"github.com/basekick-labs/arcli/internal/config"
	"github.com/basekick-labs/arcli/internal/output"
)

func newQueryCmd() *cobra.Command {
	var (
		connectionName string
		endpoint       string
		token          string
		database       string
		insecure       bool
		outputFormat   string
		sqlFile        string
		noHeader       bool
		limit          int
		timeout        time.Duration
	)
	c := &cobra.Command{
		Use:   "query [SQL]",
		Short: "Run a SQL query against an Arc cluster",
		Long: `Run a SQL query against an Arc cluster.

SQL input precedence: positional argument > --file > stdin (only when
neither is supplied). Output defaults to a pretty table; -o json|csv
emit machine-parseable formats; -o arrow streams binary Arrow IPC to
stdout for piping into pyarrow / duckdb / etc.`,
		Example: `  arcli query "SELECT count(*) FROM cpu"
  arcli query --database metrics "SELECT * FROM cpu LIMIT 10"
  arcli query -f long_query.sql -o csv > out.csv
  echo "SELECT 1" | arcli query
  arcli query "SELECT * FROM cpu" -o arrow | python -c 'import pyarrow.ipc as ipc, sys; print(ipc.open_stream(sys.stdin.buffer).read_all())'`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !output.ValidFormat(outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json, csv, arrow)", outputFormat)
			}
			if timeout <= 0 {
				return fmt.Errorf("--timeout must be > 0 (got %s)", timeout)
			}

			sql, err := readSQL(cmd, args, sqlFile)
			if err != nil {
				return err
			}
			if strings.TrimSpace(sql) == "" {
				return fmt.Errorf("empty SQL (pass a positional arg, -f, or pipe via stdin)")
			}

			cli, _, err := buildClient(cmd.ErrOrStderr(), connectionName, endpoint, token, insecure, timeout)
			if err != nil {
				return err
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()

			if outputFormat == output.FormatArrow {
				return runArrowQuery(ctx, cli, sql, database, cmd.OutOrStdout(), cmd.ErrOrStderr())
			}

			qr, err := cli.QueryJSON(ctx, sql, database)
			if err != nil {
				return err
			}
			return output.RenderQueryResult(cmd.OutOrStdout(), qr, outputFormat, noHeader, limit)
		},
	}
	c.Flags().StringVarP(&connectionName, "connection", "c", "", "named connection (overrides active)")
	c.Flags().StringVar(&endpoint, "endpoint", "", "ad-hoc Arc endpoint URL")
	c.Flags().StringVar(&token, "token", "", "ad-hoc bearer token")
	c.Flags().StringVar(&database, "database", "", "target database (defaults to connection's default_database)")
	c.Flags().BoolVar(&insecure, "insecure", false, "skip TLS certificate verification (logs a warning to stderr)")
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json|csv|arrow")
	c.Flags().StringVarP(&sqlFile, "file", "f", "", "read SQL from a file instead of the positional arg")
	c.Flags().BoolVar(&noHeader, "no-header", false, "suppress column header row (table + csv)")
	c.Flags().IntVar(&limit, "limit", 0, "cap output rows client-side (0 = no cap; server result is already bounded by the SQL)")
	c.Flags().DurationVar(&timeout, "timeout", 60*time.Second, "per-request HTTP timeout")
	return c
}

// readSQL resolves the SQL string from the three input modes. Order:
//  1. Positional arg
//  2. -f file
//  3. Stdin (only if stdin is not a TTY, to avoid hanging on a missing
//     arg)
func readSQL(cmd *cobra.Command, args []string, sqlFile string) (string, error) {
	if len(args) > 0 {
		return args[0], nil
	}
	if sqlFile != "" {
		b, err := os.ReadFile(sqlFile)
		if err != nil {
			return "", fmt.Errorf("read SQL file: %w", err)
		}
		return string(b), nil
	}
	stdin := cmd.InOrStdin()
	if isPipe(stdin) {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return "", fmt.Errorf("read SQL from stdin: %w", err)
		}
		return string(b), nil
	}
	return "", fmt.Errorf("no SQL provided (pass a positional arg, -f, or pipe via stdin)")
}

// isPipe reports whether r is a non-TTY pipe/file (i.e. data is
// actually available). Avoids hanging on an interactive terminal when
// the user forgot to pass SQL.
//
// Returns true for any non-*os.File reader because that's what cobra's
// cmd.InOrStdin() returns in tests (typically *bytes.Buffer); tests
// always want "yes, read whatever's there." For real *os.File stdin
// we check ModeCharDevice == 0 to distinguish "pipe/file redirect"
// from "interactive TTY."
func isPipe(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return true
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) == 0
}

// runArrowQuery streams the Arrow IPC body straight from the server to
// stdout. Server-side execution time is read from the response trailer
// (available only after Body has been drained to EOF) and printed to
// stderr after the stream finishes.
//
// On a mid-stream error from io.Copy (network drop, client kill, server
// reset) we have already written N bytes of a partial IPC stream to
// stdout — downstream consumers like pyarrow will surface that as a
// hard parse error. Print an explanatory stderr line in addition to
// returning the error so the operator sees both the corrupt binary on
// stdout AND the explanation on stderr.
func runArrowQuery(ctx context.Context, cli *client.Client, sql, database string, stdout, stderr io.Writer) error {
	resp, err := cli.QueryArrow(ctx, sql, database)
	if err != nil {
		return err
	}
	defer resp.Close()

	n, err := io.Copy(stdout, resp.Body)
	if err != nil {
		fmt.Fprintf(stderr, "arrow: stream interrupted after %d bytes — stdout contains a truncated Arrow IPC payload that will not parse cleanly\n", n)
		return fmt.Errorf("stream arrow body: %w", err)
	}
	if execMs, ok := resp.ExecutionTimeMs(); ok {
		fmt.Fprintf(stderr, "arrow: %d bytes, server execution %dms\n", n, execMs)
	} else {
		fmt.Fprintf(stderr, "arrow: %d bytes\n", n)
	}
	return nil
}

// buildClient resolves the connection (per CLAUDE.md precedence) and
// constructs an HTTP client. The connection's stored InsecureTLS is
// honored unless the user passes --insecure on the command line
// (flag wins, OR semantics).
//
// Shared by `query` and `write` so the resolution rules stay in one
// place. Returns the resolved connection name purely for `-v` /
// error messages.
//
// The TLS-disabled warning goes to the passed-in stderr writer (not
// os.Stderr directly) so tests using cmd.SetErr(buf) can capture it.
func buildClient(stderr io.Writer, connectionName, endpoint, token string, insecureFlag bool, timeout time.Duration) (*client.Client, string, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, "", err
	}
	conn, name, err := cfg.Resolve(config.ResolveOptions{
		ConnectionName: connectionName,
		Endpoint:       endpoint,
		Token:          token,
	})
	if err != nil {
		return nil, "", err
	}
	insecure := conn.InsecureTLS || insecureFlag
	if insecure && strings.HasPrefix(strings.ToLower(conn.Endpoint), "https://") {
		// Only warn when TLS verify would actually have been applied.
		// On http:// endpoints the skip-verify flag is a no-op and the
		// warning would mislead.
		if insecureFlag {
			fmt.Fprintln(stderr, "WARNING: TLS certificate verification disabled (--insecure)")
		} else {
			fmt.Fprintln(stderr, "WARNING: TLS certificate verification disabled (connection has insecure_tls=true)")
		}
	}
	cli, err := client.New(client.Config{
		Endpoint:    conn.Endpoint,
		Token:       conn.Token,
		Database:    conn.DefaultDatabase,
		InsecureTLS: insecure,
		Timeout:     timeout,
	})
	return cli, name, err
}
