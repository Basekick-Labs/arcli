// retention subcommand: manage Arc retention policies over
// /api/v1/retention/*.
//
// A policy deletes whole Parquet files whose newest row is older than
// now − (retention_days + buffer_days). Reads work with any token;
// create/update/delete/execute need admin. The server's PUT is a full
// replace, so `update` reads the policy, merges the flags that were
// passed, re-validates, and writes the whole object back.
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

// addTimeoutFlagDefault registers --timeout with a command-specific
// default for calls the server itself budgets more generously (retention
// execute: 30m, cq execute: 10m).
func addTimeoutFlagDefault(c *cobra.Command, timeout *time.Duration, def time.Duration) {
	c.Flags().DurationVar(timeout, "timeout", def, "per-request HTTP timeout")
}

func (f *connFlags) addWithTimeout(c *cobra.Command, def time.Duration) {
	addCommonConnectionFlags(c, &f.connectionName, &f.endpoint, &f.token, &f.insecure)
	addTimeoutFlagDefault(c, &f.timeout, def)
}

// fmtServerTime renders one of Arc's RFC3339 timestamp strings as UTC
// RFC3339. Empty or zero values (the SQLite driver emits
// 0001-01-01T00:00:00Z when it cannot parse a column) render as none; a
// string that is not a timestamp at all is shown as-is (scrubbed).
func fmtServerTime(s, none string) string {
	if s == "" {
		return none
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || t.IsZero() {
		if err != nil {
			return clean(s)
		}
		return none
	}
	return t.UTC().Format(time.RFC3339)
}

func fmtServerTimePtr(p *string, none string) string {
	if p == nil {
		return none
	}
	return fmtServerTime(*p, none)
}

func strPtr(p *string, none string) string {
	if p == nil || *p == "" {
		return none
	}
	return clean(*p)
}

func int64Ptr(p *int64, none string) string {
	if p == nil {
		return none
	}
	return strconv.FormatInt(*p, 10)
}

// isDeadline reports whether err is a client-side timeout on a
// synchronous server call that keeps running after the client gives up.
func isDeadline(err error) bool {
	if err == nil {
		return false
	}
	// errors.Is covers the context path; the string checks catch the
	// http.Client.Timeout wording on older Go versions.
	return errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "context deadline exceeded") || strings.Contains(err.Error(), "Client.Timeout exceeded")
}

// clientGaveUp is isDeadline plus an interrupt (ctrl-C cancels the root
// context): either way the client stopped waiting and the server keeps
// working, which is what the caller needs to tell the operator.
func clientGaveUp(err error) bool {
	return isDeadline(err) || errors.Is(err, context.Canceled)
}

func newRetentionCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "retention",
		Short: "Manage retention policies (age-based data deletion)",
		Long: `Manage retention policies (/api/v1/retention/*).

A policy deletes whole files whose newest row is older than
now − (retention-days + buffer-days), in one database and optionally one
measurement. Listing and showing work with any token; create, update,
delete and execute need an admin token.

On Arc OSS nothing runs policies automatically (the retention scheduler
is an Enterprise feature); run them with "arcli retention execute".`,
	}
	c.AddCommand(
		newRetentionListCmd(), newRetentionShowCmd(), newRetentionCreateCmd(), newRetentionUpdateCmd(),
		newRetentionDeleteCmd(), newRetentionExecuteCmd(), newRetentionExecutionsCmd(),
	)
	return c
}

// resolvePolicyRef turns "<id|name>" into a policy: numeric first (one
// GET), else an exact-name match over the list.
func resolvePolicyRef(ctx context.Context, cli *client.Client, ref string) (*client.RetentionPolicy, error) {
	if ref == "" {
		return nil, fmt.Errorf("policy id or name is required")
	}
	if id, err := strconv.ParseInt(ref, 10, 64); err == nil {
		if id < 0 {
			return nil, fmt.Errorf("policy id must be a non-negative integer (got %s)", ref)
		}
		p, _, err := cli.GetRetentionPolicy(ctx, id)
		return p, err
	}
	list, _, err := cli.ListRetentionPolicies(ctx)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].Name == ref {
			return &list[i], nil
		}
	}
	return nil, fmt.Errorf("no retention policy named %q (run `arcli retention list`)", ref)
}

// ---- list / show -----------------------------------------------------------

func newRetentionListCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
		noHeader     bool
	)
	c := &cobra.Command{
		Use:   "list",
		Short: "List retention policies",
		Args:  cobra.NoArgs,
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
			list, raw, err := cli.ListRetentionPolicies(ctx)
			if err != nil {
				return err
			}
			if outputFormat == output.FormatJSON {
				return writeRawJSON(cmd.OutOrStdout(), raw)
			}
			return renderPolicyList(cmd.OutOrStdout(), list, noHeader, outputFormat == output.FormatCSV)
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json|csv")
	c.Flags().BoolVar(&noHeader, "no-header", false, "suppress column header row (table + csv)")
	return c
}

func newRetentionShowCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
	)
	c := &cobra.Command{
		Use:   "show <id|name>",
		Short: "Show one retention policy",
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
			p, err := resolvePolicyRef(ctx, cli, args[0])
			if err != nil {
				return err
			}
			if outputFormat == output.FormatJSON {
				return writeJSON(cmd.OutOrStdout(), p)
			}
			writePolicyInfo(cmd.OutOrStdout(), p)
			return nil
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	return c
}

// ---- create / update -------------------------------------------------------

type retentionFlags struct {
	name, database, measurement string
	retentionDays, bufferDays   int
	inactive, active            bool
}

func (rf *retentionFlags) add(c *cobra.Command, forUpdate bool) {
	c.Flags().StringVar(&rf.name, "name", "", "policy name (unique)")
	c.Flags().StringVar(&rf.database, "database", "", "database the policy applies to")
	c.Flags().StringVar(&rf.measurement, "measurement", "", "restrict to one measurement (default: all; pass \"\" to clear on update)")
	c.Flags().IntVar(&rf.retentionDays, "retention-days", 0, "delete files whose newest row is older than this many days (plus buffer)")
	c.Flags().IntVar(&rf.bufferDays, "buffer-days", 0, "extra safety margin in days added to retention-days")
	c.Flags().BoolVar(&rf.inactive, "inactive", false, "create the policy disabled / disable it")
	if forUpdate {
		c.Flags().BoolVar(&rf.active, "active", false, "enable the policy")
	}
}

func newRetentionCreateCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
		rf           retentionFlags
	)
	c := &cobra.Command{
		Use:   "create",
		Short: "Create a retention policy (admin)",
		Long: `Create a retention policy (POST /api/v1/retention/).

The policy is active unless --inactive is given (the server's own
default is inactive, which then refuses to execute). Files are deleted
only when their newest row is older than retention-days + buffer-days;
a file straddling the cutoff is kept whole.`,
		Example: `  arcli retention create --name metrics-90d --database metrics --retention-days 90 --buffer-days 7
  arcli retention create --name cpu-30d --database metrics --measurement cpu --retention-days 30`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validObjectFormat(outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", outputFormat)
			}
			req := client.RetentionPolicyRequest{
				Name: rf.name, Database: rf.database, RetentionDays: rf.retentionDays, BufferDays: rf.bufferDays, IsActive: !rf.inactive,
			}
			if rf.measurement != "" {
				m := rf.measurement
				req.Measurement = &m
			}
			if err := client.ValidateRetentionPolicyRequest(req); err != nil {
				return err
			}
			cli, _, err := f.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := f.ctx(cmd)
			defer cancel()
			p, raw, err := cli.CreateRetentionPolicy(ctx, req)
			if err != nil {
				return err
			}
			if outputFormat == output.FormatJSON {
				return writeRawJSON(cmd.OutOrStdout(), raw)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Created retention policy %q (id %d)\n", clean(p.Name), p.ID)
			writePolicyInfo(cmd.OutOrStdout(), p)
			schedulerHint(cmd, &f, cli, "retention", fmt.Sprintf("arcli retention execute %d", p.ID))
			return nil
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	rf.add(c, false)
	_ = c.MarkFlagRequired("name")
	_ = c.MarkFlagRequired("database")
	_ = c.MarkFlagRequired("retention-days")
	return c
}

// schedulerHint tells the operator when nothing on the server will run
// the object they just created (OSS has no schedulers). Best effort.
func schedulerHint(cmd *cobra.Command, f *connFlags, cli *client.Client, which, runCmd string) {
	ctx, cancel := f.ctx(cmd)
	defer cancel()
	st, err := cli.SchedulerStatus(ctx)
	if err != nil {
		return
	}
	block := st.Retention
	if which == "cq" {
		block = st.CQ
	}
	if block.NotRunning() {
		fmt.Fprintf(cmd.ErrOrStderr(), "note: the %s scheduler is not running on this server (%s); run it manually with `%s`\n", which, clean(block.Reason), runCmd)
	} else if !block.Running {
		fmt.Fprintf(cmd.ErrOrStderr(), "note: the %s scheduler is present but not running; run manually with `%s`\n", which, runCmd)
	}
}

func newRetentionUpdateCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
		rf           retentionFlags
	)
	c := &cobra.Command{
		Use:   "update <id|name>",
		Short: "Change fields of a retention policy (admin)",
		Long: `Change one or more fields of a retention policy.

Arc's PUT replaces the whole policy, so arcli reads the current policy,
applies only the flags you pass, re-validates the result, and writes it
back. Last writer wins; there is no conflict detection. If the stored
policy already violates a rule (for example retention-days not greater
than buffer-days), the error names the extra flag to pass.

--measurement "" clears the measurement (policy applies to all).`,
		Example: `  arcli retention update metrics-90d --buffer-days 14
  arcli retention update 3 --inactive`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validObjectFormat(outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", outputFormat)
			}
			fl := cmd.Flags()
			if rf.active && rf.inactive {
				return fmt.Errorf("--active and --inactive are mutually exclusive")
			}
			changed := false
			for _, n := range []string{"name", "database", "measurement", "retention-days", "buffer-days", "active", "inactive"} {
				if fl.Changed(n) {
					changed = true
				}
			}
			if !changed {
				return fmt.Errorf("nothing to update (pass at least one of --name, --database, --measurement, --retention-days, --buffer-days, --active, --inactive)")
			}
			cli, _, err := f.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := f.ctx(cmd)
			defer cancel()
			cur, err := resolvePolicyRef(ctx, cli, args[0])
			if err != nil {
				return err
			}
			req := client.RequestFromPolicy(*cur)
			if fl.Changed("name") {
				if rf.name == "" {
					return fmt.Errorf("--name must not be empty")
				}
				if rf.name != cur.Name {
					// The server answers a rename collision with a bare 500,
					// so check first; if the list itself fails, say so.
					list, _, lerr := cli.ListRetentionPolicies(ctx)
					if lerr != nil {
						return fmt.Errorf("cannot check whether %q is already taken: %w", rf.name, lerr)
					}
					for _, other := range list {
						if other.Name == rf.name && other.ID != cur.ID {
							return fmt.Errorf("a retention policy named %q already exists (id %d)", rf.name, other.ID)
						}
					}
				}
				req.Name = rf.name
			}
			if fl.Changed("database") {
				req.Database = rf.database
			}
			if fl.Changed("measurement") {
				if rf.measurement == "" {
					req.Measurement = nil
				} else {
					m := rf.measurement
					req.Measurement = &m
				}
			}
			if fl.Changed("retention-days") {
				req.RetentionDays = rf.retentionDays
			}
			if fl.Changed("buffer-days") {
				req.BufferDays = rf.bufferDays
			}
			if rf.active {
				req.IsActive = true
			}
			if rf.inactive {
				req.IsActive = false
			}
			if err := client.ValidateRetentionPolicyRequest(req); err != nil {
				if strings.Contains(err.Error(), "days") {
					return fmt.Errorf("merged policy is invalid: %w (stored: retention-days %d, buffer-days %d; pass --retention-days and/or --buffer-days to fix both together)", err, cur.RetentionDays, cur.BufferDays)
				}
				return fmt.Errorf("merged policy is invalid: %w", err)
			}
			ctx, cancel = f.ctx(cmd)
			defer cancel()
			p, raw, err := cli.UpdateRetentionPolicy(ctx, cur.ID, req)
			if err != nil {
				return err
			}
			if outputFormat == output.FormatJSON {
				return writeRawJSON(cmd.OutOrStdout(), raw)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Updated retention policy %q (id %d)\n", clean(p.Name), p.ID)
			writePolicyInfo(cmd.OutOrStdout(), p)
			return nil
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	rf.add(c, true)
	return c
}

// ---- delete ----------------------------------------------------------------

func newRetentionDeleteCmd() *cobra.Command {
	var (
		f   connFlags
		yes bool
	)
	c := &cobra.Command{
		Use:   "delete <id|name>",
		Short: "Delete a retention policy and its execution history (admin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cli, _, err := f.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := f.ctx(cmd)
			defer cancel()
			p, err := resolvePolicyRef(ctx, cli, args[0])
			if err != nil {
				return err
			}
			q := fmt.Sprintf("Delete retention policy %q (id %d, database %s) and its execution history?", clean(p.Name), p.ID, clean(p.Database))
			if err := confirmOrAbort(cmd, q, yes); err != nil {
				return err
			}
			ctx, cancel = f.ctx(cmd)
			defer cancel()
			if err := cli.DeleteRetentionPolicy(ctx, p.ID); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted retention policy %q (id %d)\n", clean(p.Name), p.ID)
			return nil
		},
	}
	f.add(c)
	c.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	return c
}

