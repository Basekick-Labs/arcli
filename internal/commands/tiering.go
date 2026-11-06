// tiering exposes Arc's tiered-storage status, file, migration, scan, and
// per-database policy endpoints.
package commands

import (
	"encoding/csv"
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

func newTieringCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "tiering",
		Short: "Inspect and manage tiered storage",
		Long: `Inspect and manage Arc tiered storage (/api/v1/tiering/*).

These endpoints require an administrator token and an Enterprise licence
with the tiering feature enabled. File listings are bounded by --limit;
the current Arc API does not expose an offset or cursor for this endpoint.
CSV preserves original field values with lowercase snake_case headers.
Table and CSV timestamps use UTC RFC3339Nano; JSON retains the server response.`,
	}
	c.AddCommand(
		newTieringStatusCmd(),
		newTieringFileCmd(),
		newTieringStatsCmd(),
		newTieringMigrateCmd(),
		newTieringScanCmd(),
		newTieringPolicyCmd(),
	)
	return c
}

func newTieringStatusCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
	)
	c := &cobra.Command{
		Use:   "status",
		Short: "Show tiering configuration and scheduler state",
		Args:  cobra.NoArgs,
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
			status, err := cli.TieringStatus(ctx)
			if err != nil {
				return err
			}
			if outputFormat == output.FormatJSON {
				return writeRawJSON(cmd.OutOrStdout(), status.Raw)
			}
			return renderTieringStatus(cmd.OutOrStdout(), status)
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	return c
}

func newTieringFileCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
		noHeader     bool
		tier         string
		database     string
		limit        int
	)
	c := &cobra.Command{
		Use:   "file",
		Short: "List indexed files in tiered storage",
		Long: `List indexed files (GET /api/v1/tiering/files).

The response is bounded by --limit (default 100). Arc currently accepts
either a tier or database filter, plus limit, but has no offset or cursor
parameter. The API ignores tier when database is also set, so arcli rejects
that combination rather than returning an incorrectly filtered list.`,
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
			files, raw, err := cli.ListTieringFiles(ctx, tier, database, limit)
			if err != nil {
				return err
			}
			if outputFormat == output.FormatJSON {
				return writeRawJSON(cmd.OutOrStdout(), raw)
			}
			return renderTieringFiles(cmd.OutOrStdout(), files, noHeader, outputFormat == output.FormatCSV)
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json|csv")
	c.Flags().BoolVar(&noHeader, "no-header", false, "suppress column header row (table + csv)")
	c.Flags().StringVar(&tier, "tier", "", "filter by tier: hot or cold")
	c.Flags().StringVar(&database, "database", "", "filter by database")
	c.Flags().IntVar(&limit, "limit", 100, "maximum number of files returned")
	return c
}

func newTieringStatsCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
	)
	c := &cobra.Command{
		Use:   "stats",
		Short: "Show tier file totals and recent migrations",
		Args:  cobra.NoArgs,
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
			stats, err := cli.TieringStats(ctx)
			if err != nil {
				return err
			}
			if outputFormat == output.FormatJSON {
				return writeRawJSON(cmd.OutOrStdout(), stats.Raw)
			}
			return renderTieringStats(cmd.OutOrStdout(), stats)
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	return c
}

func newTieringMigrateCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
		database     string
		measurement  string
		fromTier     string
		toTier       string
		dryRun       bool
	)
	c := &cobra.Command{
		Use:   "migrate",
		Short: "Move eligible files from hot to cold storage (admin)",
		Long: `Run a tier migration cycle (POST /api/v1/tiering/migrate).

Arc currently supports hot-to-cold migration. Use --dry-run to preview the
number of files and bytes before starting a migration. Database and
measurement filters limit the cycle to one target.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if f.timeout <= 0 {
				return fmt.Errorf("--timeout must be > 0")
			}
			if !validObjectFormat(outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", outputFormat)
			}
			cli, _, err := f.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := f.ctx(cmd)
			defer cancel()
			result, err := cli.MigrateTiering(ctx, client.TieringMigrationRequest{
				FromTier: fromTier, ToTier: toTier, Database: database,
				Measurement: measurement, DryRun: dryRun,
			})
			if err != nil {
				return err
			}
			if outputFormat == output.FormatJSON {
				return writeRawJSON(cmd.OutOrStdout(), result.Raw)
			}
			return renderTieringMigration(cmd.OutOrStdout(), result)
		},
	}
	f.addWithTimeout(c, 2*time.Hour)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	c.Flags().StringVar(&database, "database", "", "limit the cycle to one database")
	c.Flags().StringVar(&measurement, "measurement", "", "limit the cycle to one measurement")
	c.Flags().StringVar(&fromTier, "from-tier", "", "source tier (Arc currently supports hot)")
	c.Flags().StringVar(&toTier, "to-tier", "", "destination tier (Arc currently supports cold)")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "preview migration without moving files")
	return c
}

func newTieringScanCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
	)
	c := &cobra.Command{
		Use:   "scan",
		Short: "Refresh the tier file index (admin)",
		Long:  "Refresh tier metadata from configured storage backends (POST /api/v1/tiering/scan).",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if f.timeout <= 0 {
				return fmt.Errorf("--timeout must be > 0")
			}
			if !validObjectFormat(outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", outputFormat)
			}
			cli, _, err := f.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := f.ctx(cmd)
			defer cancel()
			result, raw, err := cli.ScanTieringFiles(ctx)
			if err != nil {
				return err
			}
			if outputFormat == output.FormatJSON {
				return writeRawJSON(cmd.OutOrStdout(), raw)
			}
			return renderTieringScan(cmd.OutOrStdout(), result)
		},
	}
	f.addWithTimeout(c, 2*time.Hour)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	return c
}

func newTieringPolicyCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "policy",
		Short: "Manage per-database tiering policies",
	}
	c.AddCommand(
		newTieringPolicyListCmd(),
		newTieringPolicyShowCmd(),
		newTieringPolicySetCmd(),
		newTieringPolicyDeleteCmd(),
		newTieringPolicyEffectiveCmd(),
	)
	return c
}

func newTieringPolicyListCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
		noHeader     bool
	)
	c := &cobra.Command{
		Use:   "list",
		Short: "List custom tiering policies",
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
			list, err := cli.ListTieringPolicies(ctx)
			if err != nil {
				return err
			}
			if outputFormat == output.FormatJSON {
				return writeRawJSON(cmd.OutOrStdout(), list.Raw)
			}
			return renderTieringPolicyList(cmd.OutOrStdout(), list.Policies, noHeader, outputFormat == output.FormatCSV)
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json|csv")
	c.Flags().BoolVar(&noHeader, "no-header", false, "suppress column header row (table + csv)")
	return c
}

func newTieringPolicyShowCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
	)
	c := &cobra.Command{
		Use:   "show <database>",
		Short: "Show a database's custom tiering policy",
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
			policy, raw, err := cli.GetTieringPolicy(ctx, args[0])
			if err != nil {
				return err
			}
			if outputFormat == output.FormatJSON {
				return writeRawJSON(cmd.OutOrStdout(), raw)
			}
			return renderTieringPolicy(cmd.OutOrStdout(), policy)
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	return c
}

func newTieringPolicySetCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
		hotOnly      bool
		hotMaxAge    int
	)
	c := &cobra.Command{
		Use:   "set <database>",
		Short: "Create or replace a database's tiering policy (admin)",
		Long: `Create or replace a database's custom tiering policy.

--hot-only keeps the database out of future tier migrations. Set
--hot-max-age-days to override the global age threshold; omit it to use the
global default. The age must be zero or greater.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validObjectFormat(outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", outputFormat)
			}
			var age *int
			if cmd.Flags().Changed("hot-max-age-days") {
				age = &hotMaxAge
			}
			cli, _, err := f.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := f.ctx(cmd)
			defer cancel()
			mutation, err := cli.SetTieringPolicy(ctx, args[0], client.TieringPolicyRequest{HotOnly: hotOnly, HotMaxAgeDays: age})
			if err != nil {
				return err
			}
			if outputFormat == output.FormatJSON {
				return writeRawJSON(cmd.OutOrStdout(), mutation.Raw)
			}
			if mutation.Message != "" {
				fmt.Fprintln(cmd.OutOrStdout(), clean(mutation.Message))
			}
			if mutation.Policy != nil {
				if err := renderTieringPolicy(cmd.OutOrStdout(), mutation.Policy); err != nil {
					return err
				}
			}
			if len(mutation.ColdFiles) > 0 && string(mutation.ColdFiles) != "null" {
				fmt.Fprintf(cmd.OutOrStdout(), "cold files: %s\n", strings.TrimSpace(clean(string(mutation.ColdFiles))))
			}
			return nil
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	c.Flags().BoolVar(&hotOnly, "hot-only", false, "exclude this database from future tiering migrations")
	c.Flags().IntVar(&hotMaxAge, "hot-max-age-days", 0, "override the global hot-tier age threshold (non-negative)")
	return c
}

func newTieringPolicyDeleteCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
		yes          bool
	)
	c := &cobra.Command{
		Use:   "delete <database>",
		Short: "Delete a custom tiering policy (admin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validObjectFormat(outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", outputFormat)
			}
			if err := client.ValidateDatabaseName(args[0]); err != nil {
				return err
			}
			if err := confirmOrAbort(cmd, fmt.Sprintf("Delete the custom tiering policy for database %q?", clean(args[0])), yes); err != nil {
				return err
			}
			cli, _, err := f.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := f.ctx(cmd)
			defer cancel()
			mutation, err := cli.DeleteTieringPolicy(ctx, args[0])
			if err != nil {
				return err
			}
			if outputFormat == output.FormatJSON {
				return writeRawJSON(cmd.OutOrStdout(), mutation.Raw)
			}
			if mutation.Message != "" {
				_, err = fmt.Fprintln(cmd.OutOrStdout(), clean(mutation.Message))
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Deleted custom tiering policy for database %q\n", clean(args[0]))
			return err
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	c.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	return c
}

func newTieringPolicyEffectiveCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
	)
	c := &cobra.Command{
		Use:   "effective <database>",
		Short: "Show the effective tiering policy for a database",
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
			effective, raw, err := cli.EffectiveTieringPolicy(ctx, args[0])
			if err != nil {
				return err
			}
			if outputFormat == output.FormatJSON {
				return writeRawJSON(cmd.OutOrStdout(), raw)
			}
			return renderEffectiveTieringPolicy(cmd.OutOrStdout(), effective)
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	return c
}

func renderTieringStatus(w io.Writer, status *client.TieringStatus) error {
	rows := [][]string{
		{"enabled", strconv.FormatBool(status.Enabled)},
		{"licence valid", strconv.FormatBool(status.LicenseValid)},
		{"reason", valueOrDash(status.Reason)},
		{"quarantined files", strconv.FormatInt(status.QuarantinedFiles, 10)},
	}
	if status.Scheduler != nil {
		rows = append(rows,
			[]string{"scheduler running", strconv.FormatBool(status.Scheduler.Running)},
			[]string{"schedule", valueOrDash(status.Scheduler.Schedule)},
			[]string{"role gated", strconv.FormatBool(status.Scheduler.RoleGated)},
			[]string{"next run", fmtTieringTimePtr(status.Scheduler.NextRun, "-")},
			[]string{"last run", fmtTieringTimePtr(status.Scheduler.LastRun, "-")},
		)
	}
	if status.LastScanAt != nil {
		rows = append(rows, []string{"last scan at", fmtTieringTimePtr(status.LastScanAt, "-")})
	}
	if err := output.Table(w, []string{"FIELD", "VALUE"}, rows); err != nil {
		return err
	}
	if len(status.Tiers) > 0 {
		names := make([]string, 0, len(status.Tiers))
		for name := range status.Tiers {
			names = append(names, name)
		}
		sort.Strings(names)
		tierRows := make([][]string, 0, len(names))
		for _, name := range names {
			tier := status.Tiers[name]
			tierRows = append(tierRows, []string{clean(name), strconv.FormatBool(tier.Enabled), clean(tier.Backend), strconv.FormatInt(tier.FileCount, 10), strconv.FormatInt(tier.TotalSizeMB, 10)})
		}
		fmt.Fprintln(w)
		if err := output.Table(w, []string{"TIER", "ENABLED", "BACKEND", "FILES", "SIZE MB"}, tierRows); err != nil {
			return err
		}
	}
	if events := status.ReplicationEvents; events != nil {
		fmt.Fprintln(w)
		if err := output.Table(w, []string{"REPLICATION EVENTS", "COUNT"}, [][]string{
			{"applied", strconv.FormatInt(events.Applied, 10)},
			{"dropped", strconv.FormatInt(events.Dropped, 10)},
			{"failed", strconv.FormatInt(events.Failed, 10)},
		}); err != nil {
			return err
		}
	}
	if scan := status.LastScan; scan != nil {
		fmt.Fprintln(w)
		return renderTieringScan(w, scan)
	}
	return nil
}

