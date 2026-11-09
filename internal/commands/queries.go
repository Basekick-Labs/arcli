// admin query exposes Arc Enterprise query-management endpoints.
package commands

import (
	"encoding/csv"
	"fmt"
	"io"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/basekick-labs/arcli/internal/client"
	"github.com/basekick-labs/arcli/internal/output"
)

func newAdminCmd() *cobra.Command {
	c := &cobra.Command{Use: "admin", Short: "Administer Arc Enterprise features"}
	c.AddCommand(newQueriesCmd())
	return c
}

func newQueriesCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "query",
		Short: "Inspect and manage running and completed queries (Arc Enterprise)",
		Long: `Inspect and manage Arc query activity. These endpoints require an admin
token and an Arc Enterprise licence with the query_management feature enabled.

Table and CSV timestamps are UTC RFC3339 by default; --time-output
rfc3339nano shows fractional seconds. JSON preserves the original response.
CSV retains multiline SQL unchanged.

Cancelling a query prompts for confirmation unless --yes is supplied.`,
	}
	c.AddCommand(newQueriesActiveCmd(), newQueriesHistoryCmd(), newQueriesShowCmd(), newQueriesCancelCmd())
	return c
}

func newQueriesActiveCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
		noHeader     bool
		to           timeOutput
	)
	c := &cobra.Command{
		Use:   "active",
		Short: "List active queries",
		Long: `List active queries (GET /api/v1/queries/active).

Output includes the query id, status, token, client address, start time,
duration, and SQL. Supports table, JSON, and CSV output.`,
		Example: `  arcli admin query active
  arcli admin query active -o json
  arcli admin query active -o csv`,
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
			res, err := cli.ActiveQueries(ctx)
			if err != nil {
				return err
			}
			return renderManagedQueryList(cmd, res, outputFormat, noHeader, false, to)
		},
	}
	f.add(c)
	to.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json|csv")
	c.Flags().BoolVar(&noHeader, "no-header", false, "suppress column header row (table + csv)")
	return c
}

func newQueriesHistoryCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
		noHeader     bool
		limit        int
		to           timeOutput
	)
	c := &cobra.Command{
		Use:   "history",
		Short: "List recently completed queries",
		Long: `List recently completed queries (GET /api/v1/queries/history).

Arc returns the most recent 50 queries by default. Use --limit to request
between 1 and 1000 entries. Supports table, JSON, and CSV output.`,
		Example: `  arcli admin query history
  arcli admin query history --limit 200 -o csv`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validListFormat(outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json, csv)", outputFormat)
			}
			if cmd.Flags().Changed("limit") && (limit < 1 || limit > client.QueryHistoryMaxLimit) {
				return fmt.Errorf("--limit must be between 1 and %d (got %d)", client.QueryHistoryMaxLimit, limit)
			}
			cli, _, err := f.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := f.ctx(cmd)
			defer cancel()
			requestLimit := limit
			if !cmd.Flags().Changed("limit") {
				requestLimit = 0
			}
			res, err := cli.QueryHistory(ctx, requestLimit)
			if err != nil {
				return err
			}
			return renderManagedQueryList(cmd, res, outputFormat, noHeader, true, to)
		},
	}
	f.add(c)
	to.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json|csv")
	c.Flags().BoolVar(&noHeader, "no-header", false, "suppress column header row (table + csv)")
	c.Flags().IntVar(&limit, "limit", client.QueryHistoryDefaultLimit, "maximum queries to return (1-1000)")
	return c
}

func newQueriesShowCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
		to           timeOutput
	)
	c := &cobra.Command{
		Use:     "show <query-id>",
		Short:   "Show query details",
		Long:    "Show full metadata for a query (GET /api/v1/queries/:id). Supports table and JSON output.",
		Example: "  arcli admin query show 8f14e45fceea -o json",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validObjectFormat(outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", outputFormat)
			}
			if err := client.ValidateQueryID(args[0]); err != nil {
				return err
			}
			cli, _, err := f.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := f.ctx(cmd)
			defer cancel()
			res, err := cli.GetManagedQuery(ctx, args[0])
			if err != nil {
				return err
			}
			if outputFormat == output.FormatJSON {
				return writeRawJSON(cmd.OutOrStdout(), res.Raw)
			}
			return renderManagedQuery(cmd.OutOrStdout(), res.Query, to)
		},
	}
	f.add(c)
	to.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	return c
}

func newQueriesCancelCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
		yes          bool
	)
	c := &cobra.Command{
		Use:     "cancel <query-id>",
		Short:   "Cancel an active query",
		Long:    "Cancel an active query (DELETE /api/v1/queries/:id). Prompts before sending the request unless --yes is supplied.",
		Example: "  arcli admin query cancel 8f14e45fceea --yes",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validObjectFormat(outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", outputFormat)
			}
			if err := client.ValidateQueryID(args[0]); err != nil {
				return err
			}
			cli, _, err := f.client(cmd)
			if err != nil {
				return err
			}
			if err := confirmOrAbort(cmd, fmt.Sprintf("Cancel query %q?", args[0]), yes); err != nil {
				return err
			}
			ctx, cancel := f.ctx(cmd)
			defer cancel()
			res, err := cli.CancelManagedQuery(ctx, args[0])
			if err != nil {
				return err
			}
			if outputFormat == output.FormatJSON {
				return writeRawJSON(cmd.OutOrStdout(), res.Raw)
			}
			message := res.Message
			if message == "" {
				message = "Query cancelled"
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s (%s)\n", clean(message), args[0])
			return err
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	c.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	return c
}

func renderManagedQueryList(cmd *cobra.Command, list *client.QueryList, format string, noHeader, history bool, to timeOutput) error {
	if format == output.FormatJSON {
		return writeRawJSON(cmd.OutOrStdout(), list.Raw)
	}
	if format == output.FormatCSV {
		w := csv.NewWriter(cmd.OutOrStdout())
		headers := managedQueryCSVHeaders(history)
		if !noHeader {
			if err := w.Write(headers); err != nil {
				return err
			}
		}
		for _, query := range list.Queries {
			if err := w.Write(managedQueryListRow(query, history, false, to)); err != nil {
				return err
			}
		}
		w.Flush()
		return w.Error()
	}
	if len(list.Queries) == 0 {
		label := "(no active queries)"
		if history {
			label = "(no query history)"
		}
		_, err := fmt.Fprintln(cmd.OutOrStdout(), label)
		return err
	}
	headers := managedQueryListHeaders(history)
	if noHeader {
		headers = nil
	}
	rows := make([][]string, 0, len(list.Queries))
	for _, query := range list.Queries {
		rows = append(rows, managedQueryListRow(query, history, true, to))
	}
	return output.Table(cmd.OutOrStdout(), headers, rows)
}

func managedQueryListHeaders(history bool) []string {
	if history {
		return []string{"ID", "STATUS", "TOKEN", "CLIENT", "STARTED", "ENDED", "DURATION MS", "ROWS", "SQL"}
	}
	return []string{"ID", "STATUS", "TOKEN", "CLIENT", "STARTED", "DURATION MS", "SQL"}
}

// managedQueryCSVHeaders names the CSV columns in lower snake_case after
// the server's JSON fields, like the other CSV outputs. "token" has no
// single JSON field: it combines token_name and token_id.
func managedQueryCSVHeaders(history bool) []string {
	if history {
		return []string{"id", "status", "token", "remote_addr", "start_time", "end_time", "duration_ms", "row_count", "sql"}
	}
	return []string{"id", "status", "token", "remote_addr", "start_time", "duration_ms", "sql"}
}

// managedQueryListRow builds one table (terminal) or CSV row. An unset
// timestamp is "-" in the table and an empty field in CSV.
func managedQueryListRow(query client.ManagedQuery, history, terminal bool, to timeOutput) []string {
	none := ""
	if terminal {
		none = "-"
	}
	row := []string{
		query.ID, query.Status, managedQueryToken(query), query.RemoteAddr,
		to.Val(query.StartTime, none),
	}
	if history {
		row = append(row, to.Ptr(query.EndTime, none), formatQueryDuration(query.DurationMs), strconv.Itoa(query.RowCount), query.SQL)
		return managedQueryCleanRow(row, terminal)
	}
	return managedQueryCleanRow(append(row, formatQueryDuration(query.DurationMs), query.SQL), terminal)
}

func managedQueryToken(query client.ManagedQuery) string {
	if query.TokenName == "" {
		if query.TokenID == 0 {
			return "-"
		}
		return strconv.FormatInt(query.TokenID, 10)
	}
	if query.TokenID == 0 {
		return query.TokenName
	}
	return fmt.Sprintf("%s (%d)", query.TokenName, query.TokenID)
}

func managedQueryCleanRow(row []string, terminal bool) []string {
	if terminal {
		for i := range row {
			row[i] = clean(row[i])
		}
	}
	return row
}

func formatQueryDuration(ms float64) string {
	return strconv.FormatFloat(ms, 'f', -1, 64)
}

func renderManagedQuery(w io.Writer, query client.ManagedQuery, to timeOutput) error {
	rows := [][]string{
		{"id", clean(query.ID)},
		{"status", clean(query.Status)},
		{"sql", clean(query.SQL)},
		{"token", clean(managedQueryToken(query))},
		{"client", cleanOrDash(query.RemoteAddr)},
		{"start time", to.Val(query.StartTime, "-")},
		{"end time", to.Ptr(query.EndTime, "-")},
		{"duration ms", formatQueryDuration(query.DurationMs)},
		{"rows", strconv.Itoa(query.RowCount)},
		{"row cap", strconv.Itoa(query.RowCap)},
		{"error", cleanOrDash(query.Error)},
		{"parallel", strconv.FormatBool(query.IsParallel)},
		{"partitions", strconv.Itoa(query.PartitionCount)},
	}
	return output.Table(w, []string{"FIELD", "VALUE"}, rows)
}

func cleanOrDash(s string) string {
	if s == "" {
		return "-"
	}
	return clean(s)
}
