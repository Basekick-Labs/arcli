package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/basekick-labs/arcli/internal/client"
	"github.com/basekick-labs/arcli/internal/config"
	"github.com/basekick-labs/arcli/internal/output"
	"github.com/basekick-labs/arcli/internal/sample"
)

// downloadConcurrency is fixed rather than a flag. Four is enough to
// saturate an ordinary link on files of this size, and a knob here would
// be a knob on someone else's bandwidth as much as the user's own.
const downloadConcurrency = 4

// sampleTimeout is the default budget for a whole sample operation, not
// one request: tens of MiB over a slow connection is a legitimate several
// minutes. It is the default value of --timeout for these commands, and
// --timeout is the single deadline the fetcher and every context use, so
// the three do not drift apart.
const sampleTimeout = 30 * time.Minute

func newSampleCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "sample",
		Short: "Load a real public dataset into your Arc",
		Long: fmt.Sprintf(`Load a real public dataset into your Arc.

A synthetic data point proves the write path works; it does not show you
what Arc is for. These commands put a real dataset into a database you
own, then hand you queries worth running against it.

Datasets are fetched from %s. This is the only part of arcli
that contacts a Basekick-controlled host; every other command talks solely
to the Arc servers you configured. These requests carry the same
User-Agent and installation id as any other arcli request, and no token
and no Arc endpoint. DO_NOT_TRACK=1 or send_installation_id = false
suppress the id here as they do everywhere else.

Prefer not to fetch from us at all? `+"`arcli sample show <dataset> -o json`"+`
prints every file URL and checksum, for use with `+"`arcli import parquet`"+`.`, sample.BaseURL),
	}
	c.AddCommand(newSampleListCmd(), newSampleShowCmd(), newSampleLoadCmd())
	return c
}

// sampleClient builds the dataset fetcher, carrying the same identity
// headers as every other arcli request. OutboundInstallationID applies
// the existing opt-outs (DO_NOT_TRACK, send_installation_id), so opting
// out of the id everywhere else opts out here too.
//
// A missing or unreadable config file is not a reason to fail a download:
// it just means no id to send.
func sampleClient(timeout time.Duration) *sample.Client {
	var installationID string
	if cfg, err := config.Load(); err == nil {
		installationID = cfg.OutboundInstallationID()
	}
	return sample.NewClient(userAgent(), installationID, timeout)
}

// ---- list -----------------------------------------------------------------

func newSampleListCmd() *cobra.Command {
	var (
		outputFormat string
		timeout      time.Duration
	)
	c := &cobra.Command{
		Use:   "list",
		Short: "List the available sample datasets",
		Args:  cobra.NoArgs,
		Example: `  arcli sample list
  arcli sample list -o json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validImportOutputFormat(outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", outputFormat)
			}
			if timeout <= 0 {
				return fmt.Errorf("--timeout must be > 0 (got %s)", timeout)
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()

			m, err := sampleClient(timeout).Manifest(ctx)
			if err != nil {
				return err
			}
			return renderSampleList(cmd, m, outputFormat)
		},
	}
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "table|json")
	addTimeoutFlagDefault(c, &timeout, sampleTimeout)
	return c
}

// sampleListRow is one dataset+month pair: a dataset with three published
// months is three rows, so the row count matches what can be loaded.
type sampleListRow struct {
	Dataset     string `json:"dataset"`
	Month       string `json:"month"`
	Rows        int64  `json:"rows"`
	Size        string `json:"size"`
	Files       int    `json:"files"`
	Title       string `json:"title"`
	Measurement string `json:"measurement"`
	Database    string `json:"suggested_database"`
}

func renderSampleList(cmd *cobra.Command, m *sample.Manifest, format string) error {
	var rows []sampleListRow
	for _, id := range m.DatasetIDs() {
		d := m.Datasets[id]
		for _, mk := range d.MonthKeys() {
			mo := d.Months[mk]
			rows = append(rows, sampleListRow{
				Dataset:     id,
				Month:       mk,
				Rows:        mo.Rows,
				Size:        humanBytes(mo.Bytes),
				Files:       mo.Files,
				Title:       d.Title,
				Measurement: d.Measurement,
				Database:    d.SuggestedDatabase,
			})
		}
	}
	if format == output.FormatJSON {
		return writeSampleJSON(cmd, rows)
	}
	if len(rows) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No sample datasets are published.")
		return nil
	}
	table := make([][]string, 0, len(rows))
	for _, r := range rows {
		table = append(table, []string{
			r.Dataset, r.Month, fmt.Sprintf("%d", r.Rows), r.Size, fmt.Sprintf("%d", r.Files), r.Title,
		})
	}
	if err := output.Table(cmd.OutOrStdout(),
		[]string{"DATASET", "MONTH", "ROWS", "SIZE", "FILES", "TITLE"}, table); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "\nLoad one with:  arcli sample load %s\n", rows[0].Dataset)
	return nil
}

// writeSampleJSON renders v as indented JSON, matching the -o json shape
// the other commands emit.
func writeSampleJSON(cmd *cobra.Command, v any) error {
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// ---- show -----------------------------------------------------------------

func newSampleShowCmd() *cobra.Command {
	var (
		month        string
		outputFormat string
		timeout      time.Duration
	)
	c := &cobra.Command{
		Use:   "show DATASET",
		Short: "Show a sample dataset's details, licence and file list",
		Long: `Show a sample dataset's details, licence and file list.

With -o json the output includes every file URL and its SHA-256, so the
dataset can be downloaded without arcli and imported with
` + "`arcli import parquet`" + `.`,
		Args: cobra.ExactArgs(1),
		Example: `  arcli sample show citibike
  arcli sample show citibike -o json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validImportOutputFormat(outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", outputFormat)
			}
			if timeout <= 0 {
				return fmt.Errorf("--timeout must be > 0 (got %s)", timeout)
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()

			m, err := sampleClient(timeout).Manifest(ctx)
			if err != nil {
				return err
			}
			d, err := m.Dataset(args[0])
			if err != nil {
				return err
			}
			mk, mo, err := d.Month(month)
			if err != nil {
				return err
			}
			return renderSampleShow(cmd, args[0], d, mk, mo, outputFormat)
		},
	}
	c.Flags().StringVar(&month, "month", "", "month to describe (default: newest published)")
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "table|json")
	addTimeoutFlagDefault(c, &timeout, sampleTimeout)
	return c
}

