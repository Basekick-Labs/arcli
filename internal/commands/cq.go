// cq subcommand: manage continuous queries over
// /api/v1/continuous_queries/*. Every route in that group needs an
// admin token. As with retention, the server's PUT is a full replace,
// so `update` reads, merges, re-validates, and writes the whole object.
package commands

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/basekick-labs/arcli/internal/client"
	"github.com/basekick-labs/arcli/internal/output"
)

// maxQueryFileBytes bounds --query-file; the server caps the query at
// 10 000 characters after placeholder substitution.
const maxQueryFileBytes = 64 << 10

func newCQCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "cq",
		Short: "Manage continuous queries (scheduled aggregations)",
		Long: `Manage continuous queries (/api/v1/continuous_queries/*). Every cq
subcommand requires an admin token.

A continuous query runs an aggregation SQL over a time window and writes
the result into a destination measurement. The query must contain the
{start_time} and {end_time} placeholders and read the source as
FROM <database>.<source_measurement>; the server rewrites only that form
to the underlying files.

On Arc OSS nothing runs continuous queries automatically (the CQ
scheduler is an Enterprise feature); run them with "arcli cq execute".`,
	}
	c.AddCommand(newCQListCmd(), newCQShowCmd(), newCQCreateCmd(), newCQUpdateCmd(), newCQDeleteCmd(), newCQExecuteCmd(), newCQExecutionsCmd())
	return c
}

func resolveCQRef(ctx context.Context, cli *client.Client, ref string) (*client.ContinuousQuery, error) {
	if ref == "" {
		return nil, fmt.Errorf("continuous query id or name is required")
	}
	if id, err := strconv.ParseInt(ref, 10, 64); err == nil {
		if id < 0 {
			return nil, fmt.Errorf("continuous query id must be a non-negative integer (got %s)", ref)
		}
		q, _, err := cli.GetContinuousQuery(ctx, id)
		return q, err
	}
	list, _, err := cli.ListContinuousQueries(ctx, "", nil)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].Name == ref {
			return &list[i], nil
		}
	}
	return nil, fmt.Errorf("no continuous query named %q (run `arcli cq list`)", ref)
}

// ---- list / show -----------------------------------------------------------

func newCQListCmd() *cobra.Command {
	var (
		f                connFlags
		outputFormat     string
		noHeader         bool
		database         string
		active, inactive bool
	)
	c := &cobra.Command{
		Use:   "list",
		Short: "List continuous queries",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validListFormat(outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json, csv)", outputFormat)
			}
			if active && inactive {
				return fmt.Errorf("--active and --inactive are mutually exclusive")
			}
			var filter *bool
			if active || inactive {
				v := active
				filter = &v
			}
			cli, _, err := f.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := f.ctx(cmd)
			defer cancel()
			list, raw, err := cli.ListContinuousQueries(ctx, database, filter)
			if err != nil {
				return err
			}
			if outputFormat == output.FormatJSON {
				return writeRawJSON(cmd.OutOrStdout(), raw)
			}
			return renderCQList(cmd.OutOrStdout(), list, noHeader, outputFormat == output.FormatCSV)
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json|csv")
	c.Flags().BoolVar(&noHeader, "no-header", false, "suppress column header row (table + csv)")
	c.Flags().StringVar(&database, "database", "", "only queries in this database")
	c.Flags().BoolVar(&active, "active", false, "only active queries")
	c.Flags().BoolVar(&inactive, "inactive", false, "only inactive queries")
	return c
}

func newCQShowCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
	)
	c := &cobra.Command{
		Use:   "show <id|name>",
		Short: "Show one continuous query, including its SQL",
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
			q, err := resolveCQRef(ctx, cli, args[0])
			if err != nil {
				return err
			}
			if outputFormat == output.FormatJSON {
				return writeJSON(cmd.OutOrStdout(), q)
			}
			writeCQInfo(cmd.OutOrStdout(), q)
			return nil
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	return c
}

// ---- create / update -------------------------------------------------------

type cqFlags struct {
	name, database, source, destination string
	query, queryFile, interval          string
	tagColumns                          []string
	description                         string
	inactive, active, clearTags         bool
}

