package commands

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/basekick-labs/arcli/internal/client"
)

func newImportTestCmd() *cobra.Command {
	return &cobra.Command{Use: "test"}
}

// TestImportCSV_MarkFlagRequired_File pins the cobra MarkFlagRequired
// behavior for --file (added in Gemini PR #3 r5). Without the flag,
// cobra should error before RunE with the canonical
// `required flag(s) "file" not set` message.
func TestImportCSV_MarkFlagRequired_File(t *testing.T) {
	cmd := newImportCSVCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	// Pass --measurement so the only missing required flag is --file.
	cmd.SetArgs([]string{"--measurement", "m", "--endpoint", "http://127.0.0.1:1", "--token", "t"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), `required flag(s) "file" not set`) {
		t.Errorf("error %q does not mention required --file flag", err)
	}
}

// TestImportCSV_MarkFlagRequired_Measurement is the same check for
// --measurement, which is required for CSV (the file doesn't carry a
// measurement name).
func TestImportCSV_MarkFlagRequired_Measurement(t *testing.T) {
	cmd := newImportCSVCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--file", "/tmp/whatever.csv", "--endpoint", "http://127.0.0.1:1", "--token", "t"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), `required flag(s) "measurement" not set`) {
		t.Errorf("error %q does not mention required --measurement flag", err)
	}
}

// TestImportLP_MarkFlagRequired_File — LP does NOT require
// --measurement (measurement is self-declared in line syntax), so only
// --file should be marked required.
func TestImportLP_MarkFlagRequired_File(t *testing.T) {
	cmd := newImportLPCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--endpoint", "http://127.0.0.1:1", "--token", "t"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), `required flag(s) "file" not set`) {
		t.Errorf("error %q does not mention required --file flag", err)
	}
	// Confirm --measurement is NOT marked required for LP.
	if strings.Contains(err.Error(), "measurement") {
		t.Errorf("error %q unexpectedly mentions --measurement (should be optional for LP)", err)
	}
}

// TestImportCSV_RejectsNegativeSkipRows pins the client-side validation
// Gemini flagged in arcli PR #3: a negative --skip-rows used to pass
// silently into the client (which drops it via `> 0`), so the user got
// no error AND no skip. The RunE-level guard now fails fast.
//
// Driven through the cobra command rather than calling RunE directly so
// the test covers the wired flag path end-to-end.
func TestImportCSV_RejectsNegativeSkipRows(t *testing.T) {
	cmd := newImportCSVCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{
		"--file", "/tmp/whatever",
		"--measurement", "m",
		"--endpoint", "http://127.0.0.1:1",
		"--token", "t",
		"--skip-rows", "-5",
	})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "--skip-rows must be >= 0") {
		t.Errorf("error %q does not mention --skip-rows", err)
	}
}

func TestValidImportOutputFormat(t *testing.T) {
	for _, f := range []string{"table", "json"} {
		if !validImportOutputFormat(f) {
			t.Errorf("validImportOutputFormat(%q) = false", f)
		}
	}
	for _, f := range []string{"csv", "arrow", "", "YAML"} {
		if validImportOutputFormat(f) {
			t.Errorf("validImportOutputFormat(%q) = true (should be false — import endpoints return one-shot results, not tabular data)", f)
		}
	}
}

func TestRenderImportResult_TableHappyPath(t *testing.T) {
	var buf bytes.Buffer
	cmd := newImportTestCmd()
	cmd.SetOut(&buf)
	r := &client.ImportResult{
		Database: "metrics", Measurement: "cpu",
		RowsImported: 42, PartitionsCreated: 1,
		TimeRangeMin: "2026-01-01", TimeRangeMax: "2026-01-02",
		Columns: []string{"time", "host", "value"}, DurationMs: 17,
	}
	if err := renderImportResult(cmd, r, "table"); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"OK", "metrics", "cpu", "42", "time, host, value", "17"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestRenderImportResult_TableNoTimeRangeNoColumns(t *testing.T) {
	// Server-side error path: import succeeded but server only filled
	// in the basics. The render must not print empty bracketed time
	// ranges or empty "columns: " lines.
	var buf bytes.Buffer
	cmd := newImportTestCmd()
	cmd.SetOut(&buf)
	r := &client.ImportResult{Database: "x", Measurement: "y", RowsImported: 1}
	if err := renderImportResult(cmd, r, "table"); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "time_range:") {
		t.Errorf("empty time-range should not render: %s", out)
	}
	if strings.Contains(out, "columns:") {
		t.Errorf("empty columns should not render: %s", out)
	}
}

