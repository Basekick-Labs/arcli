// delete: row-level deletion by SQL predicate over POST /api/v1/delete/.
//
// Arc rewrites every affected Parquet file without the matching rows
// (and removes a file entirely when every row matches). The call is
// synchronous and admin-only, and refused with 403 unless the server
// runs with delete.enabled=true. Whole-database removal is `db drop`.
package commands

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/basekick-labs/arcli/internal/client"
	"github.com/basekick-labs/arcli/internal/output"
)

func newDeleteCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
		database     string
		measurement  string
		where        string
		dryRun, yes  bool
	)
	c := &cobra.Command{
		Use:   "delete --database DB --measurement M --where SQL",
		Short: "Delete rows matching a SQL predicate (admin, rewrites files)",
		Long: `Delete rows matching a SQL predicate (POST /api/v1/delete/).

Arc rewrites each affected Parquet file without the matching rows and
removes a file entirely when all of its rows match. Time bounds go in
the predicate (e.g. --where "time < '2026-01-01' AND host = 'a'").
Requires an admin token and delete.enabled=true on the server; use
"arcli db drop" to remove a whole database.

The predicate is sent as "(<where>) IS TRUE", so rows for which it is
NULL (a missing tag, say) are kept — the standard DELETE semantics — and
the preview and the real run agree exactly.

Without --yes, arcli first asks the server for a dry run and shows its
row and file counts and the server's delete limits in the confirmation
prompt; if the preview matches nothing, arcli checks the predicate with
a COUNT query so a mistyped column is reported instead of "0 rows".
--dry-run only reports. --yes skips both the preflight scan and the
prompt. The call is synchronous and --timeout defaults to 30m; on a
client timeout the server keeps rewriting — re-run with --dry-run to see
what remains. On a cluster, only the primary writer may delete; a reader
node accepts --dry-run and the preflight but refuses the real call.`,
		Example: `  arcli delete --database metrics --measurement cpu --where "host = 'decommissioned-01'" --dry-run
  arcli delete --database metrics --measurement cpu --where "time < '2025-01-01'"
  arcli delete --database metrics --measurement cpu --where "1=1" --yes   # every row of the measurement`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validObjectFormat(outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", outputFormat)
			}
			rawWhere := client.StripWhereKeyword(where)
			if err := client.ValidateWhere(rawWhere); err != nil {
				return err
			}
			req := client.DeleteRequest{Database: database, Measurement: measurement, Where: client.NormaliseWhere(rawWhere), Confirm: true}
			fullTable := client.IsFullTableWhere(rawWhere)
			cli, _, err := f.client(cmd)
			if err != nil {
				return err
			}
			stderr := cmd.ErrOrStderr()
			target := fmt.Sprintf("%s.%s", clean(database), clean(measurement))

			var previewCount int64 = -1
			if !dryRun && !yes {
				if err := requireInteractiveStdin(cmd); err != nil {
					return err
				}
				pctx, pcancel := f.ctx(cmd)
				preq := req
				preq.DryRun = true
				preview, perr := cli.DeleteRows(pctx, preq)
				pcancel()
				if perr != nil {
					var he *client.HTTPError
					if errors.As(perr, &he) {
						return fmt.Errorf("preview refused by the server (the delete itself would be refused the same way): %w", perr)
					}
					return fmt.Errorf("preview failed: %w", perr)
				}
				previewCount = preview.DeletedCount
				if preview.AffectedFiles == 0 {
					if err := explainZeroMatch(cmd, &f, cli, database, measurement, req.Where); err != nil {
						return err
					}
					if outputFormat == output.FormatJSON {
						return writeRawJSON(cmd.OutOrStdout(), preview.Raw)
					}
					fmt.Fprintf(cmd.OutOrStdout(), "No rows in %s match the predicate; nothing to delete.\n", target)
					return nil
				}
				limits := ""
				cctx, ccancel := f.ctx(cmd)
				if cfg, cerr := cli.GetDeleteConfig(cctx); cerr == nil {
					limits = fmt.Sprintf(" Server limits: confirmation threshold %d rows, max %d rows per delete.", cfg.ConfirmationThreshold, cfg.MaxRowsPerDelete)
				}
				ccancel()
				var q string
				if fullTable {
					q = fmt.Sprintf("Delete ALL %d rows of %s (%d files)? The files are removed.%s", preview.DeletedCount, target, preview.AffectedFiles, limits)
				} else {
					q = fmt.Sprintf("Delete %d rows from %s by rewriting %d file(s)?%s", preview.DeletedCount, target, preview.AffectedFiles, limits)
				}
				if err := confirmOrAbort(cmd, q, false); err != nil {
					return err
				}
			}

			req.DryRun = dryRun
			ctx, cancel := f.ctx(cmd)
			defer cancel()
			res, err := cli.DeleteRows(ctx, req)
			var partial *client.PartialDeleteError
			if errors.As(err, &partial) {
				r := partial.Result
				if len(r.FailedFiles) > 0 {
					fmt.Fprintf(stderr, "failed files: %s (re-run the same delete to retry them)\n", strings.Join(cleanAll(r.FailedFiles), ", "))
				}
				if outputFormat == output.FormatJSON {
					_ = writeRawJSON(cmd.OutOrStdout(), r.Raw)
				}
				return err
			}
			if err != nil {
				if clientGaveUp(err) && !dryRun {
					fmt.Fprintf(stderr, "arcli stopped waiting (timeout %s or interrupt); the server continues rewriting — re-run with --dry-run to see what remains\n", f.timeout)
				}
				return err
			}
			if res.AffectedFiles == 0 {
				// The server reports zeros both for "no match" and for a
				// predicate that does not evaluate; tell them apart.
				if err := explainZeroMatch(cmd, &f, cli, database, measurement, req.Where); err != nil {
					return err
				}
			}
			if !res.DryRun && previewCount >= 0 && previewCount != res.DeletedCount {
				fmt.Fprintf(stderr, "warning: the preview counted %d rows but %d were deleted; data changed between the preview and the delete\n", previewCount, res.DeletedCount)
			}
			if outputFormat == output.FormatJSON {
				return writeRawJSON(cmd.OutOrStdout(), res.Raw)
			}
			w := cmd.OutOrStdout()
			if res.DryRun {
				fmt.Fprintf(w, "Would delete %d rows from %s across %d file(s)\n", res.DeletedCount, target, res.AffectedFiles)
			} else {
				fmt.Fprintf(w, "Deleted %d rows from %s (%d file(s) rewritten, %d affected)\n", res.DeletedCount, target, res.RewrittenFiles, res.AffectedFiles)
			}
			fmt.Fprintf(w, "duration: %s\n", (time.Duration(res.ExecutionTimeMs * float64(time.Millisecond))).Round(time.Millisecond))
			return nil
		},
	}
	f.addWithTimeout(c, 30*time.Minute)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	c.Flags().StringVar(&database, "database", "", "database holding the measurement")
	c.Flags().StringVar(&measurement, "measurement", "", "measurement to delete from")
	c.Flags().StringVar(&where, "where", "", "SQL predicate selecting the rows to delete (\"1=1\" for all)")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "report what would be deleted without deleting")
	c.Flags().BoolVarP(&yes, "yes", "y", false, "skip the preflight scan and the confirmation prompt")
	_ = c.MarkFlagRequired("database")
	_ = c.MarkFlagRequired("measurement")
	_ = c.MarkFlagRequired("where")
	return c
}

