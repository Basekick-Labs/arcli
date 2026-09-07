// import subcommand: bulk-load files into an Arc cluster.
//
// All four formats use POST /api/v1/import/{csv,parquet,lp,tle} on the
// server, all are multipart uploads with field name "file", and all
// require an admin token (the server's adminAuth middleware). The body
// is streamed via io.Pipe — `arcli import csv -f huge.csv` does NOT
// buffer the whole file in memory.
//
// CSV and Parquet require --measurement (file has no notion of one).
// LP measurements are self-declared in the line syntax (--measurement
// is an optional server-side filter). TLE defaults to "satellite_tle"
// when --measurement is omitted.
package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/basekick-labs/arcli/internal/client"
	"github.com/basekick-labs/arcli/internal/output"
)

func newImportCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "import",
		Short: "Bulk-load files into an Arc cluster",
		Long: `Bulk-load files into an Arc cluster.

Four formats, all admin-only on the server side (POST /api/v1/import/*).
Body is streamed end-to-end — even multi-hundred-MB files don't buffer
in memory.

  csv      — text CSV with explicit time-column + parser options
  lp       — InfluxDB-style line protocol (auto-detects gzip on the wire)
  parquet  — Apache Parquet (preserves types; fastest path)
  tle      — Two-line element satellite tracking data

CSV and Parquet require --measurement (the file doesn't carry one).
LP lines self-declare measurements (so --measurement is an optional
filter). TLE defaults to "satellite_tle".`,
	}
	c.AddCommand(
		newImportCSVCmd(),
		newImportLPCmd(),
		newImportParquetCmd(),
		newImportTLECmd(),
	)
	return c
}

// importCommonFlags is the set every import subcommand needs:
// connection, database, file path. Factored so adding PR5+ subcommands
// stays one-line.
type importCommonFlags struct {
	connectionName string
	endpoint       string
	token          string
	insecure       bool
	database       string
	filePath       string
	outputFormat   string
	timeout        time.Duration
}