func (cf *cqFlags) add(c *cobra.Command, forUpdate bool) {
	c.Flags().StringVar(&cf.name, "name", "", "query name (unique)")
	c.Flags().StringVar(&cf.database, "database", "", "database holding the source measurement")
	c.Flags().StringVar(&cf.source, "source", "", "source measurement")
	c.Flags().StringVar(&cf.destination, "destination", "", "destination measurement (letter first; letters, digits, _ -)")
	c.Flags().StringVar(&cf.query, "query", "", "aggregation SQL with {start_time} and {end_time} placeholders")
	c.Flags().StringVar(&cf.queryFile, "query-file", "", "read the SQL from a file instead of --query")
	c.Flags().StringVar(&cf.interval, "interval", "", "run interval as a Go duration (30s, 5m, 1h); minimum 10s")
	c.Flags().StringSliceVar(&cf.tagColumns, "tag-column", nil, "tag column(s) in the result; repeat or comma-join")
	c.Flags().StringVar(&cf.description, "description", "", "free-text description (\"\" clears on update)")
	c.Flags().BoolVar(&cf.inactive, "inactive", false, "create the query disabled / disable it")
	if forUpdate {
		c.Flags().BoolVar(&cf.active, "active", false, "enable the query")
		c.Flags().BoolVar(&cf.clearTags, "clear-tag-columns", false, "remove all tag columns")
	}
}

// readQueryFlag returns the SQL from --query or --query-file.
func (cf *cqFlags) readQuery(fl interface{ Changed(string) bool }) (string, bool, error) {
	hasQ, hasF := fl.Changed("query"), fl.Changed("query-file")
	switch {
	case hasQ && hasF:
		return "", false, fmt.Errorf("--query and --query-file are mutually exclusive")
	case hasQ:
		return cf.query, true, nil
	case hasF:
		fh, err := os.Open(cf.queryFile)
		if err != nil {
			return "", false, fmt.Errorf("open query file: %w", err)
		}
		defer fh.Close()
		b, err := io.ReadAll(io.LimitReader(fh, maxQueryFileBytes+1))
		if err != nil {
			return "", false, fmt.Errorf("read query file: %w", err)
		}
		if len(b) > maxQueryFileBytes {
			return "", false, fmt.Errorf("query file exceeds %d bytes; the server accepts at most %d characters", maxQueryFileBytes, client.CQMaxQueryLen)
		}
		if !utf8.Valid(b) {
			// json.Marshal would silently replace invalid bytes with
			// U+FFFD and store a query that is not what the file says.
			return "", false, fmt.Errorf("query file is not valid UTF-8")
		}
		return strings.TrimSpace(string(b)), true, nil
	}
	return "", false, nil
}

func sourceRefWarning(w io.Writer, req client.ContinuousQueryRequest) {
	if !client.HasSourceReference(req.Query, req.Database, req.SourceMeasurement) {
		fmt.Fprintf(w, "warning: the query does not contain `FROM %s.%s`; the server only rewrites that form to the underlying files, so execution will likely fail\n", req.Database, req.SourceMeasurement)
	}
}

func newCQCreateCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
		cf           cqFlags
	)
	c := &cobra.Command{
		Use:   "create",
		Short: "Create a continuous query (admin)",
		Long: `Create a continuous query (POST /api/v1/continuous_queries/).

The query is active unless --inactive is given. arcli checks the
required fields, the destination and tag-column grammars, the
{start_time}/{end_time} placeholders, the query length, and the
interval (a Go duration of at least 10s; the server stores an invalid
interval silently and then never schedules the query). SQL semantics are
checked by the server.`,
		Example: `  arcli cq create --name cpu-1m --database metrics --source cpu --destination cpu_1m --interval 1m \
      --tag-column host --query "SELECT time_bucket(INTERVAL '1 minute', time) AS time, host, avg(usage) AS usage \
      FROM metrics.cpu WHERE time >= {start_time} AND time < {end_time} GROUP BY 1, 2"
  arcli cq create --name cpu-1m --database metrics --source cpu --destination cpu_1m --interval 1m --query-file cpu_1m.sql`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validObjectFormat(outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", outputFormat)
			}
			query, has, err := cf.readQuery(cmd.Flags())
			if err != nil {
				return err
			}
			if !has {
				return fmt.Errorf("one of --query or --query-file is required")
			}
			req := client.ContinuousQueryRequest{
				Name: cf.name, Database: cf.database, SourceMeasurement: cf.source, DestinationMeasurement: cf.destination,
				Query: query, Interval: cf.interval, TagColumns: normaliseList(cf.tagColumns), IsActive: !cf.inactive,
			}
			if cmd.Flags().Changed("description") {
				d := cf.description
				req.Description = &d
			}
			if err := client.ValidateCQRequest(req); err != nil {
				return err
			}
			sourceRefWarning(cmd.ErrOrStderr(), req)
			cli, _, err := f.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := f.ctx(cmd)
			defer cancel()
			q, raw, err := cli.CreateContinuousQuery(ctx, req)
			if err != nil {
				return err
			}
			if outputFormat == output.FormatJSON {
				return writeRawJSON(cmd.OutOrStdout(), raw)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Created continuous query %q (id %d)\n", clean(q.Name), q.ID)
			writeCQInfo(cmd.OutOrStdout(), q)
			schedulerHint(cmd, &f, cli, "cq", fmt.Sprintf("arcli cq execute %d", q.ID))
			return nil
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	cf.add(c, false)
	for _, req := range []string{"name", "database", "source", "destination", "interval"} {
		_ = c.MarkFlagRequired(req)
	}
	return c
}

func newCQUpdateCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
		cf           cqFlags
	)
	c := &cobra.Command{
		Use:   "update <id|name>",
		Short: "Change fields of a continuous query (admin)",
		Long: `Change one or more fields of a continuous query.

Arc's PUT replaces the whole query, so arcli reads it, applies only the
flags you pass, re-validates the result with the full create rules, and
writes it back. Last writer wins; there is no conflict detection.
--description "" clears the description; --clear-tag-columns removes
all tag columns.`,
		Example: `  arcli cq update cpu-1m --interval 5m
  arcli cq update 4 --query-file cpu_1m.sql --inactive`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validObjectFormat(outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", outputFormat)
			}
			fl := cmd.Flags()
			if cf.active && cf.inactive {
				return fmt.Errorf("--active and --inactive are mutually exclusive")
			}
			if cf.clearTags && fl.Changed("tag-column") {
				return fmt.Errorf("--tag-column and --clear-tag-columns are mutually exclusive")
			}
			query, hasQuery, err := cf.readQuery(fl)
			if err != nil {
				return err
			}
			changed := hasQuery
			for _, n := range []string{"name", "database", "source", "destination", "interval", "tag-column", "description", "active", "inactive", "clear-tag-columns"} {
				if fl.Changed(n) {
					changed = true
				}
			}
			if !changed {
				return fmt.Errorf("nothing to update (pass at least one field flag)")
			}
			cli, _, err := f.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := f.ctx(cmd)
			defer cancel()
			cur, err := resolveCQRef(ctx, cli, args[0])
			if err != nil {
				return err
			}
			req := client.RequestFromCQ(*cur)
			if fl.Changed("name") {
				if cf.name == "" {
					return fmt.Errorf("--name must not be empty")
				}
				if cf.name != cur.Name {
					// The server answers a rename collision with a bare 500,
					// so check first; if the list itself fails, say so.
					list, _, lerr := cli.ListContinuousQueries(ctx, "", nil)
					if lerr != nil {
						return fmt.Errorf("cannot check whether %q is already taken: %w", cf.name, lerr)
					}
					for _, other := range list {
						if other.Name == cf.name && other.ID != cur.ID {
							return fmt.Errorf("a continuous query named %q already exists (id %d)", cf.name, other.ID)
						}
					}
				}
				req.Name = cf.name
			}
			if fl.Changed("database") {
				req.Database = cf.database
			}
			if fl.Changed("source") {
				req.SourceMeasurement = cf.source
			}
			if fl.Changed("destination") {
				req.DestinationMeasurement = cf.destination
			}
			if hasQuery {
				req.Query = query
			}
			if fl.Changed("interval") {
				req.Interval = cf.interval
			}
			if cf.clearTags {
				req.TagColumns = []string{}
			} else if fl.Changed("tag-column") {
				req.TagColumns = normaliseList(cf.tagColumns)
			}
			if fl.Changed("description") {
				if cf.description == "" {
					req.Description = nil
				} else {
					d := cf.description
					req.Description = &d
				}
			}
			if cf.active {
				req.IsActive = true
			}
			if cf.inactive {
				req.IsActive = false
			}
			if err := client.ValidateCQRequest(req); err != nil {
				return fmt.Errorf("merged query is invalid: %w (fix it by passing the relevant flag as well)", err)
			}
			sourceRefWarning(cmd.ErrOrStderr(), req)
			ctx, cancel = f.ctx(cmd)
			defer cancel()
			q, raw, err := cli.UpdateContinuousQuery(ctx, cur.ID, req)
			if err != nil {
				return err
			}
			if outputFormat == output.FormatJSON {
				return writeRawJSON(cmd.OutOrStdout(), raw)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Updated continuous query %q (id %d)\n", clean(q.Name), q.ID)
			writeCQInfo(cmd.OutOrStdout(), q)
			return nil
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	cf.add(c, true)
	return c
}

// ---- delete ----------------------------------------------------------------

func newCQDeleteCmd() *cobra.Command {
	var (
		f   connFlags
		yes bool
	)
	c := &cobra.Command{
		Use:   "delete <id|name>",
		Short: "Delete a continuous query (admin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cli, _, err := f.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := f.ctx(cmd)
			defer cancel()
			q, err := resolveCQRef(ctx, cli, args[0])
			if err != nil {
				return err
			}
			prompt := fmt.Sprintf("Delete continuous query %q (id %d, %s.%s → %s)? Data already written to the destination stays.", clean(q.Name), q.ID, clean(q.Database), clean(q.SourceMeasurement), clean(q.DestinationMeasurement))
			if err := confirmOrAbort(cmd, prompt, yes); err != nil {
				return err
			}
			ctx, cancel = f.ctx(cmd)
			defer cancel()
			if err := cli.DeleteContinuousQuery(ctx, q.ID); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted continuous query %q (id %d)\n", clean(q.Name), q.ID)
			return nil
		},
	}
	f.add(c)
	c.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	return c
}