// ---- execute ---------------------------------------------------------------

func newRetentionExecuteCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
		dryRun, yes  bool
	)
	c := &cobra.Command{
		Use:   "execute <id|name>",
		Short: "Run a retention policy now (admin, deletes data)",
		Long: `Run a retention policy now (POST /api/v1/retention/:id/execute).

The server scans the database and deletes every file whose newest row
is older than the cutoff it computes at that moment
(now − retention-days − buffer-days). The call is synchronous and can
take a long time on large databases; --timeout defaults to 30m here.

Without --yes, arcli first asks the server for a dry run and shows its
cutoff, file and row counts, and affected measurements in the
confirmation prompt. --dry-run only reports and never deletes.

If the client times out, the server keeps deleting; check
"arcli retention executions <id>" afterwards. On a cluster, only the
primary writer may execute; a reader node accepts the dry-run preflight
but refuses the real call (HTTP 503).`,
		Example: `  arcli retention execute metrics-90d --dry-run
  arcli retention execute metrics-90d
  arcli retention execute 3 --yes --timeout 2h`,
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
			p, err := resolvePolicyRef(ctx, cli, args[0])
			if err != nil {
				return err
			}
			stderr := cmd.ErrOrStderr()
			if !dryRun && !yes {
				// Refuse a non-interactive stdin before spending a
				// server-side scan on the preflight.
				if err := requireInteractiveStdin(cmd); err != nil {
					return err
				}
				pctx, pcancel := f.ctx(cmd)
				preview, perr := cli.ExecuteRetentionPolicy(pctx, p.ID, true)
				pcancel()
				if perr != nil {
					return fmt.Errorf("dry-run preflight failed: %w", perr)
				}
				q := fmt.Sprintf("Delete %d files (%d rows) whose newest row is older than %s from database %q (measurements: %s)? The server recomputes the cutoff at execution time.",
					preview.FilesDeleted, preview.DeletedCount, fmtServerTime(preview.CutoffDate, "-"), clean(p.Database), measurementsOrNone(preview.AffectedMeasurements))
				if preview.FilesDeleted == 0 {
					q = fmt.Sprintf("The dry run found nothing older than %s to delete in database %q. Run the policy anyway (an execution is still recorded)?", fmtServerTime(preview.CutoffDate, "-"), clean(p.Database))
				}
				if err := confirmOrAbort(cmd, q, false); err != nil {
					return err
				}
			}
			ctx, cancel = f.ctx(cmd)
			defer cancel()
			res, err := cli.ExecuteRetentionPolicy(ctx, p.ID, dryRun)
			if err != nil {
				if clientGaveUp(err) && !dryRun {
					fmt.Fprintf(stderr, "arcli stopped waiting (timeout %s or interrupt); the deletion continues server-side — check `arcli retention executions %d`\n", f.timeout, p.ID)
				}
				return err
			}
			if outputFormat == output.FormatJSON {
				return writeRawJSON(cmd.OutOrStdout(), res.Raw)
			}
			w := cmd.OutOrStdout()
			verb := "Deleted"
			if res.DryRun {
				verb = "Would delete"
			}
			fmt.Fprintf(w, "%s %d files (%d rows) older than %s from database %q\n", verb, res.FilesDeleted, res.DeletedCount, fmtServerTime(res.CutoffDate, "-"), clean(p.Database))
			fmt.Fprintf(w, "measurements: %s\n", measurementsOrNone(res.AffectedMeasurements))
			fmt.Fprintf(w, "duration:     %s\n", (time.Duration(res.ExecutionTimeMs * float64(time.Millisecond))).Round(time.Millisecond))
			return nil
		},
	}
	f.addWithTimeout(c, 30*time.Minute)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "report what would be deleted without deleting")
	c.Flags().BoolVarP(&yes, "yes", "y", false, "skip the dry-run preflight and the confirmation prompt")
	return c
}

func measurementsOrNone(ms []string) string {
	if len(ms) == 0 {
		return "(none)"
	}
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, clean(m))
	}
	return strings.Join(out, ", ")
}

// ---- executions ------------------------------------------------------------

func newRetentionExecutionsCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
		noHeader     bool
		limit        int
	)
	c := &cobra.Command{
		Use:   "executions <id|name>",
		Short: "Show a policy's execution history, newest first",
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
			p, err := resolvePolicyRef(ctx, cli, args[0])
			if err != nil {
				return err
			}
			ctx, cancel = f.ctx(cmd)
			defer cancel()
			ex, raw, err := cli.ListRetentionExecutions(ctx, p.ID, limit)
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
					fmtServerTime(e.ExecutionTime, "-"), clean(e.Status), strconv.FormatInt(e.DeletedCount, 10),
					fmtServerTimePtr(e.CutoffDate, "-"), (time.Duration(e.ExecutionDurationMs * float64(time.Millisecond))).Round(time.Millisecond).String(),
					strPtr(e.ErrorMessage, "-"),
				})
			}
			headers := []string{"TIME", "STATUS", "DELETED", "CUTOFF", "DURATION", "ERROR"}
			if outputFormat == output.FormatCSV {
				cw := csv.NewWriter(w)
				if !noHeader {
					if err := cw.Write([]string{"execution_time", "status", "deleted_count", "cutoff_date", "duration", "error"}); err != nil {
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
				_, err := fmt.Fprintf(w, "(no executions for policy %q)\n", clean(p.Name))
				return err
			}
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

func writePolicyInfo(w io.Writer, p *client.RetentionPolicy) {
	fmt.Fprintf(w, "id:             %d\n", p.ID)
	fmt.Fprintf(w, "name:           %s\n", clean(p.Name))
	fmt.Fprintf(w, "database:       %s\n", clean(p.Database))
	fmt.Fprintf(w, "measurement:    %s\n", strPtr(p.Measurement, "(all)"))
	fmt.Fprintf(w, "retention:      %d days (+%d buffer)\n", p.RetentionDays, p.BufferDays)
	fmt.Fprintf(w, "active:         %t\n", p.IsActive)
	fmt.Fprintf(w, "last run:       %s\n", fmtServerTimePtr(p.LastExecutionTime, "never"))
	fmt.Fprintf(w, "last status:    %s\n", strPtr(p.LastExecutionStatus, "-"))
	fmt.Fprintf(w, "last deleted:   %s\n", int64Ptr(p.LastDeletedCount, "-"))
	fmt.Fprintf(w, "created:        %s\n", fmtServerTime(p.CreatedAt, "-"))
	fmt.Fprintf(w, "updated:        %s\n", fmtServerTime(p.UpdatedAt, "-"))
}

func renderPolicyList(w io.Writer, list []client.RetentionPolicy, noHeader, asCSV bool) error {
	sorted := make([]client.RetentionPolicy, len(list))
	copy(sorted, list)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	if asCSV {
		cw := csv.NewWriter(w)
		if !noHeader {
			if err := cw.Write([]string{"id", "name", "database", "measurement", "retention_days", "buffer_days", "is_active", "last_execution_time", "last_execution_status", "last_deleted_count"}); err != nil {
				return err
			}
		}
		for _, p := range sorted {
			if err := cw.Write([]string{strconv.FormatInt(p.ID, 10), p.Name, p.Database, strPtr(p.Measurement, ""), strconv.Itoa(p.RetentionDays), strconv.Itoa(p.BufferDays), strconv.FormatBool(p.IsActive), fmtServerTimePtr(p.LastExecutionTime, ""), strPtr(p.LastExecutionStatus, ""), int64Ptr(p.LastDeletedCount, "")}); err != nil {
				return err
			}
		}
		cw.Flush()
		return cw.Error()
	}
	if len(sorted) == 0 {
		_, err := fmt.Fprintln(w, "(no retention policies)")
		return err
	}
	rows := make([][]string, 0, len(sorted))
	for _, p := range sorted {
		rows = append(rows, []string{strconv.FormatInt(p.ID, 10), clean(p.Name), clean(p.Database), strPtr(p.Measurement, "(all)"),
			strconv.Itoa(p.RetentionDays) + "d", strconv.Itoa(p.BufferDays) + "d", strconv.FormatBool(p.IsActive),
			fmtServerTimePtr(p.LastExecutionTime, "never"), strPtr(p.LastExecutionStatus, "-"), int64Ptr(p.LastDeletedCount, "-")})
	}
	headers := []string{"ID", "NAME", "DATABASE", "MEASUREMENT", "RETENTION", "BUFFER", "ACTIVE", "LAST RUN", "LAST STATUS", "LAST DELETED"}
	if noHeader {
		headers = nil
	}
	return output.Table(w, headers, rows)
}
