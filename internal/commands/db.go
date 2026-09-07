// db subcommand: manage databases on an Arc cluster.
//
// Maps directly onto Arc's /api/v1/databases endpoints (list / create
// / get / delete + per-database measurement listing). `drop` is a thin
// wrapper around the server-side DELETE which itself requires
// `delete.enabled=true` in arc.toml and admin auth — when those aren't
// met the server's verbatim error surfaces to the user.
package commands

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
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

func newDBCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "db",
		Short: "Manage databases on an Arc cluster",
		Long: `Manage databases on an Arc cluster.

Maps onto Arc's /api/v1/databases endpoints. "drop" requires the server
to have delete.enabled=true in arc.toml AND an admin token; the server's
error surfaces verbatim when those aren't met.`,
	}
	c.AddCommand(
		newDBListCmd(),
		newDBShowCmd(),
		newDBCreateCmd(),
		newDBDropCmd(),
	)
	return c
}

// ---- list -----------------------------------------------------------------

func newDBListCmd() *cobra.Command {
	var (
		connectionName string
		endpoint       string
		token          string
		insecure       bool
		outputFormat   string
		noHeader       bool
		timeout        time.Duration
	)
	c := &cobra.Command{
		Use:   "list",
		Short: "List databases on the active cluster",
		Long: `List databases on the active cluster (GET /api/v1/databases).

Output formats:
  table (default) | json | csv

Each row shows the database name, its measurement count, and (when set
by the server) the creation timestamp.`,
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
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()

			list, err := cli.ListDatabases(ctx)
			if err != nil {
				return err
			}
			return renderDatabaseList(cmd, list, outputFormat, noHeader)
		},
	}
	addCommonConnectionFlags(c, &connectionName, &endpoint, &token, &insecure)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json|csv")
	c.Flags().BoolVar(&noHeader, "no-header", false, "suppress column header row (table + csv)")
	addTimeoutFlag(c, &timeout)
	return c
}

// ---- show -----------------------------------------------------------------

func newDBShowCmd() *cobra.Command {
	var (
		connectionName string
		endpoint       string
		token          string
		insecure       bool
		outputFormat   string
		noHeader       bool
		timeout        time.Duration
	)
	c := &cobra.Command{
		Use:   "show <name>",
		Short: "Show one database plus its measurements",
		Long: `Show one database plus the measurements it contains.

Combines GET /api/v1/databases/:name with
GET /api/v1/databases/:name/measurements so the operator sees
"everything about this database" in one call.

Output formats:
  table (default — two stacked tables, db info then measurements)
  json  (single object: {"database": {...}, "measurements": [...]})
  csv   (measurements only — db metadata is one row, not table-shaped)`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if timeout <= 0 {
				return fmt.Errorf("--timeout must be > 0 (got %s)", timeout)
			}
			if !validListFormat(outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json, csv)", outputFormat)
			}
			name := args[0]
			cli, _, err := buildClient(cmd.ErrOrStderr(), connectionName, endpoint, token, insecure, timeout)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()

			info, err := cli.GetDatabase(ctx, name)
			if err != nil {
				return err
			}
			measurements, err := cli.ListMeasurements(ctx, name)
			if err != nil {
				return err
			}
			return renderDatabaseShow(cmd, info, measurements, outputFormat, noHeader)
		},
	}
	addCommonConnectionFlags(c, &connectionName, &endpoint, &token, &insecure)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json|csv")
	c.Flags().BoolVar(&noHeader, "no-header", false, "suppress column headers (table + csv)")
	addTimeoutFlag(c, &timeout)
	return c
}

// ---- create ---------------------------------------------------------------

func newDBCreateCmd() *cobra.Command {
	var (
		connectionName string
		endpoint       string
		token          string
		insecure       bool
		timeout        time.Duration
	)
	c := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a new empty database",
		Long: `Create a new empty database (POST /api/v1/databases).

Server-side validation:
  - name must start with a letter and contain only alphanumeric,
    underscore, or hyphen characters (max 64)
  - names "system", "internal", "_internal" are reserved
  - 409 Conflict if the database already exists`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if timeout <= 0 {
				return fmt.Errorf("--timeout must be > 0 (got %s)", timeout)
			}
			name := args[0]
			cli, _, err := buildClient(cmd.ErrOrStderr(), connectionName, endpoint, token, insecure, timeout)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()

			info, err := cli.CreateDatabase(ctx, name)
			if err != nil {
				return err
			}
			if info.CreatedAt != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "Created database %q (created_at: %s)\n", info.Name, info.CreatedAt)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "Created database %q\n", info.Name)
			}
			return nil
		},
	}
	addCommonConnectionFlags(c, &connectionName, &endpoint, &token, &insecure)
	addTimeoutFlag(c, &timeout)
	return c
}