// explainZeroMatch distinguishes "no rows match" from "the predicate
// does not evaluate" — the server swallows per-file evaluation errors
// and reports zeros for both. It runs the predicate through the query
// endpoint; a query error is surfaced verbatim.
//
// Only an Arc HTTP 400 (the query validator or DuckDB rejecting the
// SQL) is treated as "does not evaluate". Any other failure — transport,
// timeout, 5xx, a query-validator rule the delete route does not share —
// is reported as a warning and the server's zero result stands.
func explainZeroMatch(cmd *cobra.Command, f *connFlags, cli *client.Client, database, measurement, where string) error {
	ctx, cancel := f.ctx(cmd)
	defer cancel()
	sql := fmt.Sprintf(`SELECT count(*) FROM "%s" WHERE %s`, strings.ReplaceAll(measurement, `"`, `""`), where)
	if _, err := cli.QueryJSON(ctx, sql, database); err != nil {
		var he *client.HTTPError
		if errors.As(err, &he) && he.Status == 400 {
			return fmt.Errorf("the WHERE clause does not evaluate against %s.%s: %w", clean(database), clean(measurement), err)
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not verify the predicate against %s.%s (%v); the server reported 0 matching rows\n", clean(database), clean(measurement), err)
	}
	return nil
}

func cleanAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, clean(s))
	}
	return out
}
