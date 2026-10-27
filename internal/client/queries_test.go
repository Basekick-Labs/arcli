package client

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

const testQueryID = "8f14e45fceea"

func TestQueryManagementEndpoints(t *testing.T) {
	var requests int
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		assertAuthHeaders(t, r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/queries/active":
			_, _ = w.Write([]byte(`{"success":true,"count":1,"queries":[{"id":"8f14e45fceea","sql":"SELECT 1","token_id":7,"token_name":"admin","remote_addr":"127.0.0.1","status":"running","start_time":"2026-10-27T10:00:00Z","duration_ms":12.5,"is_parallel":false}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/queries/history":
			if got := r.URL.Query().Get("limit"); got != "" && got != "2" {
				t.Errorf("history limit = %q", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"success":true,"count":1,"queries":[{"id":"8f14e45fceea","sql":"SELECT 1","status":"completed","start_time":"2026-10-27T10:00:00Z","end_time":"2026-10-27T10:00:01Z","duration_ms":1000,"row_count":1,"row_cap":9,"is_parallel":false}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/queries/"+testQueryID:
			_, _ = w.Write([]byte(`{"success":true,"query":{"id":"8f14e45fceea","sql":"SELECT 1","status":"running","start_time":"2026-10-27T10:00:00Z","is_parallel":false},"extra":"preserved"}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/queries/"+testQueryID:
			_, _ = w.Write([]byte(`{"success":true,"message":"Query cancelled"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	})

	active, err := cli.ActiveQueries(context.Background())
	if err != nil || active.Count != 1 || len(active.Queries) != 1 || active.Queries[0].TokenName != "admin" || active.Queries[0].DurationMs != 12.5 {
		t.Fatalf("active = %+v, err = %v", active, err)
	}
	history, err := cli.QueryHistory(context.Background(), 2)
	if err != nil || history.Queries[0].RowCap != 9 || history.Queries[0].EndTime == nil {
		t.Fatalf("history = %+v, err = %v", history, err)
	}
	if _, err := cli.QueryHistory(context.Background(), 0); err != nil {
		t.Fatalf("default history = %v", err)
	}
	detail, err := cli.GetManagedQuery(context.Background(), testQueryID)
	if err != nil || detail.Query.ID != testQueryID || !strings.Contains(string(detail.Raw), `"extra":"preserved"`) {
		t.Fatalf("detail = %+v, err = %v", detail, err)
	}
	cancelled, err := cli.CancelManagedQuery(context.Background(), testQueryID)
	if err != nil || !cancelled.Success || cancelled.Message != "Query cancelled" {
		t.Fatalf("cancelled = %+v, err = %v", cancelled, err)
	}
	if requests != 5 {
		t.Errorf("requests = %d, want 5", requests)
	}
}

func TestQueryManagementValidationAndEmptyList(t *testing.T) {
	var requests int
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = w.Write([]byte(`{"success":true,"count":0,"queries":null,"extra":"preserved"}`))
	})
	for _, id := range []string{"", "123", "8f14e45fceeg", "8F14E45FCEEA"} {
		if _, err := cli.GetManagedQuery(context.Background(), id); err == nil {
			t.Errorf("invalid ID %q was accepted", id)
		}
		if _, err := cli.CancelManagedQuery(context.Background(), id); err == nil {
			t.Errorf("invalid ID %q was accepted for cancellation", id)
		}
	}
	for _, limit := range []int{-1, 1001} {
		if _, err := cli.QueryHistory(context.Background(), limit); err == nil {
			t.Errorf("invalid limit %d was accepted", limit)
		}
	}
	if requests != 0 {
		t.Fatalf("invalid input made %d requests", requests)
	}
	res, err := cli.ActiveQueries(context.Background())
	if err != nil || res.Queries == nil || len(res.Queries) != 0 {
		t.Fatalf("empty active result = %+v, err = %v", res, err)
	}
	if !strings.Contains(string(res.Raw), `"queries":[]`) || !strings.Contains(string(res.Raw), `"extra":"preserved"`) {
		t.Errorf("normalised raw response lost data: %s", res.Raw)
	}
}

func TestQueryManagementPreservesServerErrorStatuses(t *testing.T) {
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/queries/active":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"success":false,"error":"Query management requires an enterprise license with the 'query_management' feature enabled"}`))
		case "/api/v1/queries/history":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"success":false,"error":"Permission denied: admin required"}`))
		case "/api/v1/queries/" + testQueryID:
			if r.Method == http.MethodDelete {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"success":false,"error":"Query already completed"}`))
				return
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"success":false,"error":"Query not found"}`))
		}
	})
	checks := []struct {
		name   string
		call   func() error
		status int
		text   string
	}{
		{"licence", func() error { _, err := cli.ActiveQueries(context.Background()); return err }, 403, "query_management"},
		{"admin permission", func() error { _, err := cli.QueryHistory(context.Background(), 0); return err }, 403, "admin required"},
		{"not found", func() error { _, err := cli.GetManagedQuery(context.Background(), testQueryID); return err }, 404, "Query not found"},
		{"already finished", func() error { _, err := cli.CancelManagedQuery(context.Background(), testQueryID); return err }, 409, "Query already completed"},
	}
	for _, tc := range checks {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			var he *HTTPError
			if !errors.As(err, &he) || he.Status != tc.status || !strings.Contains(he.Message, tc.text) {
				t.Fatalf("error = %#v, want HTTP %d containing %q", err, tc.status, tc.text)
			}
		})
	}
}
