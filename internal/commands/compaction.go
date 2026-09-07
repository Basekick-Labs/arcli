// compaction subcommand: observe and trigger Arc's background
// compaction over /api/v1/compaction/*.
//
// The routes exist only when compaction.enabled=true on the server;
// otherwise every subcommand reports "compaction is disabled". Note
// that Arc's /compaction/jobs endpoint is a stub (always zero jobs) and
// is deliberately not exposed here.
package commands

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/basekick-labs/arcli/internal/client"
	"github.com/basekick-labs/arcli/internal/output"
)

func newCompactionCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "compaction",
		Short: "Observe and trigger background compaction",
		Long: `Observe and trigger Arc's background compaction (/api/v1/compaction/*).

Compaction merges small Parquet files per partition on the "hourly" and
"daily" tiers. "trigger" requires an admin token; the rest work with any
token. When compaction.enabled=false on the server every subcommand
reports that it is disabled.`,
	}
	c.AddCommand(
		newCompactionStatusCmd(),
		newCompactionStatsCmd(),
		newCompactionCandidatesCmd(),
		newCompactionHistoryCmd(),
		newCompactionTriggerCmd(),
	)
	return c
}

// ---- status ----------------------------------------------------------------

func newCompactionStatusCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
	)
	c := &cobra.Command{
		Use:   "status",
		Short: "Show compaction manager counters and scheduler state",
		Long: `Show compaction manager counters and per-tier scheduler state
(GET /api/v1/compaction/status). Next-run times are shown in UTC.`,
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
			st, err := cli.CompactionStatus(ctx)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if outputFormat == output.FormatJSON {
				return writeRawJSON(w, st.Raw)
			}
			active := "-"
			if st.Manager.ActiveJobs != nil {
				active = strconv.Itoa(*st.Manager.ActiveJobs)
			}
			fmt.Fprintf(w, "jobs:        %d completed, %d failed, active %s\n", st.Manager.TotalCompleted, st.Manager.TotalFailed, active)
			if len(st.Schedulers) == 0 {
				_, err := fmt.Fprintln(w, "schedulers:  (none)")
				return err
			}
			names := make([]string, 0, len(st.Schedulers))
			for n := range st.Schedulers {
				names = append(names, n)
			}
			sort.Strings(names)
			rows := make([][]string, 0, len(names))
			for _, n := range names {
				s := st.Schedulers[n]
				next := "-"
				if s.NextRun != nil && !s.NextRun.IsZero() {
					next = s.NextRun.UTC().Format(time.RFC3339)
				}
				gate := "-"
				if s.RoleGated != nil {
					gate = strconv.FormatBool(*s.RoleGated)
					if s.GateRole != "" {
						gate += " (" + clean(s.GateRole) + ")"
					}
				}
				rows = append(rows, []string{clean(n), strconv.FormatBool(s.Enabled), strconv.FormatBool(s.Running), clean(s.Schedule), next, gate})
			}
			fmt.Fprintln(w)
			return output.Table(w, []string{"SCHEDULER", "ENABLED", "RUNNING", "SCHEDULE", "NEXT RUN UTC", "ROLE GATED"}, rows)
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	return c
}

// ---- stats -----------------------------------------------------------------

func newCompactionStatsCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
	)
	c := &cobra.Command{
		Use:   "stats",
		Short: "Show lifetime compaction totals, per-tier settings, and recent jobs",
		Long: `Show lifetime compaction totals, per-tier settings, and the most recent
jobs (GET /api/v1/compaction/stats). Byte totals are shown with binary
units in table mode; -o json carries the server's raw numbers.`,
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
			st, err := cli.CompactionStats(ctx)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if outputFormat == output.FormatJSON {
				return writeRawJSON(w, st.Raw)
			}
			fmt.Fprintf(w, "jobs:            %d completed, %d failed\n", st.TotalJobsCompleted, st.TotalJobsFailed)
			fmt.Fprintf(w, "files compacted: %d\n", st.TotalFilesCompacted)
			fmt.Fprintf(w, "bytes saved:     %s\n", humanBytes(st.TotalBytesSaved))
			fmt.Fprintf(w, "manifests:       %d recovered\n", st.TotalManifestsRecover)
			cycle := "idle"
			if st.CycleRunning {
				cycle = "running"
			}
			fmt.Fprintf(w, "cycle:           %s (current id %d)\n", cycle, st.CurrentCycleID)
			fmt.Fprintln(w)
			if len(st.Tiers) == 0 {
				fmt.Fprintln(w, "(no tiers enabled)")
			} else {
				rows := make([][]string, 0, len(st.Tiers))
				for _, t := range st.Tiers {
					rows = append(rows, []string{clean(t.Tier), strconv.FormatBool(t.Enabled), strconv.Itoa(t.MinAgeHours) + "h", strconv.Itoa(t.MinFiles),
						strconv.FormatInt(t.TotalCompactions, 10), strconv.FormatInt(t.TotalFilesCompacted, 10), humanBytes(t.TotalBytesSaved)})
				}
				if err := output.Table(w, []string{"TIER", "ENABLED", "MIN AGE", "MIN FILES", "COMPACTIONS", "FILES", "SAVED"}, rows); err != nil {
					return err
				}
			}
			fmt.Fprintln(w)
			if len(st.RecentJobs) == 0 {
				_, err := fmt.Fprintln(w, "(no recent jobs)")
				return err
			}
			return renderJobTable(w, st.RecentJobs, false, false)
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	return c
}

// ---- candidates ------------------------------------------------------------

func newCompactionCandidatesCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
		noHeader     bool
		database     string
	)
	c := &cobra.Command{
		Use:   "candidates",
		Short: "List partitions eligible for compaction right now",
		Long: `List partitions that currently qualify for compaction
(GET /api/v1/compaction/candidates). This scans storage on the server
(30s server-side limit), so it can be slow on large deployments.

The server has no database filter; --database filters client-side.

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
			cands, raw, err := cli.CompactionCandidates(ctx)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if outputFormat == output.FormatJSON && database == "" {
				return writeRawJSON(w, raw)
			}
			if database != "" {
				filtered := cands[:0:0]
				for _, c := range cands {
					if c.Database == database {
						filtered = append(filtered, c)
					}
				}
				cands = filtered
			}
			sort.Slice(cands, func(i, j int) bool {
				if cands[i].Database != cands[j].Database {
					return cands[i].Database < cands[j].Database
				}
				return cands[i].PartitionPath < cands[j].PartitionPath
			})
			switch outputFormat {
			case output.FormatJSON:
				return writeJSON(w, struct {
					Count      int                          `json:"count"`
					Candidates []client.CompactionCandidate `json:"candidates"`
				}{len(cands), cands})
			case output.FormatCSV:
				cw := csv.NewWriter(w)
				if !noHeader {
					if err := cw.Write([]string{"database", "measurement", "partition_path", "file_count", "tier"}); err != nil {
						return err
					}
				}
				for _, c := range cands {
					if err := cw.Write([]string{c.Database, c.Measurement, c.PartitionPath, strconv.Itoa(c.FileCount), c.Tier}); err != nil {
						return err
					}
				}
				cw.Flush()
				return cw.Error()
			default:
				if len(cands) == 0 {
					_, err := fmt.Fprintln(w, "(no compaction candidates)")
					return err
				}
				rows := make([][]string, 0, len(cands))
				for _, c := range cands {
					rows = append(rows, []string{clean(c.Database), clean(c.Measurement), clean(c.PartitionPath), strconv.Itoa(c.FileCount), clean(c.Tier)})
				}
				headers := []string{"DATABASE", "MEASUREMENT", "PARTITION", "FILES", "TIER"}
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
	c.Flags().StringVar(&database, "database", "", "only show candidates in this database (client-side filter)")
	return c
}

// ---- history ---------------------------------------------------------------

func newCompactionHistoryCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
		noHeader     bool
		limit        int
	)
	c := &cobra.Command{
		Use:   "history",
		Short: "Show the most recent compaction jobs",
		Long: `Show the most recent compaction jobs (GET /api/v1/compaction/history).

The server returns at most the 10 most recent jobs, so --limit above
10 has no effect. Jobs carry no timestamp; rows are in the order
the server ran them, oldest first. "total" counts successful jobs since
the server started. SAVED% is the fraction of bytes removed.

Output formats: table (default) | json | csv`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validListFormat(outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json, csv)", outputFormat)
			}
			if limit < 1 {
				return fmt.Errorf("--limit must be >= 1 (got %d)", limit)
			}
			if limit > 10 {
				fmt.Fprintln(cmd.ErrOrStderr(), "note: the server returns at most the 10 most recent jobs")
			}
			cli, _, err := f.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := f.ctx(cmd)
			defer cancel()
			h, err := cli.CompactionHistory(ctx, limit)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			switch outputFormat {
			case output.FormatJSON:
				return writeRawJSON(w, h.Raw)
			case output.FormatCSV:
				return renderJobTable(w, h.RecentJobs, noHeader, true)
			default:
				fmt.Fprintf(w, "total: %d successful jobs since server start\n", h.TotalJobs)
				if len(h.RecentJobs) == 0 {
					_, err := fmt.Fprintln(w, "(no recent jobs)")
					return err
				}
				return renderJobTable(w, h.RecentJobs, noHeader, false)
			}
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json|csv")
	c.Flags().BoolVar(&noHeader, "no-header", false, "suppress column header row (table + csv)")
	c.Flags().IntVar(&limit, "limit", 10, "maximum jobs to show (server caps at 10)")
	return c
}

// ---- trigger ---------------------------------------------------------------

func newCompactionTriggerCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
		tiers        []string
		database     string
		wait         bool
		waitTimeout  time.Duration
	)
	c := &cobra.Command{
		Use:   "trigger",
		Short: "Start a compaction cycle now (admin)",
		Long: `Start a compaction cycle now (POST /api/v1/compaction/trigger).

The cycle runs asynchronously on the server; the response carries the
cycle id the server expects to assign. With --wait arcli polls
"compaction stats" until that cycle has finished (bounded by
--wait-timeout, default 30m, the server's own cycle limit). Without
--wait, check progress with "arcli compaction status" or "stats".

--tier limits the cycle to hourly and/or daily; a tier that is disabled
or not configured on the server is skipped silently by the server, so
arcli warns first. If another cycle is already running the server
answers 409 and arcli reports its id.

Two server-side races affect --wait: the expected cycle id can be one
too high, and a scheduled cycle starting at the same instant drops the
manual trigger. In both cases arcli stops waiting once the server sits
idle below the expected id and tells you to check "compaction stats".`,
		Example: `  arcli compaction trigger
  arcli compaction trigger --tier hourly --database metrics --wait`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validObjectFormat(outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", outputFormat)
			}
			tiers = normaliseTiers(tiers)
			for _, t := range tiers {
				if !containsString(client.CompactionTiers, t) {
					return fmt.Errorf("invalid --tier %q (valid: %s)", t, strings.Join(client.CompactionTiers, ", "))
				}
			}
			if waitTimeout <= 0 {
				return fmt.Errorf("--wait-timeout must be > 0")
			}
			cli, _, err := f.client(cmd)
			if err != nil {
				return err
			}
			stderr := cmd.ErrOrStderr()

			if len(tiers) > 0 {
				// The server silently skips disabled or unconfigured
				// tiers; say so up front. This check is advisory: only a
				// "compaction disabled" answer is fatal, any other
				// failure just costs the warning.
				ctx, cancel := f.ctx(cmd)
				st, serr := cli.CompactionStats(ctx)
				cancel()
				var disabled *client.CompactionDisabledError
				switch {
				case errors.As(serr, &disabled):
					return serr
				case serr != nil:
					fmt.Fprintf(stderr, "warning: could not check tier state (%v); continuing\n", serr)
				default:
					for _, want := range tiers {
						found := false
						for _, have := range st.Tiers {
							if have.Tier != want {
								continue
							}
							found = true
							if !have.Enabled {
								fmt.Fprintf(stderr, "warning: tier %q is disabled on the server and will be skipped\n", want)
							}
						}
						if !found {
							fmt.Fprintf(stderr, "warning: tier %q is not configured on the server and will be skipped\n", want)
						}
					}
				}
			}

			ctx, cancel := f.ctx(cmd)
			defer cancel()
			res, err := cli.TriggerCompaction(ctx, tiers, database)
			if err != nil {
				return err
			}
			if len(res.Tiers) == 0 {
				fmt.Fprintf(stderr, "warning: no tiers are enabled on the server; the cycle will do nothing\n")
			}
			if outputFormat == output.FormatJSON && !wait {
				return writeRawJSON(cmd.OutOrStdout(), res.Raw)
			}
			scope := "all databases"
			if res.Database != "" {
				scope = "database " + clean(res.Database)
			}
			tierNames := make([]string, 0, len(res.Tiers))
			for _, t := range res.Tiers {
				tierNames = append(tierNames, clean(t))
			}
			// In JSON mode stdout is reserved for the final JSON document,
			// so the progress line goes to stderr.
			progress := cmd.OutOrStdout()
			if outputFormat == output.FormatJSON {
				progress = stderr
			}
			fmt.Fprintf(progress, "Compaction cycle requested (expected cycle id %d) for tiers [%s] on %s\n", res.CycleID, strings.Join(tierNames, ", "), scope)
			if !wait {
				fmt.Fprintln(stderr, "It runs asynchronously; follow it with `arcli compaction status` or `arcli compaction stats`.")
				return nil
			}
			final, err := waitForCycle(cmd.Context(), cli, res.CycleID, f.timeout, waitTimeout)
			if err != nil {
				return err
			}
			if outputFormat == output.FormatJSON {
				return writeRawJSON(cmd.OutOrStdout(), final.Raw)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Cycle %d finished: %d jobs completed, %d failed, %s saved in total\n", final.CurrentCycleID, final.TotalJobsCompleted, final.TotalJobsFailed, humanBytes(final.TotalBytesSaved))
			return nil
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	c.Flags().StringSliceVar(&tiers, "tier", nil, "tier(s) to run: hourly, daily (repeat or comma-join; default all enabled)")
	c.Flags().StringVar(&database, "database", "", "limit the cycle to one database")
	c.Flags().BoolVar(&wait, "wait", false, "poll until the cycle has finished")
	c.Flags().DurationVar(&waitTimeout, "wait-timeout", 30*time.Minute, "give up waiting after this long (with --wait)")
	return c
}

// waitPollInterval is the first poll delay for --wait; it grows by one
// second per poll up to waitPollMax. A variable so tests can shrink it.
var (
	waitPollInterval = 2 * time.Second
	waitPollMax      = 10 * time.Second
)

// waitForCycle polls /compaction/stats until the server reports the
// expected cycle id as assigned and no cycle running. Each poll gets
// its own request timeout; the overall wait is bounded separately.
//
// The expected id is the server's prediction (current+1 read after the
// cycle goroutine is spawned), so it can be one too high when the
// goroutine wins the race; and a scheduled cycle starting at the same
// moment drops the manual trigger entirely. Rather than sleep to the
// deadline in either case, give up once the server has sat idle below
// the expected id for a few polls.
func waitForCycle(parent context.Context, cli *client.Client, cycleID int64, reqTimeout, waitTimeout time.Duration) (*client.CompactionStats, error) {
	deadline := time.Now().Add(waitTimeout)
	interval := waitPollInterval
	idleBelow := 0
	for {
		ctx, cancel := context.WithTimeout(parent, reqTimeout)
		st, err := cli.CompactionStats(ctx)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("while waiting for cycle %d: %w", cycleID, err)
		}
		if st.CurrentCycleID >= cycleID && !st.CycleRunning {
			return st, nil
		}
		if !st.CycleRunning && st.CurrentCycleID < cycleID {
			idleBelow++
			if idleBelow >= 3 {
				return nil, fmt.Errorf("no cycle %d was started (server is idle at cycle %d); the trigger may have raced a scheduled cycle — check `arcli compaction stats` and retry", cycleID, st.CurrentCycleID)
			}
		} else {
			idleBelow = 0
		}
		if time.Now().After(deadline) {
			if st.CycleRunning {
				return nil, fmt.Errorf("cycle %d still running after %s; it continues on the server", st.CurrentCycleID, waitTimeout)
			}
			return nil, fmt.Errorf("gave up after %s waiting for cycle %d (server at cycle %d, idle)", waitTimeout, cycleID, st.CurrentCycleID)
		}
		select {
		case <-parent.Done():
			return nil, parent.Err()
		case <-time.After(interval):
		}
		if interval < waitPollMax {
			interval += time.Second
		}
	}
}

// csvInt / csvInt64 render an optional number as an empty CSV cell when
// absent, otherwise its exact decimal form.
func csvInt(p *int) string {
	if p == nil {
		return ""
	}
	return strconv.Itoa(*p)
}

func csvInt64(p *int64) string {
	if p == nil {
		return ""
	}
	return strconv.FormatInt(*p, 10)
}

func normaliseTiers(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, t := range in {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// renderJobTable writes compaction jobs in server order as a table or CSV.
func renderJobTable(w io.Writer, jobs []client.CompactionJob, noHeader, asCSV bool) error {
	optInt := func(p *int) string {
		if p == nil {
			return "-"
		}
		return strconv.Itoa(*p)
	}
	optBytes := func(p *int64) string {
		if p == nil {
			return "-"
		}
		return humanBytes(*p)
	}
	saved := func(p *float64) string {
		if p == nil {
			return "-"
		}
		return fmt.Sprintf("%.1f%%", *p*100)
	}
	result := func(j client.CompactionJob) string {
		switch {
		case j.Success == nil && j.Error == "":
			return "-"
		case j.Error != "":
			return "failed: " + clean(j.Error)
		case *j.Success:
			return "ok"
		default:
			return "failed"
		}
	}
	if asCSV {
		cw := csv.NewWriter(w)
		if !noHeader {
			if err := cw.Write([]string{"database", "measurement", "partition_path", "tier", "files_compacted", "bytes_before", "bytes_after", "saved_fraction", "result"}); err != nil {
				return err
			}
		}
		for _, j := range jobs {
			sf := ""
			if j.CompressionRatio != nil {
				sf = strconv.FormatFloat(*j.CompressionRatio, 'f', -1, 64)
			}
			if err := cw.Write([]string{j.Database, j.Measurement, j.PartitionPath, j.Tier, csvInt(j.FilesCompacted), csvInt64(j.BytesBefore), csvInt64(j.BytesAfter), sf, result(j)}); err != nil {
				return err
			}
		}
		cw.Flush()
		return cw.Error()
	}
	rows := make([][]string, 0, len(jobs))
	for _, j := range jobs {
		rows = append(rows, []string{clean(j.Database), clean(j.Measurement), clean(j.PartitionPath), clean(j.Tier), optInt(j.FilesCompacted), optBytes(j.BytesBefore), optBytes(j.BytesAfter), saved(j.CompressionRatio), result(j)})
	}
	headers := []string{"DATABASE", "MEASUREMENT", "PARTITION", "TIER", "FILES", "BEFORE", "AFTER", "SAVED%", "RESULT"}
	if noHeader {
		headers = nil
	}
	return output.Table(w, headers, rows)
}