type sampleShowJSON struct {
	Dataset     string       `json:"dataset"`
	Title       string       `json:"title"`
	Description string       `json:"description"`
	SourceURL   string       `json:"source_url"`
	License     string       `json:"license"`
	Measurement string       `json:"measurement"`
	Database    string       `json:"suggested_database"`
	Month       string       `json:"month"`
	Rows        int64        `json:"rows"`
	Bytes       int64        `json:"bytes"`
	Files       int          `json:"files"`
	TimeMin     string       `json:"time_min"`
	TimeMax     string       `json:"time_max"`
	Parts       []samplePart `json:"parts"`
}

type samplePart struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Bytes  int64  `json:"bytes"`
	Rows   int64  `json:"rows"`
	SHA256 string `json:"sha256"`
}

func renderSampleShow(cmd *cobra.Command, id string, d sample.Dataset, mk string, mo sample.Month, format string) error {
	if format == output.FormatJSON {
		parts := make([]samplePart, 0, len(mo.Parts))
		for _, p := range mo.Parts {
			parts = append(parts, samplePart{
				Name: p.Name(), URL: p.URL(), Bytes: p.Bytes, Rows: p.Rows, SHA256: p.SHA256,
			})
		}
		return writeSampleJSON(cmd, sampleShowJSON{
			Dataset: id, Title: d.Title, Description: d.Description,
			SourceURL: d.SourceURL, License: d.License,
			Measurement: d.Measurement, Database: d.SuggestedDatabase,
			Month: mk, Rows: mo.Rows, Bytes: mo.Bytes, Files: mo.Files,
			TimeMin: mo.TimeMin, TimeMax: mo.TimeMax, Parts: parts,
		})
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "%s (%s)\n", d.Title, id)
	if d.Description != "" {
		fmt.Fprintf(out, "%s\n", d.Description)
	}
	fmt.Fprintln(out)

	rows := [][]string{
		{"Month", mk},
		{"Rows", fmt.Sprintf("%d", mo.Rows)},
		{"Download size", humanBytes(mo.Bytes)},
		{"Files", fmt.Sprintf("%d", mo.Files)},
	}
	if mo.TimeMin != "" {
		rows = append(rows, []string{"Time range", mo.TimeMin + "  to  " + mo.TimeMax})
	}
	rows = append(rows,
		[]string{"Measurement", d.Measurement},
		[]string{"Suggested database", d.SuggestedDatabase},
	)
	if d.SourceURL != "" {
		rows = append(rows, []string{"Source", d.SourceURL})
	}
	if d.License != "" {
		rows = append(rows, []string{"Licence", d.License})
	}
	if err := output.Table(out, []string{"FIELD", "VALUE"}, rows); err != nil {
		return err
	}
	fmt.Fprintf(out, "\nLoad it with:  arcli sample load %s\n", id)
	return nil
}