// ---- execute ---------------------------------------------------------------

func newCQExecuteCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
		start, end   string
		dryRun       bool
	)
	c := &cobra.Command{
		Use:   "execute <id|name>",
		Short: "Run a continuous query now over a time window (admin)",
		Long: `Run a continuous query now (POST /api/v1/continuous_queries/:id/execute).

The window defaults to [last processed time or now−1h, now). --start and
--end (RFC3339) run a backfill instead. The server always moves the
query's last-processed watermark to --end, so a backfill that ends in
the past makes the next scheduled or default run reprocess everything
since; arcli warns when that will happen. --dry-run returns the SQL the
server would run and writes nothing. The call is synchronous and the
server caps it at 10 minutes; --timeout defaults to 10m here.`,
		Example: `  arcli cq execute cpu-1m --dry-run
  arcli cq execute cpu-1m
  arcli cq execute cpu-1m --start 2026-09-01T00:00:00Z --end 2026-09-02T00:00:00Z`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validObjectFormat(outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", outputFormat)
			}
			var opts client.CQExecuteOptions
			opts.DryRun = dryRun
			if start != "" {
				t, err := time.Parse(time.RFC3339, start)
				if err != nil {
					return fmt.Errorf("--start must be RFC3339 (e.g. 2026-09-01T00:00:00Z): %w", err)
				}
				opts.Start = &t
			}
			if end != "" {
				t, err := time.Parse(time.RFC3339, end)
				if err != nil {
					return fmt.Errorf("--end must be RFC3339 (e.g. 2026-09-02T00:00:00Z): %w", err)
				}
				opts.End = &t
			}
			if opts.Start != nil && opts.End != nil && !opts.Start.Before(*opts.End) {
				return fmt.Errorf("--start must be before --end")
			}
			cli, _, err := f.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := f.ctx(cmd)
			defer cancel()
			q, err := resolveCQRef(ctx, cli, args[0])
			if err != nil {
				return err
			}
			stderr := cmd.ErrOrStderr()
			if opts.End != nil && q.LastProcessedTime != nil && !dryRun {
				if lp, perr := time.Parse(time.RFC3339Nano, *q.LastProcessedTime); perr == nil && opts.End.Before(lp) {
					fmt.Fprintf(stderr, "warning: --end %s is earlier than the query's last processed time %s; the server will rewind the watermark and the next run will reprocess everything since\n", opts.End.UTC().Format(time.RFC3339), lp.UTC().Format(time.RFC3339))
				}
			}
			ctx, cancel = f.ctx(cmd)
			defer cancel()
			res, err := cli.ExecuteContinuousQuery(ctx, q.ID, opts)
			if err != nil {
				if isDeadline(err) && !dryRun {
					fmt.Fprintf(stderr, "timed out after %s; the server may still be running the query — check `arcli cq executions %d`\n", f.timeout, q.ID)
				}
				return err
			}
			if outputFormat == output.FormatJSON {
				return writeRawJSON(cmd.OutOrStdout(), res.Raw)
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "status:       %s (%s)\n", clean(res.Status), clean(res.ExecutionID))
			fmt.Fprintf(w, "window:       %s → %s\n", fmtServerTime(res.StartTime, "-"), fmtServerTime(res.EndTime, "-"))
			fmt.Fprintf(w, "destination:  %s\n", clean(res.DestinationMeasurement))
			fmt.Fprintf(w, "written:      %d records\n", res.RecordsWritten)
			fmt.Fprintf(w, "duration:     %s\n", (time.Duration(res.ExecutionTimeSeconds * float64(time.Second))).Round(time.Millisecond))
			if res.ExecutedQuery != "" {
				fmt.Fprintf(w, "query:\n%s\n", cleanMultiline(res.ExecutedQuery))
			}
			return nil
		},
	}
	f.addWithTimeout(c, 10*time.Minute)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	c.Flags().StringVar(&start, "start", "", "window start (RFC3339)")
	c.Flags().StringVar(&end, "end", "", "window end (RFC3339)")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "show the SQL the server would run without executing it")
	return c
}

