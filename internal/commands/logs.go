// logs: read the server's recent application log entries over
// GET /api/v1/logs (admin when authentication is on).
package commands

import (
	"encoding/csv"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/basekick-labs/arcli/internal/client"
	"github.com/basekick-labs/arcli/internal/output"
)

func newLogsCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
		noHeader     bool
		limit        int
		level        string
		since        time.Duration
	)
	c := &cobra.Command{
		Use:   "logs",
		Short: "Show recent server log entries (admin)",
		Long: `Show recent server log entries (GET /api/v1/logs), newest first.

Arc keeps the last 10 000 entries in memory, per process, and returns
at most 1000 per call; behind a load balancer each call may reach a
different node. --level is a minimum: "warn" returns warn, error and
fatal entries. --since is a window ending now (1m to 24h, rounded up
to whole minutes). Messages are as the server captured them; a message
containing a double quote may be cut short by the server's log parser.

Output formats: table (default) | json | csv`,
		Example: `  arcli logs
  arcli logs --level error --since 6h --limit 200
  arcli logs -o json | jq '.logs[] | select(.component == "compaction")'`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validListFormat(outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json, csv)", outputFormat)
			}
			if cmd.Flags().Changed("limit") && (limit < 1 || limit > client.LogsMaxLimit) {
				return fmt.Errorf("--limit must be between 1 and %d (got %d)", client.LogsMaxLimit, limit)
			}
			if level != "" && !containsString(client.LogLevels, strings.ToLower(level)) {
				return fmt.Errorf("invalid --level %q (valid: %s)", level, strings.Join(client.LogLevels, ", "))
			}
			if cmd.Flags().Changed("since") && (since < time.Minute || since > client.LogsMaxSince) {
				return fmt.Errorf("--since must be between 1m and 24h (got %s)", since)
			}
			cli, _, err := f.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := f.ctx(cmd)
			defer cancel()
			opts := client.LogsOptions{Level: level}
			if cmd.Flags().Changed("limit") {
				opts.Limit = limit
			}
			if cmd.Flags().Changed("since") {
				opts.Since = since
			}
			res, err := cli.Logs(ctx, opts)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			switch outputFormat {
			case output.FormatJSON:
				return writeRawJSON(w, res.Raw)
			case output.FormatCSV:
				cw := csv.NewWriter(w)
				if !noHeader {
					if err := cw.Write([]string{"timestamp", "level", "component", "message", "caller"}); err != nil {
						return err
					}
				}
				for _, e := range res.Logs {
					if err := cw.Write([]string{fmtTimeVal(e.Timestamp, ""), e.Level, e.Component, e.Message, e.Caller}); err != nil {
						return err
					}
				}
				cw.Flush()
				return cw.Error()
			default:
				if len(res.Logs) == 0 {
					_, err := fmt.Fprintf(w, "(no log entries in the last %d minute(s)%s)\n", res.SinceMinutes, levelSuffix(res.LevelFilter))
					return err
				}
				rows := make([][]string, 0, len(res.Logs))
				for _, e := range res.Logs {
					rows = append(rows, []string{fmtTimeVal(e.Timestamp, "-"), clean(e.Level), clean(e.Component), clean(e.Message)})
				}
				headers := []string{"TIME", "LEVEL", "COMPONENT", "MESSAGE"}
				if noHeader {
					headers = nil
				}
				return output.Table(w, headers, rows)
			}
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json|csv")
	c.Flags().BoolVar(&noHeader, "no-header", false, "suppress column header row (table + csv)")
	c.Flags().IntVar(&limit, "limit", 100, "maximum entries (1-1000)")
	c.Flags().StringVar(&level, "level", "", "minimum level: debug|info|warn|error|fatal")
	c.Flags().DurationVar(&since, "since", time.Hour, "window ending now (1m-24h)")
	return c
}

func levelSuffix(level string) string {
	if level == "" {
		return ""
	}
	return " at level " + clean(level) + " or above"
}