// ---- load -----------------------------------------------------------------

func newSampleLoadCmd() *cobra.Command {
	var (
		conn         connFlags
		month        string
		database     string
		measurement  string
		downloadDir  string
		downloadOnly bool
		yes          bool
	)
	c := &cobra.Command{
		Use:   "load DATASET",
		Short: "Download a sample dataset and import it into your Arc",
		Long: fmt.Sprintf(`Download a sample dataset and import it into your Arc.

Files are fetched from %s, verified against the SHA-256
in the published manifest, and imported one by one. The target database
is created if it does not exist.

Importing requires an admin token when the server has authentication
enabled. Interrupted downloads resume: verified files are not fetched
again.`, sample.BaseURL),
		Args: cobra.ExactArgs(1),
		Example: `  arcli sample load citibike
  arcli sample load citibike --database nyc --measurement trips
  arcli sample load citibike --download-only --download-dir ./data`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSampleLoad(cmd, args[0], sampleLoadOpts{
				conn:         &conn,
				month:        month,
				database:     database,
				measurement:  measurement,
				downloadDir:  downloadDir,
				downloadOnly: downloadOnly,
				yes:          yes,
			})
		},
	}
	conn.addWithTimeout(c, sampleTimeout)
	c.Flags().StringVar(&month, "month", "", "month to load (default: newest published)")
	c.Flags().StringVar(&database, "database", "", "target database (default: the dataset's suggested database)")
	c.Flags().StringVar(&measurement, "measurement", "", "target measurement (default: the dataset's measurement)")
	c.Flags().StringVar(&downloadDir, "download-dir", "", "keep the downloaded Parquet here instead of a temp directory")
	c.Flags().BoolVar(&downloadOnly, "download-only", false, "download and verify, but do not import")
	c.Flags().BoolVar(&yes, "yes", false, "proceed without confirmation when the measurement already holds data")
	return c
}

type sampleLoadOpts struct {
	conn         *connFlags
	month        string
	database     string
	measurement  string
	downloadDir  string
	downloadOnly bool
	yes          bool
}

