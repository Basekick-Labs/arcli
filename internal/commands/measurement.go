// measurement subcommand: list measurements inside a database.
//
// Thin wrapper over GET /api/v1/databases/:name/measurements. The same
// data is also available via `arcli db show <name>`; this exposes it
// in a measurement-first flow that mirrors `kubectl get pods -n ns`
// rather than `kubectl describe namespace`.
package commands

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/basekick-labs/arcli/internal/client"
	"github.com/basekick-labs/arcli/internal/output"
)

func newMeasurementCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "measurement",
		Short: "Inspect measurements within a database",
	}
	c.AddCommand(newMeasurementListCmd())
	return c
}

func newMeasurementListCmd() *cobra.Command {
	var (
		connectionName string
		endpoint       string
		token          string
		insecure       bool
		database       string
		outputFormat   string
		noHeader       bool
		timeout        time.Duration
	)
	c := &cobra.Command{
		Use:   "list",
		Short: "List measurements inside a database",
		Long: `List measurements inside a database (GET /api/v1/databases/:name/measurements).

The database name comes from --database, or (when --database is omitted)
from the active connection's default_database. If neither is set the
command errors before any network call.`,
		Example: `  arcli measurement list --database metrics
  arcli measurement list -c prod --database logs -o json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if timeout <= 0 {
				return fmt.Errorf("--timeout must be > 0 (got %s)", timeout)
			}
			if !validListFormat(outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json, csv)", outputFormat)
			}
			cli, _, err := buildClient(cmd.ErrOrStderr(), connectionName, endpoint, token, insecure, timeout)
			if err != nil {
				return err
			}
			// Per-call --database overrides the client default; if both
			// are empty, the server would 400 on a missing :name path
			// segment, but it's friendlier to catch it client-side.
			db := database
			if db == "" {
				db = cli.DefaultDatabase()
			}
			if db == "" {
				return fmt.Errorf("no database specified (pass --database or set default_database on the active connection)")
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()

			list, err := cli.ListMeasurements(ctx, db)
			if err != nil {
				return err
			}
			return renderMeasurementList(cmd, list, outputFormat, noHeader)
		},
	}
	addCommonConnectionFlags(c, &connectionName, &endpoint, &token, &insecure)
	c.Flags().StringVar(&database, "database", "", "database to list measurements from (defaults to connection's default_database)")
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json|csv")
	c.Flags().BoolVar(&noHeader, "no-header", false, "suppress column header row (table + csv)")
	addTimeoutFlag(c, &timeout)
	return c
}

func renderMeasurementList(cmd *cobra.Command, list *client.MeasurementListResponse, format string, noHeader bool) error {
	// Defensive copy + sort: same rationale as renderDatabaseList —
	// keep table/csv/json row order consistent, don't mutate the
	// caller's slice. make+copy (not append-to-nil) guarantees the
	// result is a non-nil empty slice when the server returned no
	// measurements, so JSON output stays `"measurements": []` rather
	// than `null`. (Gemini PR #2 finding.)
	ms := make([]client.DatabaseMeasurement, len(list.Measurements))
	copy(ms, list.Measurements)
	sort.Slice(ms, func(i, j int) bool { return ms[i].Name < ms[j].Name })

	switch format {
	case output.FormatJSON:
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		// Encode with the sorted slice so JSON output matches table/CSV
		// order. Preserve list.Count and list.Database from the server.
		return enc.Encode(client.MeasurementListResponse{
			Database:     list.Database,
			Measurements: ms,
			Count:        list.Count,
		})

	case output.FormatCSV:
		return writeMeasurementListCSV(cmd.OutOrStdout(), list.Database, ms, noHeader)

	default: // FormatTable
		w := cmd.OutOrStdout()
		if len(ms) == 0 {
			fmt.Fprintf(w, "(no measurements in database %q)\n", list.Database)
			return nil
		}
		rows := make([][]string, 0, len(ms))
		for _, m := range ms {
			fc := ""
			if m.FileCount > 0 {
				fc = strconv.Itoa(m.FileCount)
			}
			rows = append(rows, []string{m.Name, fc})
		}
		headers := []string{"MEASUREMENT", "FILES"}
		if noHeader {
			headers = nil
		}
		return output.Table(w, headers, rows)
	}
}

func writeMeasurementListCSV(w io.Writer, db string, ms []client.DatabaseMeasurement, noHeader bool) error {
	cw := csv.NewWriter(w)
	if !noHeader {
		if err := cw.Write([]string{"database", "measurement", "file_count"}); err != nil {
			return err
		}
	}
	for _, m := range ms {
		fc := ""
		if m.FileCount > 0 {
			fc = strconv.Itoa(m.FileCount)
		}
		if err := cw.Write([]string{db, m.Name, fc}); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}
