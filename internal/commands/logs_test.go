package commands

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fakeOpsServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/logs", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(403)
			_, _ = w.Write([]byte(`{"success":false,"error":"Permission denied: admin required"}`))
			return
		}
		if r.URL.Query().Get("level") == "fatal" {
			_, _ = w.Write([]byte(`{"count":0,"level_filter":"fatal","limit":100,"logs":null,"since_minutes":60,"timestamp":"t"}`))
			return
		}
		_, _ = w.Write([]byte(`{"count":2,"level_filter":"","limit":100,"logs":[{"timestamp":"2026-09-07T20:42:09Z","level":"ERROR","component":"query","message":"Estimate query failed\u001b[31m","caller":"q.go:1"},{"timestamp":"2026-09-07T20:41:00Z","level":"INFO","message":"started"}],"since_minutes":60,"timestamp":"t"}`))
	})
	mux.HandleFunc("/api/v1/import/stats", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"stats":{"total_errors":1,"total_records":250,"total_requests":3},"status":"success"}`))
	})
	mux.HandleFunc("/api/v1/query/estimate", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "nosuch") {
			_, _ = w.Write([]byte(`{"success":false,"estimated_rows":null,"warning_level":"error","execution_time_ms":1,"error":"Cannot estimate query: No files found"}`))
			return
		}
		_, _ = w.Write([]byte(`{"success":true,"estimated_rows":2500000,"warning_level":"high","warning_message":"Large query: 2,500,000 rows.","execution_time_ms":840}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestLogs_TableCSVJSONAndValidation(t *testing.T) {
	srv := fakeOpsServer(t)
	writeTestConfig(t, srv.URL, "tok")
	out, _, err := execCmd(t, newLogsCmd())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "2026-09-07T20:42:09Z") || !strings.Contains(out, "Estimate query failed[31m") || strings.Contains(out, "\x1b") {
		t.Errorf("table = %s", out)
	}
	out, _, err = execCmd(t, newLogsCmd(), "-o", "csv", "--no-header")
	if err != nil || !strings.HasPrefix(out, "2026-09-07T20:42:09Z,ERROR,query,") {
		t.Errorf("csv = %q err=%v", out, err)
	}
	out, _, err = execCmd(t, newLogsCmd(), "--level", "FATAL")
	if err != nil || !strings.Contains(out, "(no log entries in the last 60 minute(s) at level fatal or above)") {
		t.Errorf("empty = %q err=%v", out, err)
	}
	out, _, err = execCmd(t, newLogsCmd(), "-o", "json")
	if err != nil || !strings.Contains(out, `"logs": [`) {
		t.Errorf("json = %s err=%v", out, err)
	}
	for _, bad := range [][]string{{"--limit", "0"}, {"--limit", "1001"}, {"--level", "loud"}, {"--since", "30s"}, {"--since", "25h"}} {
		if _, _, err := execCmd(t, newLogsCmd(), bad...); err == nil {
			t.Errorf("%v should be rejected", bad)
		}
	}
	writeTestConfig(t, srv.URL, "reader")
	_, _, err = execCmd(t, newLogsCmd())
	if err == nil || !strings.Contains(err.Error(), "admin required") {
		t.Errorf("err = %v", err)
	}
}

func TestImportStats(t *testing.T) {
	srv := fakeOpsServer(t)
	writeTestConfig(t, srv.URL, "tok")
	out, _, err := execCmd(t, newImportStatsCmd())
	if err != nil || !strings.Contains(out, "requests: 3") || !strings.Contains(out, "records:  250") || !strings.Contains(out, "errors:   1") {
		t.Errorf("out=%q err=%v", out, err)
	}
	out, _, err = execCmd(t, newImportStatsCmd(), "-o", "json")
	if err != nil || !strings.Contains(out, `"total_records": 250`) {
		t.Errorf("json=%q err=%v", out, err)
	}
}

func TestQueryEstimate(t *testing.T) {
	srv := fakeOpsServer(t)
	writeTestConfig(t, srv.URL, "tok")
	out, _, err := execCmd(t, newQueryCmd(), "--estimate", "SELECT * FROM cpu")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"estimated rows: 2500000", "size class:     high", "note:           Large query", "estimate took:  840ms"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %s", want, out)
		}
	}
	out, _, err = execCmd(t, newQueryCmd(), "--estimate", "SELECT * FROM nosuch", "-o", "json")
	if err == nil || !strings.Contains(err.Error(), "Cannot estimate query") {
		t.Errorf("err = %v", err)
	}
	if !strings.Contains(out, `"success": false`) {
		t.Errorf("json must still be printed on failure: %q", out)
	}
	_, _, err = execCmd(t, newQueryCmd(), "--estimate", "SELECT 1", "-o", "arrow")
	if err == nil || !strings.Contains(err.Error(), "--estimate supports -o table or json") {
		t.Errorf("err = %v", err)
	}
}
