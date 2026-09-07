package output

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"strings"
	"testing"

	"github.com/basekick-labs/arcli/internal/client"
)

func sampleQR() *client.QueryResult {
	// Row-major: Data[i] = row i's cells in Columns order.
	return &client.QueryResult{
		Columns: []string{"host", "value", "ok"},
		Data: [][]any{
			{"server-1", 42.5, true},
			{"server-2", 100.0, false},
		},
		RowCount:        2,
		ExecutionTimeMs: 1.5,
	}
}

func TestValidFormat(t *testing.T) {
	for _, f := range []string{"table", "json", "csv", "arrow"} {
		if !ValidFormat(f) {
			t.Errorf("ValidFormat(%q) = false", f)
		}
	}
	for _, f := range []string{"", "yaml", "xml", "ARROW"} {
		if ValidFormat(f) {
			t.Errorf("ValidFormat(%q) = true", f)
		}
	}
}

func TestRenderQueryResult_JSON(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderQueryResult(&buf, sampleQR(), FormatJSON, false, 0); err != nil {
		t.Fatalf("Render: %v", err)
	}
	// Round-trip back to confirm we emitted valid JSON with the
	// same shape as the input.
	var got client.QueryResult
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	if got.RowCount != 2 || len(got.Columns) != 3 {
		t.Errorf("round-trip shape wrong: %+v", got)
	}
}

func TestRenderQueryResult_CSV(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderQueryResult(&buf, sampleQR(), FormatCSV, false, 0); err != nil {
		t.Fatalf("Render: %v", err)
	}
	r := csv.NewReader(&buf)
	records, err := r.ReadAll()
	if err != nil {
		t.Fatalf("output is not valid CSV: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("got %d records, want 3 (header + 2 rows)", len(records))
	}
	if records[0][0] != "host" {
		t.Errorf("header row = %v", records[0])
	}
	if records[1][0] != "server-1" || records[1][1] != "42.5" {
		t.Errorf("row 1 = %v", records[1])
	}
	if records[2][1] != "100" {
		t.Errorf("row 2 value (whole-float should print as int): %v", records[2])
	}
	if records[2][2] != "false" {
		t.Errorf("row 2 = %v", records[2])
	}
}

func TestRenderQueryResult_CSV_NoHeader(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderQueryResult(&buf, sampleQR(), FormatCSV, true, 0); err != nil {
		t.Fatalf("Render: %v", err)
	}
	r := csv.NewReader(&buf)
	records, err := r.ReadAll()
	if err != nil {
		t.Fatalf("invalid CSV: %v", err)
	}
	if len(records) != 2 {
		t.Errorf("expected 2 rows (no header), got %d: %v", len(records), records)
	}
}

func TestRenderQueryResult_TableEmptyResult(t *testing.T) {
	// Arc's "no files found" response — empty columns AND empty data.
	// Must print SOMETHING so the operator knows the query ran.
	var buf bytes.Buffer
	empty := &client.QueryResult{Columns: nil, Data: nil, RowCount: 0}
	if err := RenderQueryResult(&buf, empty, FormatTable, false, 0); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(buf.String(), "(0 rows)") {
		t.Errorf("empty table output = %q, want '(0 rows)'", buf.String())
	}
}

func TestRenderQueryResult_TableHasHeaders(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderQueryResult(&buf, sampleQR(), FormatTable, false, 0); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"HOST", "VALUE", "OK", "server-1", "42", "true"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q\n%s", want, out)
		}
	}
}

func TestRenderQueryResult_LimitCaps(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderQueryResult(&buf, sampleQR(), FormatCSV, false, 1); err != nil {
		t.Fatalf("Render: %v", err)
	}
	r := csv.NewReader(&buf)
	records, _ := r.ReadAll()
	// header + 1 row = 2 records
	if len(records) != 2 {
		t.Errorf("expected 2 records (header + 1 row), got %d", len(records))
	}
}

func TestRenderQueryResult_ArrowRejected(t *testing.T) {
	var buf bytes.Buffer
	err := RenderQueryResult(&buf, sampleQR(), FormatArrow, false, 0)
	if err == nil || !strings.Contains(err.Error(), "arrow format is streamed") {
		t.Errorf("expected arrow-streamed-elsewhere error, got %v", err)
	}
}

func TestRenderQueryResult_UnknownFormat(t *testing.T) {
	var buf bytes.Buffer
	err := RenderQueryResult(&buf, sampleQR(), "xml", false, 0)
	if err == nil || !strings.Contains(err.Error(), "unknown output format") {
		t.Errorf("expected unknown-format error, got %v", err)
	}
}

func TestFormatCell(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{nil, ""},
		{"hello", "hello"},
		{true, "true"},
		{false, "false"},
		{42.0, "42"},
		{42.5, "42.5"},
		{json.Number("999999999999999"), "999999999999999"},
		{[]any{1, 2}, "[1,2]"},
	}
	for _, c := range cases {
		got := formatCell(c.in)
		if got != c.want {
			t.Errorf("formatCell(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