// addImportCommonFlags registers the flags shared by every import
// subcommand. The format-specific subcommands add their own on top.
//
// Marks --file as required via cobra's built-in mechanism. Cobra
// enforces it before RunE runs and produces a uniform error message
// ("required flag(s) \"file\" not set"), so individual subcommands
// don't need manual `if filePath == ""` guards. (Gemini PR #3 r5.)
func addImportCommonFlags(c *cobra.Command, f *importCommonFlags) {
	addCommonConnectionFlags(c, &f.connectionName, &f.endpoint, &f.token, &f.insecure)
	c.Flags().StringVar(&f.database, "database", "", "target database (required; can also come from active connection's default_database)")
	c.Flags().StringVarP(&f.filePath, "file", "f", "", "path to the input file (required)")
	c.Flags().StringVarP(&f.outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	addTimeoutFlag(c, &f.timeout)
	_ = c.MarkFlagRequired("file")
}

// resolveImportDatabase picks --database if set, else the active
// connection's default_database. Empty → error before any network call.
// Mirrors the precedence used by `measurement list`.
func resolveImportDatabase(flag string, cli *client.Client) (string, error) {
	db := flag
	if db == "" {
		db = cli.DefaultDatabase()
	}
	if db == "" {
		return "", fmt.Errorf("no database specified (pass --database or set default_database on the active connection)")
	}
	return db, nil
}

// validImportOutputFormat reports whether the format is one of the two
// supported by import commands (table, json — no csv since the result
// IS the import outcome, not tabular data; no arrow since these
// endpoints don't stream).
func validImportOutputFormat(s string) bool {
	switch s {
	case output.FormatTable, output.FormatJSON:
		return true
	}
	return false
}

// ---- csv ------------------------------------------------------------------

func newImportCSVCmd() *cobra.Command {
	var (
		common      importCommonFlags
		measurement string
		timeColumn  string
		timeFormat  string
		delimiter   string
		skipRows    int
	)
	c := &cobra.Command{
		Use:   "csv",
		Short: "Import a CSV file into a measurement",
		Example: `  arcli import csv -f data.csv --database metrics --measurement cpu
  arcli import csv -f data.csv --database metrics --measurement cpu \
      --time-column ts --time-format epoch_ms --delimiter ';' --skip-rows 1`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if common.timeout <= 0 {
				return fmt.Errorf("--timeout must be > 0 (got %s)", common.timeout)
			}
			if !validImportOutputFormat(common.outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", common.outputFormat)
			}
			// --file and --measurement are enforced by cobra's
			// MarkFlagRequired below — no manual check needed here.
			if skipRows < 0 {
				return fmt.Errorf("--skip-rows must be >= 0 (got %d)", skipRows)
			}

			cli, _, err := buildClient(cmd.ErrOrStderr(), common.connectionName, common.endpoint, common.token, common.insecure, common.timeout)
			if err != nil {
				return err
			}
			db, err := resolveImportDatabase(common.database, cli)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), common.timeout)
			defer cancel()

			result, err := cli.ImportCSV(ctx, common.filePath, db, measurement, client.CSVImportOptions{
				TimeColumn: timeColumn,
				TimeFormat: timeFormat,
				Delimiter:  delimiter,
				SkipRows:   skipRows,
			})
			if err != nil {
				return err
			}
			return renderImportResult(cmd, result, common.outputFormat)
		},
	}
	addImportCommonFlags(c, &common)
	c.Flags().StringVar(&measurement, "measurement", "", "target measurement name (required)")
	_ = c.MarkFlagRequired("measurement")
	c.Flags().StringVar(&timeColumn, "time-column", "", "column to use as the row timestamp (server default: time)")
	c.Flags().StringVar(&timeFormat, "time-format", "", "epoch_s|epoch_ms|epoch_us|epoch_ns (empty = let DuckDB infer, works for ISO-8601 strings)")
	c.Flags().StringVar(&delimiter, "delimiter", "", "field separator (server default: ,)")
	c.Flags().IntVar(&skipRows, "skip-rows", 0, "number of header rows to skip before parsing")
	return c
}

// ---- lp -------------------------------------------------------------------

func newImportLPCmd() *cobra.Command {
	var (
		common            importCommonFlags
		precision         string
		measurementFilter string
	)
	c := &cobra.Command{
		Use:   "lp",
		Short: "Import a Line Protocol file into a database",
		Long: `Import a Line Protocol file into a database (POST /api/v1/import/lp).

LP lines carry their own measurement names, so --measurement here acts
as a server-side filter rather than a destination. The server auto-
detects gzip via magic bytes — pass either a .lp or a .lp.gz file.

Server-side cap: 500 MB decompressed.`,
		Example: `  arcli import lp -f telegraf-snapshot.lp --database metrics
  arcli import lp -f data.lp.gz --database metrics --precision ms
  arcli import lp -f data.lp --database metrics --measurement cpu  # filter to cpu`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if common.timeout <= 0 {
				return fmt.Errorf("--timeout must be > 0 (got %s)", common.timeout)
			}
			if !validImportOutputFormat(common.outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", common.outputFormat)
			}
			// --file is enforced by cobra's MarkFlagRequired in
			// addImportCommonFlags.
			if precision != "" && !client.ValidPrecision(precision) {
				return fmt.Errorf("invalid --precision %q (must be one of ns, us, ms, s)", precision)
			}

			cli, _, err := buildClient(cmd.ErrOrStderr(), common.connectionName, common.endpoint, common.token, common.insecure, common.timeout)
			if err != nil {
				return err
			}
			db, err := resolveImportDatabase(common.database, cli)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), common.timeout)
			defer cancel()

			result, err := cli.ImportLP(ctx, common.filePath, db, client.LPImportOptions{
				Precision:         client.Precision(precision),
				MeasurementFilter: measurementFilter,
			})
			if err != nil {
				return err
			}
			return renderLPImportResult(cmd, result, common.outputFormat)
		},
	}
	addImportCommonFlags(c, &common)
	c.Flags().StringVar(&precision, "precision", "", "timestamp precision: ns|us|ms|s (default: server-side default = ns)")
	c.Flags().StringVar(&measurementFilter, "measurement", "", "filter to a single measurement (LP lines for other measurements are dropped)")
	return c
}

