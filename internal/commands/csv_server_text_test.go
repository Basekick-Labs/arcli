package commands

import (
	"encoding/csv"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// multiLineErr is a server error with a newline, a tab and an ANSI escape:
// CSV must carry it verbatim (csv.Writer quotes it), the table must strip
// the control characters (#94).
const (
	multiLineErr     = "line one\nline\ttwo \x1b[31mred"
	multiLineErrJSON = `"line one\nline\ttwo \u001b[31mred"`
	multiLineErrTbl  = "line onelinetwo [31mred"
)

func serverTextTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	cq := `{"id":1,"name":"cq","database":"d","source_measurement":"cpu","destination_measurement":"cpu_1m","query":"SELECT 1","interval":"1m","is_active":true}`
	pol := `{"id":1,"name":"p","database":"d","measurement":null,"retention_days":30,"buffer_days":1,"is_active":true}`
	routes := map[string]string{
		"/health":                      `{"status":"ok","time":"t","uptime":"1s"}`,
		"/api/v1/continuous_queries/":  "[" + cq + "]",
		"/api/v1/continuous_queries/1": cq,
		"/api/v1/continuous_queries/1/executions": `{"query_id":1,"executions":[` +
			`{"id":3,"query_id":1,"execution_id":"e3","execution_time":"2026-09-07T19:00:00Z","status":"failed","start_time":"2026-09-07T18:00:00Z","end_time":"2026-09-07T19:00:00Z","records_written":0,"execution_duration_seconds":0,"error_message":` + multiLineErrJSON + `},` +
			`{"id":2,"query_id":1,"execution_id":"e2","execution_time":"2026-09-07T19:00:00Z","status":"completed","start_time":"2026-09-07T18:00:00Z","end_time":"2026-09-07T19:00:00Z","records_written":5,"execution_duration_seconds":0,"error_message":""},` +
			`{"id":1,"query_id":1,"execution_id":"e1","execution_time":"2026-09-07T19:00:00Z","status":"completed","start_time":"2026-09-07T18:00:00Z","end_time":"2026-09-07T19:00:00Z","records_written":5,"execution_duration_seconds":0,"error_message":null}]}`,
		"/api/v1/retention/":  "[" + pol + "]",
		"/api/v1/retention/1": pol,
		"/api/v1/retention/1/executions": `{"policy_id":1,"executions":[` +
			`{"id":3,"policy_id":1,"execution_time":"2026-09-07T19:00:00Z","status":"failed","deleted_count":0,"cutoff_date":null,"execution_duration_ms":2,"error_message":` + multiLineErrJSON + `},` +
			`{"id":2,"policy_id":1,"execution_time":"2026-09-07T19:00:00Z","status":"completed","deleted_count":1,"cutoff_date":null,"execution_duration_ms":2,"error_message":""},` +
			`{"id":1,"policy_id":1,"execution_time":"2026-09-07T19:00:00Z","status":"completed","deleted_count":1,"cutoff_date":null,"execution_duration_ms":2,"error_message":null}]}`,
		"/api/v1/compaction/history": `{"total_jobs":4,"recent_jobs":[` +
			`{"database":"m","measurement":"cpu","partition_path":"p1","tier":"hourly","success":false,"error":` + multiLineErrJSON + `},` +
			`{"database":"m","measurement":"cpu","partition_path":"p2","tier":"hourly"},` +
			`{"database":"m","measurement":"cpu","partition_path":"p3","tier":"hourly","success":true},` +
			`{"database":"m","measurement":"cpu","partition_path":"p4","tier":"hourly","success":false}]}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := routes[r.URL.Path]
		if !ok {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// csvColumn parses out as CSV (header first) and returns the named column.
func csvColumn(t *testing.T, out, column string) []string {
	t.Helper()
	recs, err := csv.NewReader(strings.NewReader(out)).ReadAll()
	if err != nil {
		t.Fatalf("stdout is not valid CSV: %v\n%s", err, out)
	}
	if len(recs) == 0 {
		t.Fatalf("no CSV records:\n%s", out)
	}
	idx := -1
	for i, h := range recs[0] {
		if h == column {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatalf("no %q column in %v", column, recs[0])
	}
	var col []string
	for _, r := range recs[1:] {
		col = append(col, r[idx])
	}
	return col
}

func TestCSV_ServerErrorTextIsVerbatim(t *testing.T) {
	srv := serverTextTestServer(t)
	writeTestConfig(t, srv.URL, "tok")
	cases := []struct {
		name   string
		cmd    func() *cobra.Command
		args   []string
		column string
		want   []string
	}{
		{"cq executions", newCQExecutionsCmd, []string{"1"}, "error", []string{multiLineErr, "-", "-"}},
		{"retention executions", newRetentionExecutionsCmd, []string{"1"}, "error", []string{multiLineErr, "-", "-"}},
		{"compaction history", newCompactionHistoryCmd, nil, "result", []string{"failed: " + multiLineErr, "-", "ok", "failed"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, _, err := execCmd(t, tc.cmd(), append(append([]string{}, tc.args...), "-o", "csv")...)
			if err != nil {
				t.Fatal(err)
			}
			got := csvColumn(t, out, tc.column)
			if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
				t.Errorf("%s column = %q, want %q", tc.column, got, tc.want)
			}

			// The table keeps stripping terminal control characters.
			out, _, err = execCmd(t, tc.cmd(), tc.args...)
			if err != nil {
				t.Fatal(err)
			}
			if strings.ContainsAny(out, "\x1b\t") || !strings.Contains(out, multiLineErrTbl) {
				t.Errorf("table must carry the cleaned error %q:\n%q", multiLineErrTbl, out)
			}
		})
	}
}
