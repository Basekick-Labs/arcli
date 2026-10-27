// audit queries Arc Enterprise audit records over /api/v1/audit/*.
package commands

import (
	"encoding/csv"
	"fmt"
	"io"
	"sort"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/basekick-labs/arcli/internal/client"
	"github.com/basekick-labs/arcli/internal/output"
)

func newAuditCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "audit",
		Short: "Query Enterprise audit records",
		Long: `Query Arc's durable audit records (/api/v1/audit/*).

These endpoints require an administrator token and an Enterprise license
with audit logging enabled. This is separate from "arcli logs", which reads
the server's in-memory application log buffer.`,
	}
	c.AddCommand(newAuditLogsCmd(), newAuditStatsCmd())
	return c
}

func newAuditLogsCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
		noHeader     bool
		filter       client.AuditLogsOptions
	)
	c := &cobra.Command{
		Use:   "logs",
		Short: "Query audit records with filters and pagination",
		Long: `Query durable audit records, newest first.

Filters are applied on Arc. --since and --until use RFC3339 timestamps;
--limit defaults to the server's 100-entry limit and --offset supports
pagination. Output formats: table (default) | json | csv.`,
		Example: `  arcli audit logs --since 2026-01-01T00:00:00Z --limit 100 --offset 100
  arcli audit logs --event-type auth.login --actor admin -o json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validListFormat(outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json, csv)", outputFormat)
			}
			if cmd.Flags().Changed("limit") && (filter.Limit < 1 || filter.Limit > client.AuditMaxLimit) {
				return fmt.Errorf("--limit must be between 1 and %d (got %d)", client.AuditMaxLimit, filter.Limit)
			}
			if filter.Offset < 0 {
				return fmt.Errorf("--offset must be >= 0 (got %d)", filter.Offset)
			}
			cli, _, err := f.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := f.ctx(cmd)
			defer cancel()
			res, err := cli.AuditLogs(ctx, filter)
			if err != nil {
				return err
			}
			return renderAuditLogs(cmd.OutOrStdout(), res, outputFormat, noHeader)
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json|csv")
	c.Flags().BoolVar(&noHeader, "no-header", false, "suppress column header row (table + csv)")
	c.Flags().StringVar(&filter.EventType, "event-type", "", "filter by event type")
	c.Flags().StringVar(&filter.Actor, "actor", "", "filter by actor")
	c.Flags().StringVar(&filter.Database, "database", "", "filter by database")
	c.Flags().StringVar(&filter.Since, "since", "", "include records from this RFC3339 timestamp")
	c.Flags().StringVar(&filter.Until, "until", "", "include records through this RFC3339 timestamp")
	c.Flags().IntVar(&filter.Limit, "limit", 0, "maximum entries (1-10000; server default: 100)")
	c.Flags().IntVar(&filter.Offset, "offset", 0, "number of matching entries to skip")
	return c
}

func renderAuditLogs(w io.Writer, res *client.AuditLogsResult, format string, noHeader bool) error {
	if format == output.FormatJSON {
		return writeRawJSON(w, res.Raw)
	}
	if format == output.FormatCSV {
		cw := csv.NewWriter(w)
		if !noHeader {
			if err := cw.Write([]string{"id", "timestamp", "event_type", "actor", "method", "path", "database", "measurement", "status_code", "ip_address", "user_agent", "duration_ms", "detail"}); err != nil {
				return err
			}
		}
		for _, entry := range res.Data {
			if err := cw.Write(auditEntryFields(entry)); err != nil {
				return err
			}
		}
		cw.Flush()
		return cw.Error()
	}
	if len(res.Data) == 0 {
		_, err := fmt.Fprintln(w, "(no audit entries)")
		return err
	}
	rows := make([][]string, 0, len(res.Data))
	for _, entry := range res.Data {
		rows = append(rows, []string{
			strconv.FormatInt(entry.ID, 10), fmtTimeVal(entry.Timestamp, "-"), clean(entry.EventType), clean(entry.Actor),
			clean(entry.Method), clean(entry.Path), clean(entry.Database), strconv.Itoa(entry.StatusCode), strconv.FormatInt(entry.DurationMs, 10),
		})
	}
	headers := []string{"ID", "TIME", "EVENT", "ACTOR", "METHOD", "PATH", "DATABASE", "STATUS", "DURATION MS"}
	if noHeader {
		headers = nil
	}
	return output.Table(w, headers, rows)
}

func auditEntryFields(entry client.AuditEntry) []string {
	return []string{
		strconv.FormatInt(entry.ID, 10), fmtTimeVal(entry.Timestamp, ""), entry.EventType, entry.Actor, entry.Method, entry.Path,
		entry.Database, entry.Measurement, strconv.Itoa(entry.StatusCode), entry.IPAddress, entry.UserAgent,
		strconv.FormatInt(entry.DurationMs, 10), entry.Detail,
	}
}

func newAuditStatsCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
		since        string
	)
	c := &cobra.Command{
		Use:   "stats",
		Short: "Show audit event counts",
		Long: `Show audit event counts (GET /api/v1/audit/stats).

--since uses an RFC3339 timestamp. When omitted, Arc reports the last 24 hours.`,
		Args: cobra.NoArgs,
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
			res, err := cli.AuditStats(ctx, since)
			if err != nil {
				return err
			}
			return renderAuditStats(cmd.OutOrStdout(), res, outputFormat)
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	c.Flags().StringVar(&since, "since", "", "count records since this RFC3339 timestamp (default: 24h)")
	return c
}

func renderAuditStats(w io.Writer, res *client.AuditStatsResult, format string) error {
	if format == output.FormatJSON {
		return writeRawJSON(w, res.Raw)
	}
	if _, err := fmt.Fprintf(w, "since: %s\n", clean(res.Since)); err != nil {
		return err
	}
	if len(res.Data) == 0 {
		_, err := fmt.Fprintln(w, "(no audit events)")
		return err
	}
	events := make([]string, 0, len(res.Data))
	for event := range res.Data {
		events = append(events, event)
	}
	sort.Strings(events)
	rows := make([][]string, 0, len(events))
	for _, event := range events {
		rows = append(rows, []string{clean(event), strconv.FormatInt(res.Data[event], 10)})
	}
	return output.Table(w, []string{"EVENT TYPE", "COUNT"}, rows)
}
