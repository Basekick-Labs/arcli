package client

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestAuditLogs_FiltersPaginationAndRawResponse(t *testing.T) {
	var gotQuery url.Values
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertAuthHeaders(t, r)
		if r.URL.Path != "/api/v1/audit/logs" || r.Method != http.MethodGet {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		gotQuery = r.URL.Query()
		_, _ = w.Write([]byte(`{"success":true,"data":[{"id":9,"timestamp":"2026-10-27T10:00:00Z","event_type":"auth.login","actor":"admin","method":"POST","path":"/api/v1/auth/login","status_code":200,"ip_address":"127.0.0.1","user_agent":"test","duration_ms":3,"detail":"ok"}],"count":1,"server_field":"kept"}`))
	})
	res, err := cli.AuditLogs(context.Background(), AuditLogsOptions{
		EventType: "auth.login", Actor: "a b", Database: "metrics", Since: "2026-10-01T00:00:00Z",
		Until: "2026-10-27T00:00:00+03:00", Limit: 50, Offset: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"event_type": "auth.login", "actor": "a b", "database": "metrics", "since": "2026-10-01T00:00:00Z",
		"until": "2026-10-27T00:00:00+03:00", "limit": "50", "offset": "100",
	} {
		if got := gotQuery.Get(key); got != want {
			t.Errorf("query %s = %q, want %q", key, got, want)
		}
	}
	if !res.Success || res.Count != 1 || len(res.Data) != 1 || res.Data[0].Actor != "admin" || res.Data[0].Timestamp.IsZero() {
		t.Errorf("result = %+v", res)
	}
	if !strings.Contains(string(res.Raw), `"server_field":"kept"`) {
		t.Errorf("raw response lost unknown fields: %s", res.Raw)
	}
}

func TestAuditLogs_ValidationAndNullData(t *testing.T) {
	requests := 0
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = w.Write([]byte(`{"success":true,"data":null,"count":0,"other":1}`))
	})
	for _, opts := range []AuditLogsOptions{
		{Since: "yesterday"}, {Until: "2026-10-27"}, {Limit: -1}, {Limit: AuditMaxLimit + 1}, {Offset: -1},
	} {
		if _, err := cli.AuditLogs(context.Background(), opts); err == nil {
			t.Errorf("%+v should be rejected", opts)
		}
	}
	if requests != 0 {
		t.Fatalf("invalid filters made %d requests", requests)
	}
	res, err := cli.AuditLogs(context.Background(), AuditLogsOptions{})
	if err != nil || res.Data == nil || len(res.Data) != 0 || !strings.Contains(string(res.Raw), `"other":1`) || !strings.Contains(string(res.Raw), `"data":[]`) {
		t.Errorf("res=%+v raw=%s err=%v", res, res.Raw, err)
	}
}

func TestAuditStats_DefaultAndFiltered(t *testing.T) {
	var queries []string
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertAuthHeaders(t, r)
		queries = append(queries, r.URL.RawQuery)
		_, _ = w.Write([]byte(`{"success":true,"data":{"auth.login":4,"query.execute":2},"since":"2026-10-26T00:00:00Z","status":"ok"}`))
	})
	if _, err := cli.AuditStats(context.Background(), "bad"); err == nil || len(queries) != 0 {
		t.Fatalf("invalid timestamp err=%v queries=%v", err, queries)
	}
	def, err := cli.AuditStats(context.Background(), "")
	if err != nil || queries[0] != "" || def.Data["auth.login"] != 4 || !strings.Contains(string(def.Raw), `"status":"ok"`) {
		t.Errorf("default stats=%+v queries=%v err=%v", def, queries, err)
	}
	filtered, err := cli.AuditStats(context.Background(), "2026-10-01T00:00:00Z")
	if err != nil || queries[1] != "since=2026-10-01T00%3A00%3A00Z" || filtered.Since == "" {
		t.Errorf("filtered stats=%+v queries=%v err=%v", filtered, queries, err)
	}
}

func TestAuditLicenseErrorIsPreserved(t *testing.T) {
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"success":false,"error":"Audit logging requires an enterprise license with the 'audit_logging' feature enabled"}`))
	})
	_, err := cli.AuditStats(context.Background(), "")
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != http.StatusForbidden || !strings.Contains(he.Message, "enterprise license") {
		t.Errorf("err = %v", err)
	}
}