func renderTieringFiles(w io.Writer, files []client.TieringFile, noHeader, asCSV bool) error {
	rows := make([][]string, 0, len(files))
	for _, f := range files {
		row := []string{
			f.Path, f.Database, f.Measurement, f.Tier,
			strconv.FormatInt(f.SizeBytes, 10), tieringTimeValue(f.PartitionTime),
			tieringTimeValue(f.CreatedAt), tieringTimePointer(f.MigratedAt),
			tieringTimePointer(f.QuarantinedAt), f.QuarantineReason,
		}
		if !asCSV {
			for i := range row {
				row[i] = clean(row[i])
			}
			row[5], row[6] = fmtTieringTime(f.PartitionTime, "-"), fmtTieringTime(f.CreatedAt, "-")
			row[7], row[8] = fmtTieringTimePtr(f.MigratedAt, "-"), fmtTieringTimePtr(f.QuarantinedAt, "-")
			row[9] = valueOrDash(f.QuarantineReason)
		}
		rows = append(rows, row)
	}
	headers := []string{"PATH", "DATABASE", "MEASUREMENT", "TIER", "SIZE BYTES", "PARTITION TIME", "CREATED AT", "MIGRATED AT", "QUARANTINED AT", "QUARANTINE REASON"}
	if asCSV {
		return writeTieringCSV(w, []string{"path", "database", "measurement", "tier", "size_bytes", "partition_time", "created_at", "migrated_at", "quarantined_at", "quarantine_reason"}, rows, noHeader)
	}
	if noHeader {
		headers = nil
	}
	return output.Table(w, headers, rows)
}

func renderTieringStats(w io.Writer, stats *client.TieringMigrationStats) error {
	if len(stats.TierStats) == 0 {
		fmt.Fprintln(w, "(no tier statistics)")
	} else {
		rows := make([][]string, 0, len(stats.TierStats))
		for _, tier := range stats.TierStats {
			rows = append(rows, []string{clean(tier.Tier), strconv.FormatBool(tier.Enabled), clean(tier.Backend), strconv.FormatInt(tier.FileCount, 10), strconv.FormatInt(tier.TotalSizeMB, 10)})
		}
		if err := output.Table(w, []string{"TIER", "ENABLED", "BACKEND", "FILES", "SIZE MB"}, rows); err != nil {
			return err
		}
	}
	fmt.Fprintln(w)
	if len(stats.RecentMigrations) == 0 {
		_, err := fmt.Fprintln(w, "(no recent migrations)")
		return err
	}
	rows := make([][]string, 0, len(stats.RecentMigrations))
	for _, m := range stats.RecentMigrations {
		rows = append(rows, []string{
			strconv.FormatInt(m.ID, 10), clean(m.Database), clean(m.FilePath), clean(m.FromTier), clean(m.ToTier),
			strconv.FormatInt(m.SizeBytes, 10), fmtTieringTime(m.StartedAt, "-"), fmtTieringTimePtr(m.CompletedAt, "-"), valueOrDash(m.Error),
		})
	}
	return output.Table(w, []string{"ID", "DATABASE", "FILE", "FROM", "TO", "SIZE BYTES", "STARTED AT", "COMPLETED AT", "ERROR"}, rows)
}

func renderTieringMigration(w io.Writer, result *client.TieringMigrationResult) error {
	rows := [][]string{{"status", valueOrDash(result.Status)}}
	if result.Message != "" {
		rows = append(rows, []string{"message", clean(result.Message)})
	}
	if result.WouldMigrate != nil {
		rows = append(rows, []string{"would migrate", strconv.Itoa(*result.WouldMigrate)})
	}
	if result.Migrated != nil {
		rows = append(rows, []string{"migrated", strconv.Itoa(*result.Migrated)})
	}
	if result.Errors != nil {
		rows = append(rows, []string{"errors", strconv.Itoa(*result.Errors)})
	}
	if result.TotalBytes != nil {
		rows = append(rows, []string{"total bytes", humanBytes(*result.TotalBytes)})
	}
	if result.FromTier != "" {
		rows = append(rows, []string{"from tier", clean(result.FromTier)})
	}
	if result.ToTier != "" {
		rows = append(rows, []string{"to tier", clean(result.ToTier)})
	}
	return output.Table(w, []string{"FIELD", "VALUE"}, rows)
}

