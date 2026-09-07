// write: push records to an Arc cluster.
//
// Three input formats, one endpoint each:
//   - lp (default): line protocol streamed to /api/v1/write/line-protocol;
//     never buffers, so `cat huge.lp | arcli write` works at line rate.
//   - msgpack: a prepared MessagePack document (Arc's columnar / row /
//     batch shapes, optionally gzip- or zstd-compressed) streamed as-is to
//     /api/v1/write/msgpack.
//   - json: the same document written as JSON, validated and converted
//     to MessagePack client-side, then sent to /api/v1/write/msgpack.
package commands

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/basekick-labs/arcli/internal/client"
)

const (
	writeFormatLP      = "lp"
	writeFormatMsgPack = "msgpack"
	writeFormatJSON    = "json"
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
		format         string
		timeout        time.Duration
	)
	c := &cobra.Command{
		Use:   "write",
		Short: "Write records to an Arc cluster (line protocol, MessagePack, or JSON)",
		Long: `Write records to an Arc cluster.

--format lp (default) streams line protocol to /api/v1/write/line-protocol.
The body is never buffered, so huge files and pipes work at line rate.
--precision is ns|us|ms|s (server default ns).

--format msgpack streams a prepared MessagePack document to
/api/v1/write/msgpack as-is: Arc's columnar {"m","columns"} shape, its
row {"m","t","h","fields","tags"} shape, or {"batch":[...]}; gzip or
zstd compressed bodies are passed straight through (the server sniffs
the magic bytes). A file is sent with its exact length; stdin is
streamed chunked. The server accepts at most 1 GiB per request and
infers the timestamp unit from each value's magnitude (a columnar
"time" column from its first value), so --precision is not accepted. Arc silently drops batch items that fail
to decode, so prefer --format json when the document is hand-written.

--format json reads the same document as JSON (at most 64 MiB; the
conversion holds several copies in memory), validates it the way the
server does plus the cases the server only fails on later or drops
silently, and converts it to MessagePack.
Integer literals become int64, others float64 (a column mixing both
is promoted to float64); "time" values must be integer epochs of one
unit; tags may be strings, numbers, or booleans.

Which msgpack shape to use: the row shape keeps tags as tag columns
(compaction can de-duplicate; "h" defaults to "unknown" when absent),
the columnar shape is fastest but carries no tag metadata. For tagged
data written from text, plain line protocol remains the simplest path.`,
		Example: `  echo "cpu,host=a value=42 1234567890000000000" | arcli write --database metrics
  arcli write -f payload.lp --database metrics --precision ms
  arcli write --format json -f cpu.json --database metrics
  arcli write --format msgpack -f cpu.msgpack.zst --database metrics`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if timeout <= 0 {
				return fmt.Errorf("--timeout must be > 0 (got %s)", timeout)
			}
			switch format {
			case writeFormatLP:
				if !client.ValidPrecision(precision) {
					return fmt.Errorf("invalid --precision %q (must be one of ns, us, ms, s)", precision)
				}
			case writeFormatMsgPack, writeFormatJSON:
				if cmd.Flags().Changed("precision") {
					return fmt.Errorf("--precision applies to line protocol only; %s timestamps carry their own unit", format)
				}
			default:
				return fmt.Errorf("invalid --format %q (valid: lp, msgpack, json)", format)
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
			// --timeout bounds the HTTP request; the JSON branch converts
			// the document first, so the deadline starts after that.
			var doc []byte
			var sum client.MsgPackDocSummary
			if format == writeFormatJSON {
				if doc, sum, err = client.JSONToMsgPack(body); err != nil {
					return err
				}
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()

			switch format {
			case writeFormatLP:
				if err := cli.WriteLineProtocol(ctx, body, database, client.Precision(precision)); err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), "OK")
				return nil
			case writeFormatJSON:
				if err := cli.WriteMsgPack(ctx, bytes.NewReader(doc), int64(len(doc)), database); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "OK: %d measurement(s), %d row(s), %d bytes as MessagePack\n", sum.Items, sum.Rows, len(doc))
				return nil
			default: // msgpack pass-through
				size := int64(-1)
				if f, ok := body.(*os.File); ok && filePath != "" {
					fi, err := f.Stat()
					if err != nil {
						return fmt.Errorf("stat %s: %w", filePath, err)
					}
					size = fi.Size()
					if size > client.MsgPackMaxBody {
						return fmt.Errorf("%s is %d bytes; the server accepts at most 1 GiB per request", filePath, size)
					}
				}
				br := bufio.NewReader(body)
				first, err := br.Peek(1)
				if err != nil {
					if errors.Is(err, io.EOF) {
						return fmt.Errorf("empty body; nothing to write")
					}
					return fmt.Errorf("read body: %w", err)
				}
				if !client.MsgPackFirstByteOK(first[0]) {
					return fmt.Errorf("not a MessagePack document (first byte 0x%02x %q); use --format lp for line protocol or --format json for a JSON document", first[0], string(first[0]))
				}
				counter := &countingReader{r: br, limit: client.MsgPackMaxBody}
				if err := cli.WriteMsgPack(ctx, counter, size, database); err != nil {
					if counter.exceeded {
						return fmt.Errorf("body exceeded 1 GiB; the request was aborted before the server could ingest it")
					}
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "OK: %d bytes as MessagePack\n", counter.n)
				return nil
			}
		},
	}
	c.Flags().StringVarP(&connectionName, "connection", "c", "", "named connection (overrides active)")
	c.Flags().StringVar(&endpoint, "endpoint", "", "ad-hoc Arc endpoint URL")
	c.Flags().StringVar(&token, "token", "", "ad-hoc bearer token")
	c.Flags().StringVar(&database, "database", "", "target database (defaults to connection's default_database)")
	c.Flags().BoolVar(&insecure, "insecure", false, "skip TLS certificate verification (logs a warning to stderr)")
	c.Flags().StringVarP(&filePath, "file", "f", "", "read the body from a file instead of stdin")
	c.Flags().StringVar(&format, "format", writeFormatLP, "input format: lp|msgpack|json")
	c.Flags().StringVar(&precision, "precision", "", "line-protocol timestamp precision: ns|us|ms|s (default: server-side default = ns)")
	c.Flags().DurationVar(&timeout, "timeout", 60*time.Second, "per-request HTTP timeout")
	return c
}

// countingReader counts bytes and fails the read once limit is
// exceeded, so a chunked stdin stream can be aborted client-side.
type countingReader struct {
	r        io.Reader
	n        int64
	limit    int64
	exceeded bool
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.n > c.limit {
		c.exceeded = true
		return n, fmt.Errorf("body exceeds %d bytes", c.limit)
	}
	return n, err
}

// openWriteBody returns the io.Reader to stream into the POST body.
// When `path` is non-empty it opens the file (caller closes via the
// returned io.Closer). Otherwise it returns stdin, which the caller
// MUST NOT close (no closer is returned for that path).
//
// We deliberately do NOT block on a TTY stdin like `query` does: an
// operator who runs `arcli write` interactively and types lines is a
// supported (if rare) workflow. The hang-on-empty-TTY foot-gun
// matters for query because empty-SQL would error anyway; for write an
// empty body is a no-op (lp) or a clear client error (msgpack, json).
func openWriteBody(cmd *cobra.Command, path string) (io.Reader, io.Closer, error) {
	if path == "" {
		return cmd.InOrStdin(), nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", path, err)
	}
	return f, f, nil
}
