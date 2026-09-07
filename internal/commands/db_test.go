package commands

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/basekick-labs/arcli/internal/client"
)

func TestValidListFormat(t *testing.T) {
	for _, f := range []string{"table", "json", "csv"} {
		if !validListFormat(f) {
			t.Errorf("validListFormat(%q) = false", f)
		}
	}
	for _, f := range []string{"arrow", "", "yaml", "ARROW"} {
		if validListFormat(f) {
			t.Errorf("validListFormat(%q) = true (should be false — arrow not valid for list endpoints)", f)
		}
	}
}

func newTestCmd() *cobra.Command {
	// Bare cobra.Command is enough — renderDatabaseList only uses
	// cmd.OutOrStdout() which works on a default-constructed command.
	return &cobra.Command{Use: "test"}
}

func TestRenderDatabaseList_TableEmpty(t *testing.T) {
	// Server returned databases:[] / count:0. Must print SOMETHING.
	var buf bytes.Buffer
	cmd := newTestCmd()
	cmd.SetOut(&buf)
	list := &client.DatabaseListResponse{Databases: nil, Count: 0}
	if err := renderDatabaseList(cmd, list, "table", false); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(buf.String(), "(no databases)") {
		t.Errorf("empty render = %q, want '(no databases)'", buf.String())
	}
}

// Regression for Gemini PR #2 finding: when the server returns no
// databases, JSON output MUST be `"databases": []`, not `"databases":
// null`. Many JSON consumers (jq filters, JSON-schema validators with
// `minItems: 0`, frontends doing `.map()`) treat null and [] as
// semantically distinct — null is "unknown/missing", [] is "known
// empty." Catching this in a test pins the make+copy idiom inside
// renderDatabaseList so a future refactor back to append-to-nil
// fails CI loudly.
func TestRenderDatabaseList_JSONEmpty_IsArrayNotNull(t *testing.T) {
	var buf bytes.Buffer
	cmd := newTestCmd()
	cmd.SetOut(&buf)
	list := &client.DatabaseListResponse{Databases: nil, Count: 0}
	if err := renderDatabaseList(cmd, list, "json", false); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "null") {
		t.Errorf("JSON output contains `null` — empty slice must encode as `[]`. Got:\n%s", out)
	}
	// Positive form: confirm the bracketed-empty shape is actually present.
	if !strings.Contains(out, `"databases": []`) {
		t.Errorf("expected `\"databases\": []` in output, got:\n%s", out)
	}
}

func TestRenderDatabaseShow_JSONEmpty_IsArrayNotNull(t *testing.T) {
	var buf bytes.Buffer
	cmd := newTestCmd()
	cmd.SetOut(&buf)
	info := &client.DatabaseInfo{Name: "empty_db", MeasurementCount: 0}
	list := &client.MeasurementListResponse{Database: "empty_db", Measurements: nil, Count: 0}
	if err := renderDatabaseShow(cmd, info, list, "json", false); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "null") {
		t.Errorf("JSON output contains `null` — empty measurements slice must encode as `[]`. Got:\n%s", out)
	}
	if !strings.Contains(out, `"measurements": []`) {
		t.Errorf("expected `\"measurements\": []` in output, got:\n%s", out)
	}
}

func TestRenderMeasurementList_JSONEmpty_IsArrayNotNull(t *testing.T) {
	var buf bytes.Buffer
	cmd := newTestCmd()
	cmd.SetOut(&buf)
	list := &client.MeasurementListResponse{Database: "metrics", Measurements: nil, Count: 0}
	if err := renderMeasurementList(cmd, list, "json", false); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "null") {
		t.Errorf("JSON output contains `null` — empty measurements slice must encode as `[]`. Got:\n%s", out)
	}
	if !strings.Contains(out, `"measurements": []`) {
		t.Errorf("expected `\"measurements\": []` in output, got:\n%s", out)
	}
}