// Regression for the PR3-style nil-slice JSON encoding issue.
// `ImportResult.Columns` is a []string without `omitempty`; if the
// server returns null (or arcli decodes the field as nil for any
// reason), JSON output must STILL emit `"columns": []` so downstream
// consumers don't see `null`.
func TestRenderImportResult_JSONEmptyColumns_IsArrayNotNull(t *testing.T) {
	var buf bytes.Buffer
	cmd := newImportTestCmd()
	cmd.SetOut(&buf)
	r := &client.ImportResult{
		Database: "x", Measurement: "y", RowsImported: 0,
		Columns: nil, // server returned no columns
	}
	if err := renderImportResult(cmd, r, "json"); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "null") {
		t.Errorf("JSON output contains `null`. Got:\n%s", out)
	}
	if !strings.Contains(out, `"columns": []`) {
		t.Errorf("expected `\"columns\": []`, got:\n%s", out)
	}
}

func TestRenderLPImportResult_TableHappyPath(t *testing.T) {
	var buf bytes.Buffer
	cmd := newImportTestCmd()
	cmd.SetOut(&buf)
	r := &client.LPImportResult{
		Database: "metrics", Measurements: []string{"cpu", "mem"},
		RowsImported: 100, Precision: "ms", DurationMs: 8,
	}
	if err := renderLPImportResult(cmd, r, "table"); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"OK", "metrics", "cpu, mem", "100", "ms"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestRenderLPImportResult_JSONEmptyMeasurements_IsArrayNotNull(t *testing.T) {
	var buf bytes.Buffer
	cmd := newImportTestCmd()
	cmd.SetOut(&buf)
	r := &client.LPImportResult{
		Database: "x", Measurements: nil, RowsImported: 0,
	}
	if err := renderLPImportResult(cmd, r, "json"); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "null") {
		t.Errorf("JSON output contains `null`. Got:\n%s", out)
	}
	if !strings.Contains(out, `"measurements": []`) {
		t.Errorf("expected `\"measurements\": []`, got:\n%s", out)
	}
}

func TestRenderTLEImportResult_WithWarnings(t *testing.T) {
	var buf bytes.Buffer
	cmd := newImportTestCmd()
	cmd.SetOut(&buf)
	r := &client.TLEImportResult{
		Database: "sats", Measurement: "satellite_tle",
		SatelliteCount: 2, RowsImported: 2, DurationMs: 5,
		ParseWarnings: []string{"entry 1 line 1 checksum mismatch"},
	}
	if err := renderTLEImportResult(cmd, r, "table"); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "parse_warnings (1):") {
		t.Errorf("expected parse_warnings header, got:\n%s", out)
	}
	if !strings.Contains(out, "entry 1 line 1 checksum mismatch") {
		t.Errorf("expected warning line, got:\n%s", out)
	}
}

func TestRenderTLEImportResult_NoWarningsBlock(t *testing.T) {
	// TLE's ParseWarnings uses omitempty on the server, so a nil
	// slice means "no warnings" — table output must NOT print an
	// empty warnings block.
	var buf bytes.Buffer
	cmd := newImportTestCmd()
	cmd.SetOut(&buf)
	r := &client.TLEImportResult{
		Database: "sats", Measurement: "satellite_tle",
		SatelliteCount: 5, RowsImported: 5,
	}
	if err := renderTLEImportResult(cmd, r, "table"); err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(buf.String(), "parse_warnings") {
		t.Errorf("empty warnings should not render: %s", buf.String())
	}
}