func runSampleLoad(cmd *cobra.Command, id string, o sampleLoadOpts) error {
	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()

	sc := sampleClient(o.conn.timeout)
	mfCtx, cancel := context.WithTimeout(cmd.Context(), o.conn.timeout)
	m, err := sc.Manifest(mfCtx)
	cancel()
	if err != nil {
		return err
	}
	d, err := m.Dataset(id)
	if err != nil {
		return err
	}
	mk, mo, err := d.Month(o.month)
	if err != nil {
		return err
	}

	measurement := o.measurement
	if measurement == "" {
		measurement = d.Measurement
	}
	if measurement == "" {
		return errors.New("dataset does not name a measurement; pass --measurement")
	}

	// --download-only needs no Arc at all, so don't demand a connection
	// for it: "fetch me the files" is a legitimate use on a machine with
	// no configured server.
	var (
		cli *client.Client
		db  string
	)
	if !o.downloadOnly {
		cli, _, err = o.conn.client(cmd)
		if err != nil {
			return err
		}
		db = o.database
		if db == "" {
			db = d.SuggestedDatabase
		}
		if db == "" {
			db = cli.DefaultDatabase()
		}
		if db == "" {
			return errors.New("no database specified (pass --database)")
		}
		if err := sampleImportPreflight(cmd, cli, db, measurement, o.yes); err != nil {
			return err
		}
	}

	dir := sample.DownloadDir(o.downloadDir, id, mk)

	// Say what is about to happen before it happens. No prompt: the user
	// typed the command. The one prompt lives in the preflight above,
	// where existing data is at stake.
	fmt.Fprintf(out, "Dataset:     %s (%s), %s\n", d.Title, id, mk)
	fmt.Fprintf(out, "Source:      %s\n", sample.BaseURL)
	fmt.Fprintf(out, "Download:    %d files, %s, %d rows\n", mo.Files, humanBytes(mo.Bytes), mo.Rows)
	fmt.Fprintf(out, "Directory:   %s\n", dir)
	if !o.downloadOnly {
		fmt.Fprintf(out, "Target:      %s.%s\n", db, measurement)
	}
	if d.License != "" {
		fmt.Fprintf(out, "Licence:     %s\n", d.License)
	}
	fmt.Fprintln(out)

	dlCtx, dlCancel := context.WithTimeout(cmd.Context(), o.conn.timeout)
	defer dlCancel()

	paths, err := sc.Fetch(dlCtx, dir, mo.Parts, downloadConcurrency, func(done, total int, p sample.Part, cached bool) {
		verb := "downloaded"
		if cached {
			verb = "cached"
		}
		fmt.Fprintf(errOut, "\r  %s %d/%d (%s)          ", verb, done, total, p.Name())
	})
	fmt.Fprintln(errOut)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Verified %d files against the published checksums.\n", len(paths))

	if o.downloadOnly {
		fmt.Fprintf(out, "\nFiles are in %s\n", dir)
		if o.downloadDir == "" {
			// Nothing cleans this up on the download-only path, and the
			// OS may reap the temp directory before the user gets back
			// to it. Say so rather than let them find out later.
			fmt.Fprintf(out, "That is a temporary directory; pass --download-dir to keep them somewhere durable.\n")
		}
		fmt.Fprintf(out, "Import them with:  arcli import parquet -f <file> --database %s --measurement %s\n",
			firstNonEmpty(o.database, d.SuggestedDatabase, "<database>"), measurement)
		return nil
	}

	imported, rows, err := sampleImport(cmd, cli, paths, db, measurement, o.conn.timeout)
	if err != nil {
		// A partial import is a real state the user has to reason about,
		// so say exactly where it stopped rather than only what failed.
		if imported > 0 {
			fmt.Fprintf(errOut, "\n%d of %d files were imported into %s.%s before this failure; %d rows landed.\n",
				imported, len(paths), db, measurement, rows)
			fmt.Fprintf(errOut, "Re-running the command re-imports from the first file, which would duplicate those rows.\n")
			fmt.Fprintf(errOut, "To continue instead, import the remaining files by hand from %s\n", dir)
		}
		return err
	}
	fmt.Fprintf(out, "Imported %d files, %d rows into %s.%s\n", imported, rows, db, measurement)

	if o.downloadDir == "" {
		if err := os.RemoveAll(dir); err != nil {
			fmt.Fprintf(errOut, "warning: could not remove %s: %v\n", dir, err)
		}
	} else {
		fmt.Fprintf(out, "Parquet kept in %s\n", dir)
	}

	printSampleQueries(out, id, db, measurement)
	return nil
}

// sampleImportPreflight establishes that the import can work before tens
// of MiB are downloaded: the database exists (creating it if not), and the
// measurement is either empty or the user has agreed to add to it.
func sampleImportPreflight(cmd *cobra.Command, cli *client.Client, db, measurement string, yes bool) error {
	out := cmd.OutOrStdout()

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	list, err := cli.ListDatabases(ctx)
	if err != nil {
		return fmt.Errorf("checking databases: %w", err)
	}
	exists := false
	if list != nil {
		for _, info := range list.Databases {
			if info.Name == db {
				exists = true
				break
			}
		}
	}
	if !exists {
		cctx, ccancel := context.WithTimeout(cmd.Context(), 30*time.Second)
		defer ccancel()
		if _, err := cli.CreateDatabase(cctx, db); err != nil {
			return fmt.Errorf("creating database %q: %w", db, err)
		}
		fmt.Fprintf(out, "Created database %q\n", db)
		return nil
	}

	// The database exists; a populated target measurement means a second
	// run would silently double the row counts the showcase queries
	// report, so make that the user's decision.
	n, err := sampleMeasurementRows(cmd, cli, db, measurement)
	if err != nil || n <= 0 {
		// Counting is best-effort: a measurement that does not exist yet
		// is the common case and must not look like a failure.
		return nil
	}
	fmt.Fprintf(out, "%s.%s already holds %d rows; importing will add to them.\n", db, measurement, n)
	return confirmOrAbort(cmd, "Continue?", yes)
}

// sampleMeasurementRows counts rows in the target measurement. Any error
// means "unknown", not zero: the caller treats it as nothing to warn about.
func sampleMeasurementRows(cmd *cobra.Command, cli *client.Client, db, measurement string) (int64, error) {
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	sql := fmt.Sprintf("SELECT count(*) AS n FROM %s", quoteSQLIdent(measurement))
	qr, err := cli.QueryJSON(ctx, sql, db)
	if err != nil || qr == nil || len(qr.Data) == 0 || len(qr.Data[0]) == 0 {
		return 0, err
	}
	switch n := qr.Data[0][0].(type) {
	case float64:
		return int64(n), nil
	case int64:
		return n, nil
	case json.Number:
		v, convErr := n.Int64()
		if convErr != nil {
			return 0, nil
		}
		return v, nil
	case string:
		v, convErr := strconv.ParseInt(n, 10, 64)
		if convErr != nil {
			return 0, nil
		}
		return v, nil
	}
	return 0, nil
}