func TestRenderDatabaseList_TableSortsByName(t *testing.T) {
	var buf bytes.Buffer
	cmd := newTestCmd()
	cmd.SetOut(&buf)
	list := &client.DatabaseListResponse{
		Databases: []client.DatabaseInfo{
			{Name: "zeta", MeasurementCount: 1},
			{Name: "alpha", MeasurementCount: 3, CreatedAt: "2026-01-01"},
			{Name: "mike", MeasurementCount: 2},
		},
		Count: 3,
	}
	if err := renderDatabaseList(cmd, list, "table", false); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	// Confirm alpha appears before mike before zeta in the output.
	if !(strings.Index(out, "alpha") < strings.Index(out, "mike") && strings.Index(out, "mike") < strings.Index(out, "zeta")) {
		t.Errorf("rows not sorted by name:\n%s", out)
	}
}

func TestRenderDatabaseList_JSON_RoundTrips(t *testing.T) {
	var buf bytes.Buffer
	cmd := newTestCmd()
	cmd.SetOut(&buf)
	src := &client.DatabaseListResponse{
		Databases: []client.DatabaseInfo{{Name: "m", MeasurementCount: 1}},
		Count:     1,
	}
	if err := renderDatabaseList(cmd, src, "json", false); err != nil {
		t.Fatalf("render: %v", err)
	}
	var decoded client.DatabaseListResponse
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, buf.String())
	}
	if decoded.Count != 1 || decoded.Databases[0].Name != "m" {
		t.Errorf("decoded = %+v", decoded)
	}
}

func TestRenderDatabaseList_CSV_ShapeAndOrdering(t *testing.T) {
	var buf bytes.Buffer
	cmd := newTestCmd()
	cmd.SetOut(&buf)
	src := &client.DatabaseListResponse{
		Databases: []client.DatabaseInfo{
			{Name: "b", MeasurementCount: 2, CreatedAt: "2026-02"},
			{Name: "a", MeasurementCount: 1},
		},
		Count: 2,
	}
	if err := renderDatabaseList(cmd, src, "csv", false); err != nil {
		t.Fatalf("render: %v", err)
	}
	rows, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatalf("invalid CSV: %v", err)
	}
	// header + 2 rows; sorted alphabetically so "a" comes first
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	if rows[0][0] != "name" {
		t.Errorf("header row = %v", rows[0])
	}
	if rows[1][0] != "a" || rows[2][0] != "b" {
		t.Errorf("rows not alphabetical: %v / %v", rows[1], rows[2])
	}
	// "a" has no CreatedAt → empty cell
	if rows[1][2] != "" {
		t.Errorf("expected empty CreatedAt for 'a', got %q", rows[1][2])
	}
}

func TestRenderDatabaseShow_TableEmptyMeasurements(t *testing.T) {
	var buf bytes.Buffer
	cmd := newTestCmd()
	cmd.SetOut(&buf)
	info := &client.DatabaseInfo{Name: "empty_db", MeasurementCount: 0}
	list := &client.MeasurementListResponse{Database: "empty_db", Measurements: nil, Count: 0}
	if err := renderDatabaseShow(cmd, info, list, "table", false); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Database: empty_db") {
		t.Errorf("missing header: %s", out)
	}
	if !strings.Contains(out, "(no measurements yet)") {
		t.Errorf("missing empty hint: %s", out)
	}
}