// ---- executions ------------------------------------------------------------

func newCQExecutionsCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
		noHeader     bool
		limit        int
	)
	c := &cobra.Command{
		Use:   "executions <id|name>",
		Short: "Show a continuous query's execution history, newest first",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validListFormat(outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json, csv)", outputFormat)
			}
			if limit < 1 {
				return fmt.Errorf("--limit must be >= 1 (got %d)", limit)
			}
			cli, _, err := f.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := f.ctx(cmd)
			defer cancel()
			q, err := resolveCQRef(ctx, cli, args[0])
			if err != nil {
				return err
			}
			ctx, cancel = f.ctx(cmd)
			defer cancel()
			ex, raw, err := cli.ListCQExecutions(ctx, q.ID, limit)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if outputFormat == output.FormatJSON {
				return writeRawJSON(w, raw)
			}
			rows := make([][]string, 0, len(ex))
			for _, e := range ex {
				rows = append(rows, []string{
					fmtServerTime(e.ExecutionTime, "-"), clean(e.Status),
					fmtServerTime(e.StartTime, "-") + " → " + fmtServerTime(e.EndTime, "-"),
					strconv.FormatInt(e.RecordsWritten, 10),
					(time.Duration(e.ExecutionDurationSeconds * float64(time.Second))).Round(time.Millisecond).String(),
					strPtr(e.ErrorMessage, "-"),
				})
			}
			if outputFormat == output.FormatCSV {
				cw := csv.NewWriter(w)
				if !noHeader {
					if err := cw.Write([]string{"execution_time", "status", "window", "records_written", "duration", "error"}); err != nil {
						return err
					}
				}
				for _, r := range rows {
					if err := cw.Write(r); err != nil {
						return err
					}
				}
				cw.Flush()
				return cw.Error()
			}
			if len(rows) == 0 {
				_, err := fmt.Fprintf(w, "(no executions for continuous query %q)\n", clean(q.Name))
				return err
			}
			headers := []string{"TIME", "STATUS", "WINDOW", "WRITTEN", "DURATION", "ERROR"}
			if noHeader {
				headers = nil
			}
			return output.Table(w, headers, rows)
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json|csv")
	c.Flags().BoolVar(&noHeader, "no-header", false, "suppress column header row (table + csv)")
	c.Flags().IntVar(&limit, "limit", 50, "maximum executions to show")
	return c
}

