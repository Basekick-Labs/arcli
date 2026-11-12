// backup subcommand: full backups and restores over /api/v1/backup/*.
//
// arcli creates full backups (every database) written to the server's
// backup.local_path; it shows and restores scoped ones made elsewhere. Create and restore are asynchronous on the server
// and share one slot; progress is published by the worker goroutine
// and stays visible (sticky) after completion, so the CLI snapshots the
// status before starting an operation and waits for it to change.
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

func newBackupCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "backup",
		Short: "Create, inspect, delete, and restore full backups (admin)",
		Long: `Create, inspect, delete, and restore full backups (/api/v1/backup/*).
Every subcommand requires an admin token.

A backup created by arcli contains every database plus, by default, the
metadata store (tokens, policies, continuous queries) and the server
config. It is written to the server's backup.local_path. Arc 27.01.1+
(currently unreleased) can also scope a backup to some databases and
route databases to remote targets configured on the server; arcli does
not create scoped backups, but shows and restores them. There is no
incremental mode. Backups are unavailable when the
server runs with backup.enabled=false, or when its backup manager fails
to initialize (the server log says why).`,
	}
	c.AddCommand(newBackupCreateCmd(), newBackupListCmd(), newBackupShowCmd(), newBackupStatusCmd(), newBackupDeleteCmd(), newBackupRestoreCmd())
	return c
}

// ---- status helpers --------------------------------------------------------

// opStart describes the operation a command just requested, so the
// status poll can tell "ours" from the sticky previous one and from an
// unrelated operation another client slipped in.
type opStart struct {
	operation   string    // "backup" or "restore"
	backupID    string    // restore only; "" for create (id unknown yet)
	prevStarted time.Time // start time of the operation the snapshot showed (zero if idle)
	prevID      string    // its backup id ("" if idle)
}

// snapshotOp builds the "what was there before" half of opStart.
func snapshotOp(before *client.BackupStatus) (time.Time, string) {
	if before.Idle {
		return time.Time{}, ""
	}
	return before.Progress.StartedAt, before.Progress.BackupID
}

// isOurs reports whether cur is the operation described by want: not
// idle, the right kind, the right id when known, and not the sticky
// record the snapshot already showed (same id and start time). A new
// operation always has a later start time than the previous one.
func isOurs(cur *client.BackupStatus, want opStart) bool {
	if cur.Idle {
		return false
	}
	p := cur.Progress
	if p.Operation != want.operation {
		return false
	}
	if want.backupID != "" && p.BackupID != want.backupID {
		return false
	}
	if p.BackupID == want.prevID && p.StartedAt.Equal(want.prevStarted) {
		return false // still the previous operation
	}
	return p.StartedAt.After(want.prevStarted)
}

