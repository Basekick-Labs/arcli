// Write subcommand: POSTs line protocol to /api/v1/write/line-protocol.
//
// Input: either `-f file.lp` (file path) or stdin (when -f is not
// supplied). Body is streamed — we never buffer the full payload — so
// piping `cat huge.lp | arcli write` works at line-rate.
package commands

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/basekick-labs/arcli/internal/client"
)

func newWriteCmd() *cobra.Command {
	var (
		connectionName string
		endpoint       string
		token          string
		database       string
		insecure       bool
		filePath       string
		precision      string
		timeout        time.Duration
	)
	c := &cobra.Command{
		Use:   "write",
		Short: "Write line-protocol records to an Arc cluster",
		Long: `Write line-protocol records to an Arc cluster (POST /api/v1/write/line-protocol).

Body source: --file (-f) takes precedence; otherwise reads from stdin.
The body is streamed — large files / pipes do NOT buffer in memory.

Precision must be one of ns, us, ms, s. The server treats an unset
precision as nanoseconds.`,
		Example: `  echo "cpu,host=a value=42 1234567890000000000" | arcli write --database metrics
  arcli write -f payload.lp --database metrics --precision ms
  cat /var/log/lp/*.lp | arcli write -c prod --database metrics`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !client.ValidPrecision(precision) {
				return fmt.Errorf("invalid --precision %q (must be one of ns, us, ms, s)", precision)
			}
			if timeout <= 0 {
				return fmt.Errorf("--timeout must be > 0 (got %s)", timeout)
			}

			body, closer, err := openWriteBody(cmd, filePath)
			if err != nil {
				return err
			}
			if closer != nil {
				defer closer.Close()
			}

			cli, _, err := buildClient(cmd.ErrOrStderr(), connectionName, endpoint, token, insecure, timeout)
			if err != nil {
				return err
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()

			if err := cli.WriteLineProtocol(ctx, body, database, client.Precision(precision)); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "OK")
			return nil
		},
	}
	c.Flags().StringVarP(&connectionName, "connection", "c", "", "named connection (overrides active)")
	c.Flags().StringVar(&endpoint, "endpoint", "", "ad-hoc Arc endpoint URL")
	c.Flags().StringVar(&token, "token", "", "ad-hoc bearer token")
	c.Flags().StringVar(&database, "database", "", "target database (defaults to connection's default_database)")
	c.Flags().BoolVar(&insecure, "insecure", false, "skip TLS certificate verification (logs a warning to stderr)")
	c.Flags().StringVarP(&filePath, "file", "f", "", "read line protocol from a file instead of stdin")
	c.Flags().StringVar(&precision, "precision", "", "timestamp precision: ns|us|ms|s (default: server-side default = ns)")
	c.Flags().DurationVar(&timeout, "timeout", 60*time.Second, "per-request HTTP timeout")
	return c
}

// openWriteBody returns the io.Reader to stream into the POST body.
// When `path` is non-empty it opens the file (caller closes via the
// returned io.Closer). Otherwise it returns stdin, which the caller
// MUST NOT close (no closer is returned for that path).
//
// We deliberately do NOT block on a TTY stdin like `query` does: an
// operator who runs `arcli write` interactively and types lines is a
// supported (if rare) workflow. The hang-on-empty-TTY foot-gun
// matters for query because empty-SQL would error anyway; for write
// the server accepts an empty body as a no-op.
func openWriteBody(cmd *cobra.Command, path string) (io.Reader, io.Closer, error) {
	if path != "" {
		f, err := os.Open(path)
		if err != nil {
			return nil, nil, fmt.Errorf("open %s: %w", path, err)
		}
		return f, f, nil
	}
	return cmd.InOrStdin(), nil, nil
}
