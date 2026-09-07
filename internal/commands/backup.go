// backup subcommand: full backups and restores over /api/v1/backup/*.
//
// Backups are always full (every database) and written to the server's
// backup.local_path. Create and restore are asynchronous on the server
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

A backup contains every database plus, by default, the metadata store
(tokens, policies, continuous queries) and the server config. It is
written to the server's backup.local_path; there is no remote
destination and no incremental mode. Backups are disabled when the
server runs with backup.enabled=false.`,
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
func waitForOperation(cmd *cobra.Command, f *connFlags, cli *client.Client, expectID string, waitTimeout time.Duration) (*client.BackupProgress, []byte, error) {
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
		if expectID != "" && p.BackupID != expectID {
			return nil, nil, fmt.Errorf("the server is now tracking a different operation (%s %s); ours has finished — see `arcli backup show %s` or `arcli backup list`", clean(p.Operation), clean(p.BackupID), clean(expectID))
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

func describeProgress(w io.Writer, p *client.BackupProgress) {
	fmt.Fprintf(w, "operation:  %s\n", clean(p.Operation))
	fmt.Fprintf(w, "backup id:  %s\n", clean(p.BackupID))
	fmt.Fprintf(w, "status:     %s\n", clean(p.Status))
	fmt.Fprintf(w, "files:      %d / %d (%d skipped)\n", p.ProcessedFiles, p.TotalFiles, p.SkippedFiles)
	fmt.Fprintf(w, "bytes:      %s / %s\n", humanBytes(p.ProcessedBytes), humanBytes(p.TotalBytes))
	fmt.Fprintf(w, "started:    %s\n", fmtTimeVal(p.StartedAt, "-"))
	if p.CompletedAt != nil {
		fmt.Fprintf(w, "completed:  %s\n", fmtTimeVal(*p.CompletedAt, "-"))
	}
	if p.Error != "" {
		fmt.Fprintf(w, "error:      %s\n", clean(p.Error))
	}
}

// ---- create ----------------------------------------------------------------

func newBackupCreateCmd() *cobra.Command {
	var (
		f                    connFlags
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
deletion can run at a time.`,
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
			final, raw, err := waitForOperation(cmd, &f, cli, id, waitTimeout)
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
				describeProgress(cmd.OutOrStdout(), final)
				return fmt.Errorf("backup %s %s: %s", clean(id), clean(final.Status), clean(final.Error))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Backup %s completed: %d files, %s", clean(id), final.ProcessedFiles, humanBytes(final.ProcessedBytes))
			if final.SkippedFiles > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), " (%d files skipped — the backup is incomplete)", final.SkippedFiles)
			}
			fmt.Fprintln(cmd.OutOrStdout())
			return nil
		},
	}
	f.add(c)
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
		outputFormat string
		noHeader     bool
	)
	c := &cobra.Command{
		Use:   "list",
		Short: "List backups, newest first",
		Long: `List backups (GET /api/v1/backup/), newest first. The listing cannot
tell whether a backup is incomplete; "backup show" reports skipped files.

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
					if err := cw.Write([]string{"backup_id", "created_at", "backup_type", "database_count", "total_files", "total_size_bytes"}); err != nil {
						return err
					}
				}
				for _, b := range list {
					if err := cw.Write([]string{b.BackupID, fmtTimeVal(b.CreatedAt, ""), b.BackupType, strconv.Itoa(b.DatabaseCount), strconv.FormatInt(b.TotalFiles, 10), strconv.FormatInt(b.TotalBytes, 10)}); err != nil {
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
				rows = append(rows, []string{clean(b.BackupID), fmtTimeVal(b.CreatedAt, "-"), clean(b.BackupType), strconv.Itoa(b.DatabaseCount), strconv.FormatInt(b.TotalFiles, 10), humanBytes(b.TotalBytes)})
			}
			headers := []string{"ID", "CREATED", "TYPE", "DATABASES", "FILES", "SIZE"}
			if noHeader {
				headers = nil
			}
			return output.Table(w, headers, rows)
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json|csv")
	c.Flags().BoolVar(&noHeader, "no-header", false, "suppress column header row (table + csv)")
	return c
}

func newBackupShowCmd() *cobra.Command {
	var (
		f            connFlags
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
			return writeManifest(w, m)
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	return c
}

func writeManifest(w io.Writer, m *client.BackupManifest) error {
	fmt.Fprintf(w, "backup id:   %s\n", clean(m.BackupID))
	fmt.Fprintf(w, "created:     %s\n", fmtTimeVal(m.CreatedAt, "-"))
	fmt.Fprintf(w, "type:        %s (server %s)\n", clean(m.BackupType), clean(m.Version))
	fmt.Fprintf(w, "contents:    %d files, %s, metadata %t, config %t\n", m.TotalFiles, humanBytes(m.TotalSizeBytes), m.HasMetadata, m.HasConfig)
	if m.SkippedFiles > 0 {
		fmt.Fprintf(w, "INCOMPLETE:  %d files were skipped while backing up\n", m.SkippedFiles)
	}
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

func newBackupStatusCmd() *cobra.Command {
	var (
		f            connFlags
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
			describeProgress(w, st.Progress)
			return nil
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	return c
}

// ---- delete ----------------------------------------------------------------

func newBackupDeleteCmd() *cobra.Command {
	var (
		f   connFlags
		yes bool
	)
	c := &cobra.Command{
		Use:   "delete <id>",
		Short: "Delete a backup from the server's backup directory",
		Args:  cobra.ExactArgs(1),
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
			q := fmt.Sprintf("Delete backup %s (%s, %d files, %s)?", clean(id), fmtTimeVal(m.CreatedAt, "-"), m.TotalFiles, humanBytes(m.TotalSizeBytes))
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
	c.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	return c
}

// ---- restore ---------------------------------------------------------------

func newBackupRestoreCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
		dataOnly     bool
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
on a quiescent server: stop writers and compaction first. In cluster
mode restored files are not registered in the cluster manifest.

By default the metadata store (tokens, policies, continuous queries) is
restored too, but it is only STAGED: the server applies it at its next
start. --data-only skips metadata and config; --with-config also
restores the server config file (staged as well). The restore runs in
the background; --wait polls until it finishes (--wait-timeout defaults
to 2h).`,
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
			if dataOnly && withConfig {
				return fmt.Errorf("--data-only and --with-config are mutually exclusive")
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
			restoreMeta := !dataOnly && m.HasMetadata
			restoreCfg := withConfig
			if withConfig && !m.HasConfig {
				return fmt.Errorf("backup %s does not contain the server config; drop --with-config", clean(id))
			}
			stderr := cmd.ErrOrStderr()
			if m.SkippedFiles > 0 {
				fmt.Fprintf(stderr, "warning: backup %s is incomplete (%d files were skipped when it was taken)\n", clean(id), m.SkippedFiles)
			}
			dbs := make([]string, 0, len(m.Databases))
			for _, db := range m.Databases {
				dbs = append(dbs, clean(db.Name))
			}
			scope := "the live storage"
			if len(dbs) > 0 {
				scope = "the live storage of database(s) " + strings.Join(dbs, ", ")
			}
			parts := []string{fmt.Sprintf("Restore backup %s (%s, %d files, %s) over %s? Existing files at the same paths are replaced.", clean(id), fmtTimeVal(m.CreatedAt, "-"), m.TotalFiles, humanBytes(m.TotalSizeBytes), scope)}
			if restoreMeta {
				parts = append(parts, "The metadata store will be staged and applied at the next server start.")
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
			if restoreMeta {
				opts.RestoreMetadata = &t
			} else {
				opts.RestoreMetadata = &fls
			}
			if restoreCfg {
				opts.RestoreConfig = &t
			} else {
				opts.RestoreConfig = &fls
			}
			ctx, cancel = f.ctx(cmd)
			res, err := cli.RestoreBackup(ctx, id, opts)
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
			restartNote := func() {
				if restoreMeta || restoreCfg {
					fmt.Fprintln(stderr, "Metadata/config are staged in the server's metadata directory (.pending-restore) and applied at the next server start; data files are already overwritten. Restart the server to complete the restore.")
				}
			}
			if started == nil {
				fmt.Fprintln(stderr, "Restore accepted, but the server has not published its progress yet; check `arcli backup status`.")
				if outputFormat == output.FormatJSON {
					if err := writeRawJSON(cmd.OutOrStdout(), res.Raw); err != nil {
						return err
					}
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
					fmt.Fprintf(cmd.OutOrStdout(), "Restore of %s started\n", clean(id))
				}
				fmt.Fprintln(stderr, "It runs in the background; follow it with `arcli backup status` or re-run with --wait.")
				restartNote()
				return nil
			}
			fmt.Fprintf(stderr, "Restore of %s started; waiting...\n", clean(id))
			final, raw, err := waitForOperation(cmd, &f, cli, id, waitTimeout)
			if err != nil {
				return err
			}
			if outputFormat == output.FormatJSON {
				if err := writeRawJSON(cmd.OutOrStdout(), raw); err != nil {
					return err
				}
			} else if final.Status == "completed" {
				fmt.Fprintf(cmd.OutOrStdout(), "Restore of %s completed: %d files, %s written\n", clean(id), final.ProcessedFiles, humanBytes(final.ProcessedBytes))
			} else {
				describeProgress(cmd.OutOrStdout(), final)
			}
			restartNote()
			if final.Status != "completed" {
				return fmt.Errorf("restore of %s %s: %s", clean(id), clean(final.Status), clean(final.Error))
			}
			return nil
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	c.Flags().BoolVar(&dataOnly, "data-only", false, "restore data files only (skip metadata and config)")
	c.Flags().BoolVar(&withConfig, "with-config", false, "also restore the server config file (staged)")
	c.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	c.Flags().BoolVar(&wait, "wait", false, "poll until the restore has finished")
	c.Flags().DurationVar(&waitTimeout, "wait-timeout", 2*time.Hour, "give up waiting after this long (with --wait)")
	return c
}