// awaitOperationStart polls until the server publishes our operation
// (the worker goroutine does so after the 202). Bounded so a server
// that never publishes gives a clear message.
func awaitOperationStart(ctx context.Context, cli *client.Client, want opStart) (*client.BackupStatus, error) {
	for i := 0; i < 20; i++ {
		cur, err := cli.BackupStatus(ctx)
		if err != nil {
			return nil, err
		}
		if isOurs(cur, want) {
			return cur, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return nil, nil
}

// waitForOperation polls until the tracked operation leaves "running"
// and returns the final progress with the raw status body it came in.
func waitForOperation(cmd *cobra.Command, f *connFlags, cli *client.Client, expected *client.BackupProgress, waitTimeout time.Duration, onProgress func(*client.BackupProgress)) (*client.BackupProgress, []byte, error) {
	deadline := time.Now().Add(waitTimeout)
	interval := waitPollInterval
	for {
		ctx, cancel := f.ctx(cmd)
		st, err := cli.BackupStatus(ctx)
		cancel()
		if err != nil {
			return nil, nil, err
		}
		if st.Idle {
			return nil, nil, fmt.Errorf("the operation is no longer tracked by the server (restarted?); check `arcli backup list`")
		}
		p := st.Progress
		// Restoring the same backup again reuses its id, so the kind and
		// start time are also needed to identify the operation we started.
		if p.BackupID != expected.BackupID || p.Operation != expected.Operation || !p.StartedAt.Equal(expected.StartedAt) {
			return nil, nil, fmt.Errorf("the server is now tracking a different operation (%s %s); ours has finished — see `arcli backup show %s` or `arcli backup list`", clean(p.Operation), clean(p.BackupID), clean(expected.BackupID))
		}
		if onProgress != nil {
			onProgress(p)
		}
		if p.Status != "running" {
			return p, st.Raw, nil
		}
		if time.Now().After(deadline) {
			return nil, nil, fmt.Errorf("still running on the server after %s; check `arcli backup status`", waitTimeout)
		}
		select {
		case <-cmd.Context().Done():
			return nil, nil, cmd.Context().Err()
		case <-time.After(interval):
		}
		if interval < waitPollMax {
			interval += time.Second
		}
	}
}

func describeProgress(w io.Writer, p *client.BackupProgress, to timeOutput) {
	fmt.Fprintf(w, "operation:  %s\n", clean(p.Operation))
	fmt.Fprintf(w, "backup id:  %s\n", clean(p.BackupID))
	fmt.Fprintf(w, "status:     %s\n", clean(p.Status))
	fmt.Fprintf(w, "files:      %d / %d (%d skipped)\n", p.ProcessedFiles, p.TotalFiles, p.SkippedFiles)
	fmt.Fprintf(w, "bytes:      %s / %s\n", humanBytes(p.ProcessedBytes), humanBytes(p.TotalBytes))
	fmt.Fprintf(w, "started:    %s\n", to.Val(p.StartedAt, "-"))
	if p.CompletedAt != nil {
		fmt.Fprintf(w, "completed:  %s\n", to.Val(*p.CompletedAt, "-"))
	}
	if p.Error != "" {
		fmt.Fprintf(w, "error:      %s\n", clean(p.Error))
	}
	writeRestoreDetails(w, p)
	if p.CompactionPause != "" {
		fmt.Fprintf(w, "compaction: pause %s\n", clean(p.CompactionPause))
		if p.CompactionPause == "lost" {
			fmt.Fprintln(w, "  warning: restore lost its compaction pause; inspect the restore error and server log")
		}
	}
	// What the counts above do not say: which files, what the restored backup
	// already lacked, and warehouse files a restore had nowhere to put. The
	// server already names the counts of a failed restore in its error, so
	// only the names and the backup's own gaps are added here.
	writeRestoreGaps(w, p)
	writeSample(w, "skipped", p.SkippedSample)
	if p.UnaddressableFiles > 0 && len(p.UnaddressableSample) == 0 {
		// A backup publishes the count before copying but names the files
		// only in its manifest, which exists once it has completed; while it
		// runs, or after it failed, the names are in the server log.
		if p.Operation == "backup" && p.Status == "completed" {
			fmt.Fprintf(w, "  unaddressable: %d files (names: arcli backup show %s)\n", p.UnaddressableFiles, clean(p.BackupID))
		} else {
			fmt.Fprintf(w, "  unaddressable: %d files (names in the server log)\n", p.UnaddressableFiles)
		}
	}
	writeSample(w, "unaddressable", p.UnaddressableSample)
	if p.Operation == "backup" && p.ManifestOnlyFiles > 0 {
		fmt.Fprintf(w, "  manifest-only: %d files missing from the backup\n", p.ManifestOnlyFiles)
		writeSample(w, "manifest-only", p.ManifestOnlySample)
	}
}

// writeRestoreDetails prints the additional outcomes Arc reports for a
// restore. Older servers omit these fields, so an otherwise empty line is
// not emitted for them.
func writeRestoreDetails(w io.Writer, p *client.BackupProgress) {
	if p.Operation != "restore" {
		return
	}
	if p.Mode != "" && p.Mode != "merge" {
		fmt.Fprintf(w, "mode:       %s\n", clean(p.Mode))
	}
	if p.FilesRegistered > 0 || p.RegistrationFailed > 0 || p.SidecarMismatches > 0 || p.ReplacedFiles > 0 {
		fmt.Fprintf(w, "restore:    %d files registered, %d registration failures, %d sidecar mismatches, %d files replaced\n",
			p.FilesRegistered, p.RegistrationFailed, p.SidecarMismatches, p.ReplacedFiles)
	}
	writeSample(w, "registration-failed", p.RegistrationFailedSample)
	writeSample(w, "sidecar-mismatch", p.SidecarMismatchSample)
}

// writeRestoreGaps prints what a restore's progress says about gaps that are
// not the restore's own doing: what the restored backup already lacked when it
// was taken, and warehouse files this node had nowhere to put. Printed for a
// failed and a completed restore alike, since the latter is the only place the
// warehouse gap shows when Iceberg is off on the node.
func writeRestoreGaps(w io.Writer, p *client.BackupProgress) {
	backupGaps := incompleteCounts{
		Skipped:       p.BackupSkippedFiles,
		Unaddressable: p.BackupUnaddressableFiles,
		ManifestOnly:  p.BackupManifestOnlyFiles,
	}
	if gaps := describeIncomplete(backupGaps); gaps != "" {
		fmt.Fprintf(w, "backup had: %s when it was taken\n", gaps)
	}
	if p.BackupUnregisteredSkipped > 0 {
		fmt.Fprintf(w, "backup info: %d files were not listed in the cluster manifest when it was taken\n", p.BackupUnregisteredSkipped)
	}
	if p.IcebergWarehouseFilesSkipped > 0 {
		fmt.Fprintf(w, "iceberg:    %d warehouse files not restored (this node has no outside-root warehouse)\n", p.IcebergWarehouseFilesSkipped)
	}
}

// overlongClause phrases how many skips were for a destination key over the
// storage limit, the permanent cause fixed by renaming the file.
func overlongClause(n int64) string {
	if n == 1 {
		return "1 for a key too long to store"
	}
	return fmt.Sprintf("%d for keys too long to store", n)
}

// wasWere is the verb for n skipped files.
func wasWere(n int64) string {
	if n == 1 {
		return "was"
	}
	return "were"
}

type incompleteCounts struct {
	Skipped           int64
	SkippedReconciled int64
	Metadata          int64
	Unaddressable     int64
	ManifestOnly      int64
}

func effectiveSkipped(skipped, reconciled int64) int64 {
	if reconciled >= skipped {
		return 0
	}
	return skipped - reconciled
}

// describeIncomplete keeps the server's distinct missing-file populations
// separate. Skips later reconciled against the cluster manifest are not
// missing data, and manifest-only files are a separate source of incompleteness.
func describeIncomplete(c incompleteCounts) string {
	var parts []string
	if skipped := effectiveSkipped(c.Skipped, c.SkippedReconciled); skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped", skipped))
	}
	if c.Metadata > 0 {
		parts = append(parts, fmt.Sprintf("%d metadata", c.Metadata))
	}
	if c.Unaddressable > 0 {
		parts = append(parts, fmt.Sprintf("%d unaddressable", c.Unaddressable))
	}
	if c.ManifestOnly > 0 {
		parts = append(parts, fmt.Sprintf("%d manifest-only", c.ManifestOnly))
	}
	return strings.Join(parts, ", ")
}

func backupListIncomplete(b client.BackupSummary) string {
	// Current Arc listings include skipped_reconciled; older servers omit
	// it (zero). Match the manifest's calculation without counting informational exclusions.
	if incomplete := describeIncomplete(incompleteCounts{
		Skipped:           b.SkippedFiles,
		SkippedReconciled: b.SkippedReconciled,
		Metadata:          b.SkippedMetadataFiles,
		Unaddressable:     b.UnaddressableFiles,
		ManifestOnly:      b.ManifestOnlyFiles,
	}); incomplete != "" {
		return incomplete
	}
	return "-"
}

// writeSample prints one labelled line per path (up to the server's 32).
func writeSample(w io.Writer, label string, paths []string) {
	for _, p := range paths {
		fmt.Fprintf(w, "  %-14s %s\n", label+":", clean(p))
	}
}

// plural returns n with the singular or plural noun.
func plural(n int64, singular, pluralForm string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	return fmt.Sprintf("%d %s", n, pluralForm)
}

// ---- create ----------------------------------------------------------------

func newBackupCreateCmd() *cobra.Command {
	var (
		f                    connFlags
		to                   timeOutput
		outputFormat         string
		noMetadata, noConfig bool
		wait                 bool
		waitTimeout          time.Duration
	)
	c := &cobra.Command{
		Use:   "create",
		Short: "Start a full backup of every database",
		Long: `Start a full backup (POST /api/v1/backup/). The server runs it in the
background; arcli prints the backup id as soon as the server publishes
it and, with --wait, polls until it finishes (--wait-timeout defaults
to 2h, the server's own budget). Only one backup, restore, or backup
deletion can run at a time.

On a cluster, only the primary writer can create a backup. Other
nodes receive HTTP 503 before the operation starts.`,
		Example: `  arcli backup create
  arcli backup create --wait
  arcli backup create --no-config -o json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validObjectFormat(outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", outputFormat)
			}
			if waitTimeout <= 0 {
				return fmt.Errorf("--wait-timeout must be > 0")
			}
			cli, _, err := f.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := f.ctx(cmd)
			before, err := cli.BackupStatus(ctx)
			cancel()
			if err != nil {
				return err
			}
			prevStarted, prevID := snapshotOp(before)
			want := opStart{operation: "backup", prevStarted: prevStarted, prevID: prevID}
			var opts client.BackupCreateOptions
			if noMetadata {
				v := false
				opts.IncludeMetadata = &v
			}
			if noConfig {
				v := false
				opts.IncludeConfig = &v
			}
			ctx, cancel = f.ctx(cmd)
			err = cli.CreateBackup(ctx, opts)
			cancel()
			if err != nil {
				return err
			}
			sctx, scancel := f.ctx(cmd)
			started, err := awaitOperationStart(sctx, cli, want)
			scancel()
			if err != nil {
				return err
			}
			stderr := cmd.ErrOrStderr()
			if started == nil {
				fmt.Fprintln(stderr, "Backup started, but the server has not published its progress yet; check `arcli backup status`.")
				if outputFormat == output.FormatJSON {
					return writeJSON(cmd.OutOrStdout(), map[string]string{"status": "accepted", "backup_id": ""})
				}
				return nil
			}
			id := started.Progress.BackupID
			if !wait {
				if outputFormat == output.FormatJSON {
					return writeRawJSON(cmd.OutOrStdout(), started.Raw)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Backup %s started\n", clean(id))
				fmt.Fprintln(stderr, "It runs in the background; follow it with `arcli backup status` or re-run with --wait.")
				return nil
			}
			fmt.Fprintf(stderr, "Backup %s started; waiting...\n", clean(id))
			final, raw, err := waitForOperation(cmd, &f, cli, started.Progress, waitTimeout, nil)
			if err != nil {
				return err
			}
			if outputFormat == output.FormatJSON {
				if err := writeRawJSON(cmd.OutOrStdout(), raw); err != nil {
					return err
				}
				if final.Status != "completed" {
					return fmt.Errorf("backup %s %s: %s", clean(id), clean(final.Status), clean(final.Error))
				}
				return nil
			}
			if final.Status != "completed" {
				describeProgress(cmd.OutOrStdout(), final, to)
				return fmt.Errorf("backup %s %s: %s", clean(id), clean(final.Status), clean(final.Error))
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Backup %s completed: %d files, %s", clean(id), final.ProcessedFiles, humanBytes(final.ProcessedBytes))
			var gaps []string
			if final.SkippedFiles > 0 {
				gaps = append(gaps, fmt.Sprintf("%d files skipped", final.SkippedFiles))
			}
			if final.UnaddressableFiles > 0 {
				gaps = append(gaps, fmt.Sprintf("%d files could not be listed", final.UnaddressableFiles))
			}
			if final.ManifestOnlyFiles > 0 {
				gaps = append(gaps, fmt.Sprintf("%d files were present only in the cluster manifest", final.ManifestOnlyFiles))
			}
			if len(gaps) > 0 {
				fmt.Fprintf(out, " (%s — the backup is incomplete)", strings.Join(gaps, ", "))
			}
			fmt.Fprintln(out)
			if len(gaps) > 0 {
				writeSample(out, "skipped", final.SkippedSample)
				writeSample(out, "manifest-only", final.ManifestOnlySample)
				fmt.Fprintf(cmd.ErrOrStderr(), "see `arcli backup show %s` for the full breakdown\n", clean(id))
			}
			return nil
		},
	}
	f.add(c)
	to.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	c.Flags().BoolVar(&noMetadata, "no-metadata", false, "exclude the metadata store (tokens, policies, continuous queries)")
	c.Flags().BoolVar(&noConfig, "no-config", false, "exclude the server config file")
	c.Flags().BoolVar(&wait, "wait", false, "poll until the backup has finished")
	c.Flags().DurationVar(&waitTimeout, "wait-timeout", 2*time.Hour, "give up waiting after this long (with --wait)")
	return c
}

// ---- list / show / status --------------------------------------------------

func newBackupListCmd() *cobra.Command {
	var (
		f            connFlags
		to           timeOutput
		outputFormat string
		noHeader     bool
	)
	c := &cobra.Command{
		Use:   "list",
		Short: "List backups, newest first",
		Long: `List backups (GET /api/v1/backup/), newest first.

INCOMPLETE includes unreconciled skipped data files, manifest-only files,
metadata skips, and unaddressable files. Reconciled skips (files that had
left the cluster manifest by the end of the run), unregistered local files,
and files removed from the manifest during backup are not missing backup
data: they are not counted here, and appear as CSV columns and in
"backup show".
"-" means no gaps were reported; "backup show" names sampled files.

Output formats: table (default) | json | csv. The csv output includes the
incompleteness and cluster-manifest counts returned by the server.`,
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
			list, raw, err := cli.ListBackups(ctx)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if outputFormat == output.FormatJSON {
				return writeRawJSON(w, raw)
			}
			sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.After(list[j].CreatedAt) })
			if outputFormat == output.FormatCSV {
				cw := csv.NewWriter(w)
				if !noHeader {
					if err := cw.Write([]string{"backup_id", "created_at", "backup_type", "database_count", "total_files", "total_size_bytes", "skipped_files", "skipped_metadata_files", "unaddressable_files", "unregistered_skipped", "manifest_only_files", "left_manifest_during_run", "skipped_reconciled"}); err != nil {
						return err
					}
				}
				for _, b := range list {
					if err := cw.Write([]string{b.BackupID, to.Val(b.CreatedAt, ""), b.BackupType, strconv.Itoa(b.DatabaseCount), strconv.FormatInt(b.TotalFiles, 10), strconv.FormatInt(b.TotalBytes, 10), strconv.FormatInt(b.SkippedFiles, 10), strconv.FormatInt(b.SkippedMetadataFiles, 10), strconv.FormatInt(b.UnaddressableFiles, 10), strconv.FormatInt(b.UnregisteredSkipped, 10), strconv.FormatInt(b.ManifestOnlyFiles, 10), strconv.FormatInt(b.LeftManifestDuringRun, 10), strconv.FormatInt(b.SkippedReconciled, 10)}); err != nil {
						return err
					}
				}
				cw.Flush()
				return cw.Error()
			}
			if len(list) == 0 {
				_, err := fmt.Fprintln(w, "(no backups)")
				return err
			}
			rows := make([][]string, 0, len(list))
			for _, b := range list {
				rows = append(rows, []string{clean(b.BackupID), to.Val(b.CreatedAt, "-"), clean(b.BackupType), strconv.Itoa(b.DatabaseCount), strconv.FormatInt(b.TotalFiles, 10), humanBytes(b.TotalBytes), backupListIncomplete(b)})
			}
			headers := []string{"ID", "CREATED", "TYPE", "DATABASES", "FILES", "SIZE", "INCOMPLETE"}
			if noHeader {
				headers = nil
			}
			return output.Table(w, headers, rows)
		},
	}
	f.add(c)
	to.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json|csv")
	c.Flags().BoolVar(&noHeader, "no-header", false, "suppress column header row (table + csv)")
	return c
}

func newBackupShowCmd() *cobra.Command {
	var (
		f            connFlags
		to           timeOutput
		outputFormat string
	)
	c := &cobra.Command{
		Use:   "show <id>",
		Short: "Show a backup's manifest: databases, measurements, sizes",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validObjectFormat(outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", outputFormat)
			}
			if err := client.ValidateBackupID(args[0]); err != nil {
				return err
			}
			cli, _, err := f.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := f.ctx(cmd)
			defer cancel()
			m, err := cli.GetBackup(ctx, args[0])
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if outputFormat == output.FormatJSON {
				return writeRawJSON(w, m.Raw)
			}
			return writeManifest(w, m, to)
		},
	}
	f.add(c)
	to.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	return c
}

func writeManifest(w io.Writer, m *client.BackupManifest, to timeOutput) error {
	fmt.Fprintf(w, "backup id:   %s\n", clean(m.BackupID))
	fmt.Fprintf(w, "created:     %s\n", to.Val(m.CreatedAt, "-"))
	fmt.Fprintf(w, "type:        %s (server %s)\n", clean(m.BackupType), clean(m.Version))
	if len(m.Scope) > 0 {
		scope := make([]string, 0, len(m.Scope))
		for _, db := range m.Scope {
			scope = append(scope, clean(db))
		}
		fmt.Fprintf(w, "scope:       %s\n", strings.Join(scope, ", "))
	}
	fmt.Fprintf(w, "contents:    %d files, %s, metadata %t, config %t\n", m.TotalFiles, humanBytes(m.TotalSizeBytes), m.HasMetadata, m.HasConfig)
	if line := describeManifestIncomplete(m); line != "" {
		fmt.Fprintf(w, "INCOMPLETE:  %s\n", line)
	}
	writeSample(w, "skipped", m.SkippedSample)
	writeSample(w, "unaddressable", m.UnaddressableSample)
	writeManifestClusterInfo(w, m)
	writeSample(w, "unregistered", m.UnregisteredSample)
	writeSample(w, "manifest-only", m.ManifestOnlySample)
	writeSample(w, "left-manifest", m.LeftManifestSample)
	if len(m.Databases) == 0 {
		_, err := fmt.Fprintln(w, "(no databases)")
		return err
	}
	fmt.Fprintln(w)
	rows := [][]string{}
	for _, db := range m.Databases {
		for _, ms := range db.Measurements {
			rows = append(rows, []string{clean(db.Name), clean(ms.Name), strconv.Itoa(ms.FileCount), humanBytes(ms.SizeBytes)})
		}
		if len(db.Measurements) == 0 {
			rows = append(rows, []string{clean(db.Name), "-", strconv.Itoa(db.FileCount), humanBytes(db.SizeBytes)})
		}
	}
	return output.Table(w, []string{"DATABASE", "MEASUREMENT", "FILES", "SIZE"}, rows)
}

func writeManifestClusterInfo(w io.Writer, m *client.BackupManifest) {
	if m.SkippedReconciled > 0 {
		fmt.Fprintf(w, "reconciled:       %s later absent from the manifest; not incomplete\n", plural(m.SkippedReconciled, "skipped file was", "skipped files were"))
	}
	if m.UnregisteredSkipped > 0 {
		fmt.Fprintf(w, "unregistered:     %d local data files were not in the cluster manifest and were not backed up\n", m.UnregisteredSkipped)
	}
	if m.LeftManifestDuringRun > 0 {
		fmt.Fprintf(w, "left manifest:    %d files were copied then removed as the manifest changed during backup\n", m.LeftManifestDuringRun)
	}
	if m.ClusterManifestChecked != nil && *m.ClusterManifestChecked {
		fmt.Fprintln(w, "cluster manifest: checked")
	}
}

// describeManifestIncomplete phrases a manifest's incompleteness, or returns
// "" for a backup that reports none. The skipped clause counts data files
// against the backup's file total and metadata files apart (they are not in
// that total); the overlong clause spans both, as the server counts it.
func describeManifestIncomplete(m *client.BackupManifest) string {
	var clauses []string
	var skipped []string
	missingSkipped := effectiveSkipped(m.SkippedFiles, m.SkippedReconciled)
	if missingSkipped > 0 {
		// A manifest written before the data/metadata split folded metadata
		// skips into skipped_files, so the count can exceed the total; then
		// "N of M" would read as nonsense.
		if missingSkipped > m.TotalFiles {
			skipped = append(skipped, plural(missingSkipped, "file", "files"))
		} else {
			skipped = append(skipped, fmt.Sprintf("%d of %d files", missingSkipped, m.TotalFiles))
		}
	}
	if m.SkippedMetadataFiles > 0 {
		skipped = append(skipped, plural(m.SkippedMetadataFiles, "metadata file", "metadata files"))
	}
	if len(skipped) > 0 {
		clause := strings.Join(skipped, " and ") + " " + wasWere(missingSkipped+m.SkippedMetadataFiles) + " skipped while backing up"
		if m.SkippedOverlongKeys > 0 {
			clause += " (" + overlongClause(m.SkippedOverlongKeys) + ")"
		}
		clauses = append(clauses, clause)
	}
	if m.UnaddressableFiles > 0 {
		clauses = append(clauses, plural(m.UnaddressableFiles, "file", "files")+" could not be listed (unaddressable)")
	}
	if m.ManifestOnlyFiles > 0 {
		clauses = append(clauses, plural(m.ManifestOnlyFiles, "file was", "files were")+" present only in the cluster manifest")
	}
	if m.IcebergWarehouse != nil && m.IcebergWarehouse.SkippedFiles > 0 {
		clauses = append(clauses, plural(m.IcebergWarehouse.SkippedFiles, "Iceberg warehouse file was", "Iceberg warehouse files were")+" skipped")
	}
	return strings.Join(clauses, "; ")
}

// describeRestoreGaps phrases what a backup lacked when it was taken, for the
// warning before a restore: the same shape as describeManifestIncomplete, with
// the overlong clause on the joined skipped sentence because the server counts
// overlong keys across data and metadata files alike. "" when the manifest
// records no gap.
func describeRestoreGaps(m *client.BackupManifest) string {
	var clauses []string
	var skipped []string
	missingSkipped := effectiveSkipped(m.SkippedFiles, m.SkippedReconciled)
	if missingSkipped > 0 {
		skipped = append(skipped, plural(missingSkipped, "file", "files"))
	}
	if m.SkippedMetadataFiles > 0 {
		skipped = append(skipped, plural(m.SkippedMetadataFiles, "metadata file", "metadata files"))
	}
	if len(skipped) > 0 {
		clause := strings.Join(skipped, " and ") + " " + wasWere(missingSkipped+m.SkippedMetadataFiles) + " skipped when it was taken"
		if m.SkippedOverlongKeys > 0 {
			clause += ", " + strings.Replace(overlongClause(m.SkippedOverlongKeys), " for ", " of them for ", 1)
		}
		clauses = append(clauses, clause)
	}
	if m.UnaddressableFiles > 0 {
		clauses = append(clauses, plural(m.UnaddressableFiles, "file", "files")+" could not be listed")
	}
	if m.ManifestOnlyFiles > 0 {
		clauses = append(clauses, plural(m.ManifestOnlyFiles, "file was", "files were")+" present only in the cluster manifest")
	}
	if m.IcebergWarehouse != nil && m.IcebergWarehouse.SkippedFiles > 0 {
		clauses = append(clauses, plural(m.IcebergWarehouse.SkippedFiles, "Iceberg warehouse file was", "Iceberg warehouse files were")+" skipped")
	}
	return strings.Join(clauses, "; ")
}

func newBackupStatusCmd() *cobra.Command {
	var (
		f            connFlags
		to           timeOutput
		outputFormat string
	)
	c := &cobra.Command{
		Use:   "status",
		Short: "Show the running or most recent backup/restore operation",
		Long: `Show the running or most recent backup/restore operation
(GET /api/v1/backup/status). The server keeps the last operation's
result until the next one starts, or reports "idle" when nothing has run
since it started.`,
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
			st, err := cli.BackupStatus(ctx)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if outputFormat == output.FormatJSON {
				return writeRawJSON(w, st.Raw)
			}
			if st.Idle {
				_, err := fmt.Fprintln(w, "idle (no backup or restore has run since the server started)")
				return err
			}
			progress := st.Progress
			var unreconciled int64
			if progress.Operation == "restore" && progress.BackupSkippedFiles > 0 {
				// Status does not include skipped_reconciled. Enrich human output
				// from the source manifest without changing raw JSON or making a
				// missing/deleted manifest prevent inspection of the operation.
				display := *progress
				mctx, mcancel := f.ctx(cmd)
				manifest, manifestErr := cli.GetBackup(mctx, progress.BackupID)
				mcancel()
				if manifestErr == nil {
					display.BackupSkippedFiles = effectiveSkipped(manifest.SkippedFiles, manifest.SkippedReconciled)
				} else {
					unreconciled = progress.BackupSkippedFiles
					display.BackupSkippedFiles = 0
				}
				progress = &display
			}
			describeProgress(w, progress, to)
			if unreconciled > 0 {
				fmt.Fprintf(w, "backup skipped: %d reported before reconciliation; manifest unavailable, see `arcli backup show %s`\n", unreconciled, clean(progress.BackupID))
			}
			return nil
		},
	}
	f.add(c)
	to.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	return c
}

// ---- delete ----------------------------------------------------------------

func newBackupDeleteCmd() *cobra.Command {
	var (
		f   connFlags
		to  timeOutput
		yes bool
	)
	c := &cobra.Command{
		Use:   "delete <id>",
		Short: "Delete a backup from the server's backup directory",
		Long: `Delete a backup from the server's backup directory.

On a cluster, only the primary writer can delete a backup. A node
that is not the primary writer returns HTTP 503 after confirmation;
the backup is not deleted.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			if err := client.ValidateBackupID(id); err != nil {
				return err
			}
			cli, _, err := f.client(cmd)
			if err != nil {
				return err
			}
			// The server answers 500 for an unknown id; look first.
			ctx, cancel := f.ctx(cmd)
			m, err := cli.GetBackup(ctx, id)
			cancel()
			if err != nil {
				return err
			}
			q := fmt.Sprintf("Delete backup %s (%s, %d files, %s)?", clean(id), to.Val(m.CreatedAt, "-"), m.TotalFiles, humanBytes(m.TotalSizeBytes))
			if err := confirmOrAbort(cmd, q, yes); err != nil {
				return err
			}
			ctx, cancel = f.ctx(cmd)
			defer cancel()
			err = cli.DeleteBackup(ctx, id)
			var he *client.HTTPError
			if errors.As(err, &he) && he.Status == 500 {
				// Distinguish "vanished meanwhile" from a real failure.
				gctx, gcancel := f.ctx(cmd)
				_, gerr := cli.GetBackup(gctx, id)
				gcancel()
				var ghe *client.HTTPError
				if errors.As(gerr, &ghe) && ghe.Status == 404 {
					return fmt.Errorf("backup %s no longer exists (deleted concurrently)", clean(id))
				}
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted backup %s\n", clean(id))
			return nil
		},
	}
	f.add(c)
	to.add(c)
	c.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	return c
}

// ---- restore ---------------------------------------------------------------

// restoreModeConfirmed reports whether the mode Arc echoed confirms the one
// requested. Arc before 27.01.1 echoes no mode and always merges, so an
// unechoed merge is exactly what was asked for; only replace needs the echo.
// replaceRefusedGaps reports the manifest gaps Arc refuses a replace for
// (restore.go replaceDatabases). Arc also refuses on files missing from the
// backup storage and on a high unregistered-skip ratio; neither is in the
// manifest arcli reads, so those refusals arrive only after the 202.
func replaceRefusedGaps(m *client.BackupManifest) bool {
	return effectiveSkipped(m.SkippedFiles, m.SkippedReconciled) > 0 || m.UnaddressableFiles > 0 || m.ManifestOnlyFiles > 0
}

func restoreModeConfirmed(requested, reported string) bool {
	return requested == "" || reported == requested || (requested == "merge" && reported == "")
}

func newBackupRestoreCmd() *cobra.Command {
	var (
		f            connFlags
		to           timeOutput
		outputFormat string
		modeFlag     string
		dataOnly     bool
		withMetadata bool
		withConfig   bool
		yes, wait    bool
		waitTimeout  time.Duration
	)
	c := &cobra.Command{
		Use:   "restore <id>",
		Short: "Restore a backup over the server's storage (destructive)",
		Long: `Restore a backup (POST /api/v1/backup/restore).

Data files from the backup are written over the server's storage
unconditionally; existing files at the same paths are replaced. Run it
on a quiescent server: stop writers first. Arc 27.01.1+ (currently unreleased)
pauses compaction cluster-wide and registers restored files in the cluster
manifest. On older clusters, including 26.06–26.09, stop compaction yourself;
restored files are not registered in the cluster manifest. Stop compaction
manually on standalone servers too.

By default the metadata store (tokens, policies, continuous queries) is
restored on a standalone node if the backup contains it, but skipped on a
cluster. If restored, it is only STAGED: the server applies it at its next
start. Use --with-metadata to request it explicitly; clusters refuse that
request. --data-only explicitly skips metadata and config. --with-config
also restores the server config file (staged as well). The restore runs in
the background; --wait polls until it finishes and reports compaction-pause
observed state changes on a cluster; polling may miss intermediate states
(--wait-timeout defaults to 2h).

The restore mode is merge by default for a full backup. On a cluster,
Arc 27.01.1+ (currently unreleased) defaults a scoped backup to replace;
when --mode is omitted, arcli leaves that choice to the server and the
prompt names the backup's scope. Use --mode merge or --mode replace to
choose explicitly; --mode is case-insensitive, surrounding spaces are
ignored, and an empty value is the same as omitting it. Explicit restore
modes require Arc 27.01.1+. Replace first removes the current hot-tier
files of each restored database from every node, including data written
after the backup was taken, then restores the backup's files; cold-tier
objects are not removed. Arc refuses replace on standalone servers and on
any cluster node without the cluster file manifest (cluster.raft_data_dir).
Older servers silently ignore --mode and merge, which is what --mode merge
asks for. When Arc does not confirm --mode replace, arcli warns and exits
nonzero, but the accepted restore may still run server-side as a merge:
check backup status before retrying. A backup whose manifest records data
gaps is refused for --mode replace before the prompt. When an omitted mode
resolves to replace, Arc refuses an incomplete backup only after accepting
the restore, so without --wait arcli reports it as started; use --wait or
--mode merge.

On a cluster, only the primary writer can restore a backup. A node
that is not the primary writer returns HTTP 503 after confirmation,
before the restore starts.`,
		Example: `  arcli backup restore backup-20260907-201105-a0f5e600 --data-only --wait
  arcli backup restore backup-20260907-201105-a0f5e600 --yes`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validObjectFormat(outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", outputFormat)
			}
			if waitTimeout <= 0 {
				return fmt.Errorf("--wait-timeout must be > 0")
			}
			// Normalised the way Arc's NormalizeRestoreMode does; an empty
			// result means the flag was not set, so nothing is sent and Arc
			// picks the mode. Every later use reads the normalised value.
			mode := strings.ToLower(strings.TrimSpace(modeFlag))
			if mode != "" && mode != "merge" && mode != "replace" {
				return fmt.Errorf("invalid --mode %q (valid: merge, replace)", modeFlag)
			}
			if dataOnly && withConfig {
				return fmt.Errorf("--data-only and --with-config are mutually exclusive")
			}
			if dataOnly && withMetadata {
				return fmt.Errorf("--data-only and --with-metadata are mutually exclusive")
			}
			id := args[0]
			if err := client.ValidateBackupID(id); err != nil {
				return err
			}
			cli, _, err := f.client(cmd)
			if err != nil {
				return err
			}
			// The server 202s a missing id and fails only in status.
			ctx, cancel := f.ctx(cmd)
			m, err := cli.GetBackup(ctx, id)
			cancel()
			if err != nil {
				return err
			}
			restoreMeta := withMetadata && m.HasMetadata
			metadataMayRestoreByDefault := !dataOnly && !withMetadata && m.HasMetadata
			restoreCfg := withConfig
			if withMetadata && !m.HasMetadata {
				return fmt.Errorf("backup %s does not contain the metadata store; drop --with-metadata", clean(id))
			}
			if withConfig && !m.HasConfig {
				return fmt.Errorf("backup %s does not contain the server config; drop --with-config", clean(id))
			}
			stderr := cmd.ErrOrStderr()
			// The same gaps Arc refuses a replace for (restore.go replaceDatabases),
			// checked before the prompt instead of after a 202.
			if mode == "replace" && replaceRefusedGaps(m) {
				return fmt.Errorf("restore refused: --mode replace requires a complete backup, and backup %s is incomplete (%s); use --mode merge or take a complete backup first", clean(id), describeRestoreGaps(m))
			}
			if gaps := describeRestoreGaps(m); gaps != "" {
				fmt.Fprintf(stderr, "warning: backup %s is incomplete (%s)\n", clean(id), gaps)
			}
			dbs := make([]string, 0, len(m.Databases))
			for _, db := range m.Databases {
				dbs = append(dbs, clean(db.Name))
			}
			scope := "the live storage"
			if len(dbs) > 0 {
				scope = "the live storage of database(s) " + strings.Join(dbs, ", ")
			}
			// Arc selects the databases a replace removes by the backup's
			// scope (path segment) when it has one, else by database name.
			scoped := make([]string, 0, len(m.Scope))
			for _, db := range m.Scope {
				scoped = append(scoped, clean(db))
			}
			replaced := dbs
			if len(scoped) > 0 {
				replaced = scoped
			}
			replacedNames := "these databases"
			if len(replaced) > 0 {
				replacedNames = "database(s) " + strings.Join(replaced, ", ")
			}
			parts := []string{fmt.Sprintf("Restore backup %s (%s, %d files, %s) over %s? Existing files at the same paths are replaced.", clean(id), to.Val(m.CreatedAt, "-"), m.TotalFiles, humanBytes(m.TotalSizeBytes), scope)}
			if mode == "replace" {
				parts = append(parts, "On a cluster running Arc 27.01.1+, replace first removes the current hot-tier files of "+replacedNames+" on every node, including data written after this backup was taken, then restores the backup's files. Standalone servers refuse replace. Older Arc versions ignore --mode and merge instead.")
			}
			// Arc 27.01.1+ resolves an omitted mode to replace for a scoped
			// backup on a cluster (ResolveRestoreMode). Older manifests have
			// no scope, so the prompt for them is unchanged.
			if mode == "" && len(scoped) > 0 {
				parts = append(parts, "This backup is scoped to database(s) "+strings.Join(scoped, ", ")+". On a cluster running Arc 27.01.1+, a restore without --mode runs as replace: it first removes the current hot-tier files of these databases on every node, including data written after this backup was taken. Pass --mode merge to keep the current files.")
				if replaceRefusedGaps(m) {
					parts = append(parts, "This backup is incomplete, so on a cluster Arc refuses that replace, but only after accepting the restore and pausing compaction: without --wait arcli reports it as started and exits 0. Pass --mode merge, or --wait to see the outcome.")
				} else if !wait {
					// Files missing from the backup storage are found only by
					// the restore itself, after the 202.
					parts = append(parts, "Arc checks the backup's files only after accepting the restore, so without --wait a refused replace is reported as started; use --wait to see the outcome.")
				}
			}
			if restoreMeta {
				parts = append(parts, "The metadata store will be staged and applied at the next server start.")
			} else if metadataMayRestoreByDefault {
				parts = append(parts, "Metadata restoration follows the server default: enabled on standalone nodes and disabled on clusters. If restored, it is staged until the next server start; use --with-metadata to request it explicitly.")
			}
			if restoreCfg {
				parts = append(parts, "The server config will be staged as well.")
			}
			parts = append(parts, "Stop writers and compaction first.")
			if err := confirmOrAbort(cmd, strings.Join(parts, " "), yes); err != nil {
				return err
			}

			ctx, cancel = f.ctx(cmd)
			before, err := cli.BackupStatus(ctx)
			cancel()
			if err != nil {
				return err
			}
			prevStarted, prevID := snapshotOp(before)
			want := opStart{operation: "restore", backupID: id, prevStarted: prevStarted, prevID: prevID}
			opts := client.RestoreOptions{}
			t, fls := true, false
			opts.RestoreData = &t
			if dataOnly {
				opts.RestoreMetadata = &fls
				opts.RestoreConfig = &fls
			} else if restoreMeta {
				opts.RestoreMetadata = &t
			}
			if restoreCfg {
				opts.RestoreConfig = &t
			}
			if mode != "" {
				opts.Mode = &mode
			}
			ctx, cancel = f.ctx(cmd)
			res, err := cli.RestoreBackup(ctx, id, opts)
			cancel()
			if err != nil {
				return err
			}
			if !restoreModeConfirmed(mode, res.Mode) {
				fmt.Fprintln(stderr, "warning: Arc accepted the restore without confirming the requested mode; it may be running with different semantics. Check `arcli backup status` before retrying.")
				if outputFormat == output.FormatJSON {
					if err := writeRawJSON(cmd.OutOrStdout(), res.Raw); err != nil {
						return err
					}
				}
				return fmt.Errorf("requested restore mode %q was not confirmed by Arc (reported %q); explicit modes require Arc 27.01.1+", mode, clean(res.Mode))
			}
			sctx, scancel := f.ctx(cmd)
			started, err := awaitOperationStart(sctx, cli, want)
			scancel()
			if err != nil {
				return err
			}
			restartNote := func() {
				if res.RestartRequired || res.Staged {
					fmt.Fprintln(stderr, "The server reports that restored metadata or config requires a restart. Restart the server after the restore completes.")
				}
			}
			if started == nil {
				fmt.Fprintln(stderr, "Restore accepted, but the server has not published its progress yet; check `arcli backup status`.")
				if outputFormat == output.FormatJSON {
					if err := writeRawJSON(cmd.OutOrStdout(), res.Raw); err != nil {
						return err
					}
				} else if res.Mode == "replace" {
					fmt.Fprintf(cmd.OutOrStdout(), "Restore of %s accepted (mode: %s)\n", clean(id), clean(res.Mode))
				}
				restartNote()
				return nil
			}
			if !wait {
				if outputFormat == output.FormatJSON {
					if err := writeRawJSON(cmd.OutOrStdout(), res.Raw); err != nil {
						return err
					}
				} else {
					// Only a mode the server echoed: an older Arc ignores the
					// field and merges. A merge prints the published line.
					startedMode := res.Mode
					if startedMode == "" || startedMode == "merge" {
						fmt.Fprintf(cmd.OutOrStdout(), "Restore of %s started\n", clean(id))
					} else {
						fmt.Fprintf(cmd.OutOrStdout(), "Restore of %s started (mode: %s)\n", clean(id), clean(startedMode))
					}
				}
				fmt.Fprintln(stderr, "It runs in the background; follow it with `arcli backup status` or re-run with --wait.")
				restartNote()
				return nil
			}
			fmt.Fprintf(stderr, "Restore of %s started; waiting...\n", clean(id))
			lastPause := ""
			reportPause := func(progress *client.BackupProgress) {
				if progress.Operation != "restore" || progress.CompactionPause == "" || progress.CompactionPause == lastPause {
					return
				}
				lastPause = progress.CompactionPause
				fmt.Fprintf(stderr, "compaction pause: %s\n", clean(lastPause))
				if lastPause == "lost" {
					fmt.Fprintln(stderr, "warning: restore lost its compaction pause; inspect the restore error and server log")
				}
			}
			reportPause(started.Progress)
			final, raw, err := waitForOperation(cmd, &f, cli, started.Progress, waitTimeout, reportPause)
			if err != nil {
				return err
			}
			if !restoreModeConfirmed(mode, final.Mode) {
				fmt.Fprintln(stderr, "warning: final restore status did not confirm the requested mode; inspect `arcli backup status` and verify restored data before retrying.")
				if outputFormat == output.FormatJSON {
					if err := writeRawJSON(cmd.OutOrStdout(), raw); err != nil {
						return err
					}
				}
				return fmt.Errorf("requested restore mode %q was not confirmed by final status (reported %q)", mode, clean(final.Mode))
			}
			// Progress lacks skipped_reconciled; the manifest read before prompting
			// supplies it. Adjust only the display copy, never raw server JSON.
			display := *final
			display.BackupSkippedFiles = effectiveSkipped(m.SkippedFiles, m.SkippedReconciled)
			final = &display
			if outputFormat == output.FormatJSON {
				if err := writeRawJSON(cmd.OutOrStdout(), raw); err != nil {
					return err
				}
			} else if final.Status == "completed" {
				completedMode := final.Mode
				if completedMode == "" {
					completedMode = res.Mode
				}
				if completedMode == "" || completedMode == "merge" {
					fmt.Fprintf(cmd.OutOrStdout(), "Restore of %s completed: %d files, %s written\n", clean(id), final.ProcessedFiles, humanBytes(final.ProcessedBytes))
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "Restore of %s completed: %d files, %s written (mode: %s)\n", clean(id), final.ProcessedFiles, humanBytes(final.ProcessedBytes), clean(completedMode))
				}
				writeRestoreGaps(cmd.OutOrStdout(), final)
				details := *final
				details.Mode = "" // already printed in the completion line
				writeRestoreDetails(cmd.OutOrStdout(), &details)
			} else {
				describeProgress(cmd.OutOrStdout(), final, to)
			}
			restartNote()
			if final.Status != "completed" {
				return fmt.Errorf("restore of %s %s: %s", clean(id), clean(final.Status), clean(final.Error))
			}
			return nil
		},
	}
	f.add(c)
	to.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	c.Flags().StringVar(&modeFlag, "mode", "", "restore mode: merge|replace (default: the server's)")
	c.Flags().BoolVar(&dataOnly, "data-only", false, "restore data files only (skip metadata and config)")
	c.Flags().BoolVar(&withMetadata, "with-metadata", false, "also restore the metadata store (staged; unavailable on clusters)")
	c.Flags().BoolVar(&withConfig, "with-config", false, "also restore the server config file (staged)")
	c.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	c.Flags().BoolVar(&wait, "wait", false, "poll until the restore has finished")
	c.Flags().DurationVar(&waitTimeout, "wait-timeout", 2*time.Hour, "give up waiting after this long (with --wait)")
	return c
}