func renderTieringScan(w io.Writer, result *client.TieringScanResult) error {
	return output.Table(w, []string{"FIELD", "VALUE"}, [][]string{
		{"files scanned", strconv.Itoa(result.FilesScanned)},
		{"files registered", strconv.Itoa(result.FilesRegistered)},
		{"files skipped", strconv.Itoa(result.FilesSkipped)},
		{"errors", strconv.Itoa(result.Errors)},
		{"cold files synced", strconv.Itoa(result.ColdSynced)},
		{"cold sync failed", strconv.FormatBool(result.ColdSyncFailed)},
		{"hot rows retired", strconv.Itoa(result.HotRetired)},
		{"hot rows written", strconv.Itoa(result.HotRowsWritten)},
		{"truncated", strconv.FormatBool(result.Truncated)},
	})
}

func renderTieringPolicyList(w io.Writer, policies []client.TieringPolicy, noHeader, asCSV bool) error {
	rows := make([][]string, 0, len(policies))
	for _, p := range policies {
		age := ""
		if p.HotMaxAgeDays != nil {
			age = strconv.Itoa(*p.HotMaxAgeDays)
		}
		row := []string{p.Database, strconv.FormatBool(p.HotOnly), age, tieringTimeValue(p.CreatedAt), tieringTimeValue(p.UpdatedAt)}
		if !asCSV {
			row[0] = clean(p.Database)
			row[3], row[4] = fmtTieringTime(p.CreatedAt, "-"), fmtTieringTime(p.UpdatedAt, "-")
		}
		rows = append(rows, row)
	}
	headers := []string{"DATABASE", "HOT ONLY", "HOT MAX AGE DAYS", "CREATED AT", "UPDATED AT"}
	if asCSV {
		return writeTieringCSV(w, []string{"database", "hot_only", "hot_max_age_days", "created_at", "updated_at"}, rows, noHeader)
	}
	if noHeader {
		headers = nil
	}
	return output.Table(w, headers, rows)
}

func renderTieringPolicy(w io.Writer, policy *client.TieringPolicy) error {
	age := "global default"
	if policy.HotMaxAgeDays != nil {
		age = strconv.Itoa(*policy.HotMaxAgeDays)
	}
	return output.Table(w, []string{"FIELD", "VALUE"}, [][]string{
		{"id", strconv.FormatInt(policy.ID, 10)},
		{"database", clean(policy.Database)},
		{"hot only", strconv.FormatBool(policy.HotOnly)},
		{"hot max age days", age},
		{"created at", fmtTieringTime(policy.CreatedAt, "-")},
		{"updated at", fmtTieringTime(policy.UpdatedAt, "-")},
	})
}

func renderEffectiveTieringPolicy(w io.Writer, policy *client.EffectiveTieringPolicy) error {
	return output.Table(w, []string{"FIELD", "VALUE"}, [][]string{
		{"database", clean(policy.Database)},
		{"hot only", strconv.FormatBool(policy.HotOnly)},
		{"hot max age days", strconv.Itoa(policy.HotMaxAgeDays)},
		{"source", clean(policy.Source)},
	})
}

func writeTieringCSV(w io.Writer, headers []string, rows [][]string, noHeader bool) error {
	cw := csv.NewWriter(w)
	if !noHeader {
		if err := cw.Write(headers); err != nil {
			return err
		}
	}
	for _, row := range rows {
		if err := cw.Write(row); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

func valueOrDash(s string) string {
	if s == "" {
		return "-"
	}
	return clean(s)
}

// tieringTimeValue normalizes timestamps without applying terminal sanitization
// or display placeholders to machine-readable CSV values.
func tieringTimeValue(s string) string {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC().Format(time.RFC3339Nano)
	}
	return s
}

func tieringTimePointer(p *string) string {
	if p == nil {
		return ""
	}
	return tieringTimeValue(*p)
}

func fmtTieringTime(s, none string) string {
	if s == "" {
		return none
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil && t.IsZero() {
		return none
	}
	return clean(tieringTimeValue(s))
}

func fmtTieringTimePtr(p *string, none string) string {
	if p == nil {
		return none
	}
	return fmtTieringTime(*p, none)
}