// ---- rendering -------------------------------------------------------------

func normaliseList(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func writeCQInfo(w io.Writer, q *client.ContinuousQuery) {
	fmt.Fprintf(w, "id:              %d\n", q.ID)
	fmt.Fprintf(w, "name:            %s\n", clean(q.Name))
	if q.Description != nil && *q.Description != "" {
		fmt.Fprintf(w, "description:     %s\n", clean(*q.Description))
	}
	fmt.Fprintf(w, "database:        %s\n", clean(q.Database))
	fmt.Fprintf(w, "source:          %s\n", clean(q.SourceMeasurement))
	fmt.Fprintf(w, "destination:     %s\n", clean(q.DestinationMeasurement))
	fmt.Fprintf(w, "interval:        %s\n", clean(q.Interval))
	tags := "-"
	if len(q.TagColumns) > 0 {
		cleaned := make([]string, 0, len(q.TagColumns))
		for _, t := range q.TagColumns {
			cleaned = append(cleaned, clean(t))
		}
		tags = strings.Join(cleaned, ", ")
	}
	fmt.Fprintf(w, "tag columns:     %s\n", tags)
	fmt.Fprintf(w, "active:          %t\n", q.IsActive)
	fmt.Fprintf(w, "last run:        %s\n", fmtServerTimePtr(q.LastExecutionTime, "never"))
	fmt.Fprintf(w, "last status:     %s\n", strPtr(q.LastExecutionStatus, "-"))
	fmt.Fprintf(w, "last processed:  %s\n", fmtServerTimePtr(q.LastProcessedTime, "-"))
	fmt.Fprintf(w, "last written:    %s\n", int64Ptr(q.LastRecordsWritten, "-"))
	fmt.Fprintf(w, "created:         %s\n", fmtServerTime(q.CreatedAt, "-"))
	fmt.Fprintf(w, "updated:         %s\n", fmtServerTime(q.UpdatedAt, "-"))
	fmt.Fprintf(w, "query:\n%s\n", cleanMultiline(q.Query))
}

func renderCQList(w io.Writer, list []client.ContinuousQuery, noHeader, asCSV bool) error {
	sorted := make([]client.ContinuousQuery, len(list))
	copy(sorted, list)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	if asCSV {
		cw := csv.NewWriter(w)
		if !noHeader {
			if err := cw.Write([]string{"id", "name", "database", "source_measurement", "destination_measurement", "interval", "is_active", "last_execution_time", "last_execution_status", "last_records_written"}); err != nil {
				return err
			}
		}
		for _, q := range sorted {
			if err := cw.Write([]string{strconv.FormatInt(q.ID, 10), q.Name, q.Database, q.SourceMeasurement, q.DestinationMeasurement, q.Interval, strconv.FormatBool(q.IsActive), fmtServerTimePtr(q.LastExecutionTime, ""), strPtr(q.LastExecutionStatus, ""), int64Ptr(q.LastRecordsWritten, "")}); err != nil {
				return err
			}
		}
		cw.Flush()
		return cw.Error()
	}
	if len(sorted) == 0 {
		_, err := fmt.Fprintln(w, "(no continuous queries)")
		return err
	}
	rows := make([][]string, 0, len(sorted))
	for _, q := range sorted {
		rows = append(rows, []string{strconv.FormatInt(q.ID, 10), clean(q.Name), clean(q.Database), clean(q.SourceMeasurement) + " → " + clean(q.DestinationMeasurement), clean(q.Interval), strconv.FormatBool(q.IsActive),
			fmtServerTimePtr(q.LastExecutionTime, "never"), strPtr(q.LastExecutionStatus, "-"), int64Ptr(q.LastRecordsWritten, "-")})
	}
	headers := []string{"ID", "NAME", "DATABASE", "SOURCE → DEST", "INTERVAL", "ACTIVE", "LAST RUN", "LAST STATUS", "LAST WRITTEN"}
	if noHeader {
		headers = nil
	}
	return output.Table(w, headers, rows)
}