// ---- parquet --------------------------------------------------------------

func newImportParquetCmd() *cobra.Command {
	var (
		common      importCommonFlags
		measurement string
		timeColumn  string
	)
	c := &cobra.Command{
		Use:   "parquet",
		Short: "Import a Parquet file into a measurement",
		Long: `Import a Parquet file into a measurement (POST /api/v1/import/parquet).

Parquet preserves column types end-to-end — faster + lossless compared
to CSV for the same data.`,
		Example: `  arcli import parquet -f data.parquet --database metrics --measurement cpu
  arcli import parquet -f data.parquet --database metrics --measurement cpu --time-column ts`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if common.timeout <= 0 {
				return fmt.Errorf("--timeout must be > 0 (got %s)", common.timeout)
			}
			if !validImportOutputFormat(common.outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", common.outputFormat)
			}
			// --file and --measurement are enforced by cobra's
			// MarkFlagRequired below.

			cli, _, err := buildClient(cmd.ErrOrStderr(), common.connectionName, common.endpoint, common.token, common.insecure, common.timeout)
			if err != nil {
				return err
			}
			db, err := resolveImportDatabase(common.database, cli)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), common.timeout)
			defer cancel()

			result, err := cli.ImportParquet(ctx, common.filePath, db, measurement, client.ParquetImportOptions{
				TimeColumn: timeColumn,
			})
			if err != nil {
				return err
			}
			return renderImportResult(cmd, result, common.outputFormat)
		},
	}
	addImportCommonFlags(c, &common)
	c.Flags().StringVar(&measurement, "measurement", "", "target measurement name (required)")
	_ = c.MarkFlagRequired("measurement")
	c.Flags().StringVar(&timeColumn, "time-column", "", "column to use as the row timestamp (server default: time)")
	return c
}

// ---- tle ------------------------------------------------------------------

func newImportTLECmd() *cobra.Command {
	var (
		common      importCommonFlags
		measurement string
	)
	c := &cobra.Command{
		Use:   "tle",
		Short: "Import a TLE (two-line element) satellite-tracking file",
		Long: `Import a TLE (two-line element) satellite-tracking file (POST /api/v1/import/tle).

TLE is the standard NORAD/NASA format for orbital state vectors. The
server parses each three-line record (name + line1 + line2) and ingests
one row per satellite into the target measurement.

--measurement defaults to "satellite_tle" if omitted (matches the
server's default behavior).`,
		Example: `  arcli import tle -f starlink.tle --database satellites
  arcli import tle -f starlink.tle --database satellites --measurement starlink`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if common.timeout <= 0 {
				return fmt.Errorf("--timeout must be > 0 (got %s)", common.timeout)
			}
			if !validImportOutputFormat(common.outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", common.outputFormat)
			}
			// --file is enforced by cobra's MarkFlagRequired in
			// addImportCommonFlags. --measurement is optional for TLE
			// (server defaults to "satellite_tle"), so no required mark.

			cli, _, err := buildClient(cmd.ErrOrStderr(), common.connectionName, common.endpoint, common.token, common.insecure, common.timeout)
			if err != nil {
				return err
			}
			db, err := resolveImportDatabase(common.database, cli)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), common.timeout)
			defer cancel()

			result, err := cli.ImportTLE(ctx, common.filePath, db, client.TLEImportOptions{
				Measurement: measurement,
			})
			if err != nil {
				return err
			}
			return renderTLEImportResult(cmd, result, common.outputFormat)
		},
	}
	addImportCommonFlags(c, &common)
	c.Flags().StringVar(&measurement, "measurement", "", "target measurement (server default: satellite_tle)")
	return c
}

// ---- render helpers --------------------------------------------------------