// ---- drop -----------------------------------------------------------------

func newDBDropCmd() *cobra.Command {
	var (
		connectionName string
		endpoint       string
		token          string
		insecure       bool
		yes            bool
		timeout        time.Duration
	)
	c := &cobra.Command{
		Use:   "drop <name>",
		Short: "Delete a database and ALL its data",
		Long: `Delete a database and ALL its files (DELETE /api/v1/databases/:name).

Destructive. Prompts for y/N confirmation by default; pass --yes (-y)
to skip the prompt (CI / scripted use).

The server enforces its own layered safety:
  - Requires delete.enabled=true in arc.toml (server returns 403 if not)
  - Requires an admin-permission token
  - Reserved names ("system", "internal", "_internal") are blocked
  - Server enforces ?confirm=true on the request URL; arcli always sends it

When the server refuses, its error message surfaces verbatim — including
the "Set delete.enabled=true in arc.toml to enable" hint when the server
config gate is the reason.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if timeout <= 0 {
				return fmt.Errorf("--timeout must be > 0 (got %s)", timeout)
			}
			name := args[0]
			// Build the client BEFORE the confirmation prompt so a
			// misconfigured connection fails fast instead of making
			// the user say "yes" to a delete that can't be sent.
			// (Gemini PR #2 finding — better UX, no functional change.)
			cli, _, err := buildClient(cmd.ErrOrStderr(), connectionName, endpoint, token, insecure, timeout)
			if err != nil {
				return err
			}
			if !yes {
				if !confirmDestructive(cmd, fmt.Sprintf("Delete database %q and ALL its files?", name)) {
					fmt.Fprintln(cmd.ErrOrStderr(), "Aborted.")
					return nil
				}
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()

			if err := cli.DeleteDatabase(ctx, name); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted database %q\n", name)
			return nil
		},
	}
	addCommonConnectionFlags(c, &connectionName, &endpoint, &token, &insecure)
	c.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt (destructive!)")
	addTimeoutFlag(c, &timeout)
	return c
}

// confirmDestructive reads one line from cmd.InOrStdin() and returns
// true only if the user typed "y" or "yes" (case-insensitive). Reading
// from cmd.InOrStdin() (not os.Stdin) means tests can drive the prompt
// via cmd.SetIn(strings.NewReader("y\n")).
//
// Anything else — empty input, "n", EOF, read error — returns false.
// That's the safe default for a destructive prompt: when in doubt,
// don't delete.
//
// The prompt is written to stderr so stdout stays clean for scripts
// that capture it.
func confirmDestructive(cmd *cobra.Command, question string) bool {
	fmt.Fprintf(cmd.ErrOrStderr(), "%s [y/N] ", question)
	r := bufio.NewReader(cmd.InOrStdin())
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	resp := strings.TrimSpace(line)
	return strings.EqualFold(resp, "y") || strings.EqualFold(resp, "yes")
}

// ---- helpers --------------------------------------------------------------

// addCommonConnectionFlags registers the shared `-c/--connection`,
// `--endpoint`, `--token`, `--insecure` flags on a cobra command. These
// behave identically to the same flags on `query` and `write`.
func addCommonConnectionFlags(c *cobra.Command, connectionName, endpoint, token *string, insecure *bool) {
	c.Flags().StringVarP(connectionName, "connection", "c", "", "named connection (overrides active)")
	c.Flags().StringVar(endpoint, "endpoint", "", "ad-hoc Arc endpoint URL")
	c.Flags().StringVar(token, "token", "", "ad-hoc bearer token")
	c.Flags().BoolVar(insecure, "insecure", false, "skip TLS certificate verification (warns to stderr)")
}

// addTimeoutFlag registers the per-request HTTP `--timeout` flag with
// the project-wide default (60s). Factored so the flag's wording and
// default stay consistent across every command.
func addTimeoutFlag(c *cobra.Command, timeout *time.Duration) {
	addTimeoutFlagDefault(c, timeout, 60*time.Second)
}

// validListFormat reports whether the format is one of the three
// supported by list / show commands (table, json, csv — no arrow, since
// these endpoints return JSON not Arrow IPC).
func validListFormat(s string) bool {
	switch s {
	case output.FormatTable, output.FormatJSON, output.FormatCSV:
		return true
	}
	return false
}

// renderDatabaseList writes the list-databases response in the chosen
// format. Databases are sorted by name so screenshots / tests are
// stable regardless of server iteration order. JSON sorts too so the
// three formats agree on row order.
func renderDatabaseList(cmd *cobra.Command, list *client.DatabaseListResponse, format string, noHeader bool) error {
	// Defensive copy: we mustn't mutate the caller's slice when we
	// sort. Use make+copy (not append-to-nil) so that an empty
	// list.Databases produces an empty-but-non-nil slice; the
	// difference matters for JSON output where nil encodes as
	// `null` and []T{} encodes as `[]`. (Gemini PR #2 finding.)
	dbs := make([]client.DatabaseInfo, len(list.Databases))
	copy(dbs, list.Databases)
	sort.Slice(dbs, func(i, j int) bool { return dbs[i].Name < dbs[j].Name })

	switch format {
	case output.FormatJSON:
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		// Re-wrap with the sorted slice so JSON output matches
		// table+CSV row order. Preserve the server-reported Count
		// rather than re-deriving it from len(dbs) so any future
		// server-side pagination surface still round-trips correctly.
		return enc.Encode(client.DatabaseListResponse{Databases: dbs, Count: list.Count})
	case output.FormatCSV:
		return writeDBListCSV(cmd.OutOrStdout(), dbs, noHeader)
	default: // FormatTable
		if len(dbs) == 0 {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), "(no databases)")
			return err
		}
		rows := make([][]string, 0, len(dbs))
		for _, d := range dbs {
			rows = append(rows, []string{
				d.Name,
				strconv.Itoa(d.MeasurementCount),
				d.CreatedAt,
			})
		}
		headers := []string{"NAME", "MEASUREMENTS", "CREATED_AT"}
		if noHeader {
			headers = nil
		}
		return output.Table(cmd.OutOrStdout(), headers, rows)
	}
}

func writeDBListCSV(w io.Writer, dbs []client.DatabaseInfo, noHeader bool) error {
	cw := csv.NewWriter(w)
	if !noHeader {
		if err := cw.Write([]string{"name", "measurement_count", "created_at"}); err != nil {
			return err
		}
	}
	for _, d := range dbs {
		if err := cw.Write([]string{d.Name, strconv.Itoa(d.MeasurementCount), d.CreatedAt}); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// renderDatabaseShow combines the database info row with its
// measurements list. JSON output is a single composed object; table
// output stacks two visual blocks; CSV emits only measurements (the
// db-info block isn't tabular-shaped).
func renderDatabaseShow(cmd *cobra.Command, info *client.DatabaseInfo, list *client.MeasurementListResponse, format string, noHeader bool) error {
	// make+copy (not append-to-nil) so an empty server response
	// encodes as `"measurements": []` rather than `"measurements":
	// null` in JSON output. (Gemini PR #2 finding.)
	measurements := make([]client.DatabaseMeasurement, len(list.Measurements))
	copy(measurements, list.Measurements)
	sort.Slice(measurements, func(i, j int) bool { return measurements[i].Name < measurements[j].Name })

	switch format {
	case output.FormatJSON:
		// Compose db info + measurements into one object so a single
		// JSON read covers the whole view. `Count` mirrors the server's
		// reported value (list.Count) rather than len(measurements),
		// matching MeasurementListResponse semantics.
		composed := struct {
			Database     *client.DatabaseInfo         `json:"database"`
			Measurements []client.DatabaseMeasurement `json:"measurements"`
			Count        int                          `json:"count"`
		}{
			Database:     info,
			Measurements: measurements,
			Count:        list.Count,
		}
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(composed)

	case output.FormatCSV:
		return writeMeasurementsCSV(cmd.OutOrStdout(), measurements, noHeader)

	default: // FormatTable
		w := cmd.OutOrStdout()
		fmt.Fprintf(w, "Database: %s\n", info.Name)
		fmt.Fprintf(w, "Measurements: %d\n", info.MeasurementCount)
		if info.CreatedAt != "" {
			fmt.Fprintf(w, "Created at: %s\n", info.CreatedAt)
		}
		fmt.Fprintln(w)
		if len(measurements) == 0 {
			fmt.Fprintln(w, "(no measurements yet)")
			return nil
		}
		rows := make([][]string, 0, len(measurements))
		for _, m := range measurements {
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

func writeMeasurementsCSV(w io.Writer, ms []client.DatabaseMeasurement, noHeader bool) error {
	cw := csv.NewWriter(w)
	if !noHeader {
		if err := cw.Write([]string{"name", "file_count"}); err != nil {
			return err
		}
	}
	for _, m := range ms {
		fc := ""
		if m.FileCount > 0 {
			fc = strconv.Itoa(m.FileCount)
		}
		if err := cw.Write([]string{m.Name, fc}); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}