func TestRenderDatabaseShow_JSONComposesBothPayloads(t *testing.T) {
	var buf bytes.Buffer
	cmd := newTestCmd()
	cmd.SetOut(&buf)
	info := &client.DatabaseInfo{Name: "metrics", MeasurementCount: 2}
	list := &client.MeasurementListResponse{
		Database:     "metrics",
		Measurements: []client.DatabaseMeasurement{{Name: "cpu"}, {Name: "mem", FileCount: 5}},
		Count:        2,
	}
	if err := renderDatabaseShow(cmd, info, list, "json", false); err != nil {
		t.Fatalf("render: %v", err)
	}
	var got struct {
		Database     client.DatabaseInfo          `json:"database"`
		Measurements []client.DatabaseMeasurement `json:"measurements"`
		Count        int                          `json:"count"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, buf.String())
	}
	if got.Database.Name != "metrics" {
		t.Errorf("db.name = %q", got.Database.Name)
	}
	if got.Count != 2 || len(got.Measurements) != 2 {
		t.Errorf("decoded measurements = %+v", got)
	}
}

func TestRenderMeasurementList_CSVIncludesDatabaseColumn(t *testing.T) {
	var buf bytes.Buffer
	cmd := newTestCmd()
	cmd.SetOut(&buf)
	list := &client.MeasurementListResponse{
		Database:     "metrics",
		Measurements: []client.DatabaseMeasurement{{Name: "cpu", FileCount: 3}, {Name: "mem"}},
		Count:        2,
	}
	if err := renderMeasurementList(cmd, list, "csv", false); err != nil {
		t.Fatalf("render: %v", err)
	}
	rows, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatalf("invalid CSV: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	// Header row should be database,measurement,file_count
	if rows[0][0] != "database" || rows[0][1] != "measurement" || rows[0][2] != "file_count" {
		t.Errorf("header = %v", rows[0])
	}
	// Every data row should have the database value in col 0.
	if rows[1][0] != "metrics" || rows[2][0] != "metrics" {
		t.Errorf("database column missing in data rows: %v / %v", rows[1], rows[2])
	}
	// FileCount=0 should render as empty (matches the omitempty wire convention).
	if rows[2][2] != "" {
		t.Errorf("expected empty FileCount for 'mem', got %q", rows[2][2])
	}
}

func TestRenderMeasurementList_TableEmpty(t *testing.T) {
	var buf bytes.Buffer
	cmd := newTestCmd()
	cmd.SetOut(&buf)
	list := &client.MeasurementListResponse{Database: "empty_db", Measurements: nil}
	if err := renderMeasurementList(cmd, list, "table", false); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(buf.String(), "(no measurements in database \"empty_db\")") {
		t.Errorf("empty render = %q", buf.String())
	}
}

// confirmDestructive tests — the prompt is the only client-side gate
// on db drop, so it has to be testable without spinning up an arc.

func TestConfirmDestructive_AcceptsY(t *testing.T) {
	cmd := newTestCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetIn(strings.NewReader("y\n"))
	if !confirmDestructive(cmd, "Delete X?") {
		t.Error("y should accept")
	}
	// The prompt goes to stderr (PR5) so stdout stays clean for
	// scripts capturing a command's result.
	if !strings.Contains(errOut.String(), "Delete X? [y/N]") {
		t.Errorf("prompt missing from stderr: %q", errOut.String())
	}
	if out.Len() != 0 {
		t.Errorf("prompt leaked to stdout: %q", out.String())
	}
}

func TestConfirmDestructive_AcceptsYes(t *testing.T) {
	cmd := newTestCmd()
	cmd.SetIn(strings.NewReader("YES\n"))
	cmd.SetOut(io.Discard)
	if !confirmDestructive(cmd, "Delete X?") {
		t.Error("YES should accept (case-insensitive)")
	}
}

func TestConfirmDestructive_DefaultsNo(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"empty newline", "\n"},
		{"n", "n\n"},
		{"NO", "NO\n"},
		{"random", "delete it\n"},
		{"EOF no newline", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newTestCmd()
			cmd.SetIn(strings.NewReader(tc.input))
			cmd.SetOut(io.Discard)
			if confirmDestructive(cmd, "Delete X?") {
				t.Errorf("input %q should NOT confirm", tc.input)
			}
		})
	}
}

func TestConfirmDestructive_TrimsWhitespace(t *testing.T) {
	cmd := newTestCmd()
	cmd.SetIn(strings.NewReader("  y  \n"))
	cmd.SetOut(io.Discard)
	if !confirmDestructive(cmd, "Delete X?") {
		t.Error("'  y  ' (whitespace-padded) should accept")
	}
}