// renderImportResult writes a CSV/Parquet ImportResult in the chosen
// format. Two formats only: table (default) and json. CSV doesn't make
// sense here because the "result" is a single record describing the
// import outcome, not tabular data.
//
// JSON output normalizes nil slices to empty slices so `"columns":
// null` from the server (or omitted field) renders as `"columns": []`
// — same pattern as PR3 to keep consumers' assumptions stable.
func renderImportResult(cmd *cobra.Command, r *client.ImportResult, format string) error {
	if format == output.FormatJSON {
		// Shallow copy of the result is safe today because every field
		// of ImportResult is a value type or slice; the reassignment
		// `out.Columns = []string{}` points the COPY's slice header
		// at a fresh slice without touching the caller's `r`. If
		// ImportResult ever grows a pointer/map field this comment
		// needs revisiting.
		out := *r
		if out.Columns == nil {
			out.Columns = []string{}
		}
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(&out)
	}
	w := cmd.OutOrStdout()
	fmt.Fprintln(w, "OK")
	fmt.Fprintf(w, "  database:           %s\n", r.Database)
	fmt.Fprintf(w, "  measurement:        %s\n", r.Measurement)
	fmt.Fprintf(w, "  rows_imported:      %d\n", r.RowsImported)
	fmt.Fprintf(w, "  partitions_created: %d\n", r.PartitionsCreated)
	if r.TimeRangeMin != "" || r.TimeRangeMax != "" {
		fmt.Fprintf(w, "  time_range:         [%s … %s]\n", r.TimeRangeMin, r.TimeRangeMax)
	}
	if len(r.Columns) > 0 {
		fmt.Fprintf(w, "  columns:            %s\n", strings.Join(r.Columns, ", "))
	}
	fmt.Fprintf(w, "  duration_ms:        %d\n", r.DurationMs)
	return nil
}

// renderLPImportResult writes an LP-specific import result. LP can ingest
// multiple measurements from one file, so the field is plural.
//
// JSON output normalizes nil Measurements to []string{} (PR3 pattern).
func renderLPImportResult(cmd *cobra.Command, r *client.LPImportResult, format string) error {
	if format == output.FormatJSON {
		// Shallow copy: see renderImportResult for the safety rationale.
		out := *r
		if out.Measurements == nil {
			out.Measurements = []string{}
		}
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(&out)
	}
	w := cmd.OutOrStdout()
	fmt.Fprintln(w, "OK")
	fmt.Fprintf(w, "  database:      %s\n", r.Database)
	fmt.Fprintf(w, "  measurements:  %s\n", strings.Join(r.Measurements, ", "))
	fmt.Fprintf(w, "  rows_imported: %d\n", r.RowsImported)
	fmt.Fprintf(w, "  precision:     %s\n", r.Precision)
	fmt.Fprintf(w, "  duration_ms:   %d\n", r.DurationMs)
	return nil
}

// renderTLEImportResult writes a TLE-specific import result. Includes
// the satellite count + any parser warnings (TLE files often have
// invalid checksums on individual satellites; the server keeps the
// good ones and reports the bad in ParseWarnings).
func renderTLEImportResult(cmd *cobra.Command, r *client.TLEImportResult, format string) error {
	if format == output.FormatJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	}
	w := cmd.OutOrStdout()
	fmt.Fprintln(w, "OK")
	fmt.Fprintf(w, "  database:        %s\n", r.Database)
	fmt.Fprintf(w, "  measurement:     %s\n", r.Measurement)
	fmt.Fprintf(w, "  satellite_count: %d\n", r.SatelliteCount)
	fmt.Fprintf(w, "  rows_imported:   %d\n", r.RowsImported)
	fmt.Fprintf(w, "  duration_ms:     %d\n", r.DurationMs)
	if len(r.ParseWarnings) > 0 {
		fmt.Fprintf(w, "  parse_warnings (%d):\n", len(r.ParseWarnings))
		for _, line := range r.ParseWarnings {
			fmt.Fprintf(w, "    - %s\n", line)
		}
	}
	return nil
}