// sampleImport imports each file in order, returning how many succeeded
// and how many rows landed even when it stops early.
func sampleImport(cmd *cobra.Command, cli *client.Client, paths []string, db, measurement string, timeout time.Duration) (int, int64, error) {
	errOut := cmd.ErrOrStderr()
	var rows int64
	for i, p := range paths {
		ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
		res, err := cli.ImportParquet(ctx, p, db, measurement, client.ParquetImportOptions{})
		cancel()
		if err != nil {
			fmt.Fprintln(errOut)
			return i, rows, fmt.Errorf("importing %s: %w", filepath.Base(p), err)
		}
		if res != nil {
			rows += res.RowsImported
		}
		fmt.Fprintf(errOut, "\r  imported %d/%d          ", i+1, len(paths))
	}
	fmt.Fprintln(errOut)
	return len(paths), rows, nil
}

// printSampleQueries is the payoff. The data landing is not the point;
// the first query is.
func printSampleQueries(out io.Writer, id, db, measurement string) {
	qs := sampleQueries(id, measurement)
	if len(qs) == 0 {
		return
	}
	fmt.Fprintf(out, "\nTry these:\n")
	for _, q := range qs {
		fmt.Fprintf(out, "\n  # %s\n", q.title)
		fmt.Fprintf(out, "  arcli query --database %s %s\n", shellQuote(db), shellQuote(q.sql))
	}
}

// shellQuote wraps s so it survives a copy-paste into a POSIX shell. The
// SQL contains double quotes around identifiers, so a double-quoted
// argument would end early; single quotes are literal in sh, with the
// usual '\” dance for an embedded one.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n\"'\\$`*?[]{}()<>|&;#~=") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

type sampleQuery struct {
	title string
	sql   string
}

// sampleQueries returns the showcase queries for a dataset.
//
// The citibike pair is written against `time` and the string columns
// only: Arc stores every timestamp column other than `time` as integer
// microseconds, so a query using started_at directly would need a cast
// and would teach the wrong thing.
//
// Every other dataset gets the generic pair. The manifest is designed to
// grow datasets without a CLI change, so a dataset this build has never
// heard of must still end with something worth running rather than
// silently printing nothing.
func sampleQueries(id, measurement string) []sampleQuery {
	m := quoteSQLIdent(measurement)
	switch id {
	case "citibike":
		return []sampleQuery{
			{
				title: "Busiest stations",
				sql: fmt.Sprintf(
					"SELECT start_station_name AS station, count(*) AS trips FROM %s GROUP BY 1 ORDER BY trips DESC LIMIT 5", m),
			},
			{
				title: "Departures per hour, with a 3-hour moving average",
				sql: fmt.Sprintf(
					"WITH hourly AS (SELECT date_trunc('hour', time) AS hr, count(*) AS trips FROM %s GROUP BY 1) "+
						"SELECT hr, trips, round(avg(trips) OVER (ORDER BY hr ROWS BETWEEN 2 PRECEDING AND CURRENT ROW), 1) AS moving_avg_3h "+
						"FROM hourly ORDER BY hr LIMIT 24", m),
			},
		}
	}
	return []sampleQuery{
		{
			title: "First rows",
			sql:   fmt.Sprintf("SELECT * FROM %s ORDER BY time LIMIT 10", m),
		},
		{
			title: "Rows per hour",
			sql: fmt.Sprintf(
				"SELECT date_trunc('hour', time) AS hr, count(*) AS n FROM %s GROUP BY 1 ORDER BY hr LIMIT 24", m),
		},
	}
}

// ---- helpers --------------------------------------------------------------

// quoteSQLIdent makes an identifier safe to interpolate into SQL.
//
// A plain lower-case name is left bare: these queries are printed for the
// user to copy, and "citibike_trips" in quotes reads like an incantation.
// Anything else is double-quoted with embedded quotes doubled, so a
// measurement name from --measurement can never break out of its lane.
func quoteSQLIdent(s string) string {
	if s != "" && !strings.ContainsFunc(s, func(r rune) bool {
		return !(r == '_' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
	}) {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
