// Render strategies for query results.
//
// Three formats covered in PR2: table (default, pretty-printed),
// json (raw API shape, jq-friendly), csv (RFC 4180 with header row).
// Arrow IPC is a separate code path in cmd/query.go because it
// streams from the server rather than holding a decoded QueryResult.
package output

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strconv"

	"github.com/basekick-labs/arcli/internal/client"
)

// Format names; the value of `-o/--output`.
const (
	FormatTable = "table"
	FormatJSON  = "json"
	FormatCSV   = "csv"
	FormatArrow = "arrow"
)

// ValidFormat reports whether s is one of the four supported formats.
func ValidFormat(s string) bool {
	switch s {
	case FormatTable, FormatJSON, FormatCSV, FormatArrow:
		return true
	}
	return false
}

// RenderQueryResult writes a decoded QueryResult to w in the chosen
// format. `format` must be FormatTable, FormatJSON, or FormatCSV;
// FormatArrow is streamed from the server and does not flow through
// here (the command layer handles it directly).
//
// `noHeader` suppresses column headers for table + csv. Ignored by json.
// `limit` caps the number of rows written (0 = no cap).
func RenderQueryResult(w io.Writer, qr *client.QueryResult, format string, noHeader bool, limit int) error {
	if qr == nil {
		return fmt.Errorf("nil query result")
	}
	switch format {
	case "", FormatTable:
		return renderTable(w, qr, noHeader, limit)
	case FormatJSON:
		return renderJSON(w, qr)
	case FormatCSV:
		return renderCSV(w, qr, noHeader, limit)
	case FormatArrow:
		return fmt.Errorf("arrow format is streamed directly from the server; pass --output arrow to `arcli query` for binary IPC on stdout")
	}
	return fmt.Errorf("unknown output format %q (valid: table, json, csv, arrow)", format)
}

func renderTable(w io.Writer, qr *client.QueryResult, noHeader bool, limit int) error {
	// Arc returns `columns:[] data:[]` for a query that hit no files
	// (e.g. SELECT * FROM <unknown_measurement>). Without this branch
	// tablewriter would emit zero bytes, leaving the operator wondering
	// whether the query even ran. Mirror what psql / clickhouse-client
	// do for the empty case.
	if len(qr.Columns) == 0 {
		_, err := fmt.Fprintln(w, "(0 rows)")
		return err
	}
	rows := buildRows(qr, limit)
	if noHeader {
		return Table(w, nil, rows)
	}
	return Table(w, qr.Columns, rows)
}

func renderJSON(w io.Writer, qr *client.QueryResult) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(qr)
}

func renderCSV(w io.Writer, qr *client.QueryResult, noHeader bool, limit int) error {
	cw := csv.NewWriter(w)
	if !noHeader {
		if err := cw.Write(qr.Columns); err != nil {
			return err
		}
	}
	rowCount := qr.RowCount
	if limit > 0 && limit < rowCount {
		rowCount = limit
	}
	for i := 0; i < rowCount; i++ {
		row := qr.RowAt(i)
		strRow := make([]string, len(row))
		for j, v := range row {
			strRow[j] = formatCell(v)
		}
		if err := cw.Write(strRow); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// buildRows converts the row-major QueryResult.Data into a slice of
// stringified rows suitable for tablewriter. The transformation is
// per-cell only (formatCell); the outer shape passes through.
func buildRows(qr *client.QueryResult, limit int) [][]string {
	rowCount := qr.RowCount
	if limit > 0 && limit < rowCount {
		rowCount = limit
	}
	rows := make([][]string, 0, rowCount)
	for i := 0; i < rowCount; i++ {
		row := qr.RowAt(i)
		strRow := make([]string, len(row))
		for j, v := range row {
			strRow[j] = formatCell(v)
		}
		rows = append(rows, strRow)
	}
	return rows
}

// formatCell converts a cell value (from JSON-decoded `any`) into a
// terminal-safe string. Special-cases the common types so we don't
// fall through to "%v" for every cell (which formats float64 as
// "1.23e+09" and slices/maps awkwardly).
func formatCell(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		// JSON numbers decode as float64. Integers up to 2^53 round-trip
		// losslessly; print whole numbers without ".000000" tail.
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	case json.Number:
		return string(x)
	}
	// Slices, maps, anything else — fall back to compact JSON so the
	// cell is at least machine-parseable in csv output.
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}
