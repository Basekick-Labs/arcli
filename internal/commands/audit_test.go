package commands

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAuditLogs_TableCSVJSONAndFilters(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/audit/logs" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.URL.Query().Get("event_type") != "" && (r.URL.Query().Get("event_type") != "auth.login" || r.URL.Query().Get("actor") != "admin" || r.URL.Query().Get("limit") != "10" || r.URL.Query().Get("offset") != "20") {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"success":true,"data":[{"id":3,"timestamp":"2026-10-27T10:00:00Z","event_type":"auth.login","actor":"admin","method":"POST","path":"/login","database":"metrics","measurement":"cpu","status_code":200,"ip_address":"127.0.0.1","user_agent":"agent","duration_ms":8,"detail":"ok"}],"count":1,"extra":"kept"}`))
	}))
	defer srv.Close()
	writeTestConfig(t, srv.URL, "tok")
	out, _, err := execCmd(t, newAuditLogsCmd(), "--event-type", "auth.login", "--actor", "admin", "--limit", "10", "--offset", "20")
	if err != nil || !strings.Contains(out, "auth.login") || !strings.Contains(out, "admin") || !strings.Contains(out, "metrics") {
		t.Errorf("table=%q err=%v", out, err)
	}
	out, _, err = execCmd(t, newAuditLogsCmd(), "-o", "csv", "--no-header")
	if err != nil || !strings.HasPrefix(out, "3,2026-10-27T10:00:00Z,auth.login,admin,POST") || strings.Contains(out, "id,timestamp") {
		t.Errorf("csv=%q err=%v", out, err)
	}
	out, _, err = execCmd(t, newAuditLogsCmd(), "-o", "json")
	if err != nil || !strings.Contains(out, `"extra": "kept"`) || !strings.Contains(out, `"detail": "ok"`) {
		t.Errorf("json=%q err=%v", out, err)
	}
	for _, bad := range [][]string{{"--since", "yesterday"}, {"--until", "2026-10-27"}, {"--limit", "0"}, {"--limit", "10001"}, {"--offset", "-1"}, {"-o", "arrow"}} {
		if _, _, err := execCmd(t, newAuditLogsCmd(), bad...); err == nil {
			t.Errorf("%v should be rejected", bad)
		}
	}
}

func TestAuditStats_TableJSONAndLicenseError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/audit/stats" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.URL.Query().Get("since") != "2026-10-01T00:00:00Z" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"success":true,"data":{"query.execute":2,"auth.login":4},"since":"2026-10-01T00:00:00Z"}`))
	}))
	defer srv.Close()
	writeTestConfig(t, srv.URL, "tok")
	out, _, err := execCmd(t, newAuditStatsCmd(), "--since", "2026-10-01T00:00:00Z")
	if err != nil || !strings.Contains(out, "since: 2026-10-01T00:00:00Z") || strings.Index(out, "auth.login") > strings.Index(out, "query.execute") {
		t.Errorf("table=%q err=%v", out, err)
	}
	out, _, err = execCmd(t, newAuditStatsCmd(), "-o", "json", "--since", "2026-10-01T00:00:00Z")
	if err != nil || !strings.Contains(out, `"auth.login": 4`) {
		t.Errorf("json=%q err=%v", out, err)
	}
	_, _, err = execCmd(t, newAuditStatsCmd(), "--since", "bad")
	if err == nil || !strings.Contains(err.Error(), "RFC3339") {
		t.Errorf("invalid since err=%v", err)
	}

	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"success":false,"error":"Audit logging requires an enterprise license with the 'audit_logging' feature enabled"}`))
	}))
	defer denied.Close()
	writeTestConfig(t, denied.URL, "tok")
	_, _, err = execCmd(t, newAuditStatsCmd())
	if err == nil || !strings.Contains(err.Error(), "enterprise license") {
		t.Errorf("license err=%v", err)
	}
}
