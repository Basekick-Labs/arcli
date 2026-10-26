package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestClusterFiles_PagesAreBoundedAndDecoded(t *testing.T) {
	var requests int
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertAuthHeaders(t, r)
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/cluster/files" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.URL.Query().Get("database"); got != "metrics" {
			t.Errorf("database = %q", got)
		}
		if got := r.URL.Query().Get("limit"); got != "2" {
			t.Errorf("limit = %q", got)
		}
		requests++
		switch requests {
		case 1:
			if got := r.URL.Query().Get("cursor"); got != "" {
				t.Errorf("first cursor = %q", got)
			}
			_, _ = fmt.Fprint(w, `{"files":[{"path":"metrics/cpu/first.parquet","sha256":"abc","size_bytes":10,"database":"metrics","measurement":"cpu","partition_time":"2026-10-27T00:00:00.123Z","origin_node_id":"n1","tier":"hot","created_at":"2026-10-27T01:00:00Z","lsn":7}],"total":1,"next_cursor":"metrics/cpu/first.parquet","future_field":true}`)
		case 2:
			if got := r.URL.Query().Get("cursor"); got != "metrics/cpu/first.parquet" {
				t.Errorf("second cursor = %q", got)
			}
			_, _ = fmt.Fprint(w, `{"files":[{"path":"metrics/cpu/second.parquet","database":"metrics","measurement":"cpu","size_bytes":12}],"total":1,"next_cursor":""}`)
		default:
			t.Errorf("unexpected request %d", requests)
		}
	})

	first, err := cli.ClusterFiles(context.Background(), "metrics", "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if first.Total != 1 || first.NextCursor != "metrics/cpu/first.parquet" {
		t.Fatalf("first page = %+v", first)
	}
	if len(first.Files) != 1 || first.Files[0].SizeBytes != 10 || first.Files[0].LSN != 7 || first.Files[0].PartitionTime.Nanosecond() != 123_000_000 {
		t.Fatalf("first files = %+v", first.Files)
	}
	if !strings.Contains(string(first.Raw), `"future_field":true`) {
		t.Errorf("raw response lost unknown fields: %s", first.Raw)
	}

	second, err := cli.ClusterFiles(context.Background(), "metrics", first.NextCursor, 2)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 || second.Total != 1 || second.NextCursor != "" || len(second.Files) != 1 || second.Files[0].Path != "metrics/cpu/second.parquet" {
		t.Errorf("second page = %+v after %d requests", second, requests)
	}
}

func TestClusterFiles_ValidatesArgumentsBeforeRequest(t *testing.T) {
	var requests int
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = fmt.Fprint(w, `{"files":[],"total":0}`)
	})
	tooLongCursor := strings.Repeat("x", 4097)
	for _, tc := range []struct {
		name     string
		database string
		cursor   string
		limit    int
		want     string
	}{
		{name: "database", database: "1metrics", limit: 1000, want: "invalid database name"},
		{name: "low limit", limit: 0, want: "limit must be between 1 and 10000"},
		{name: "high limit", limit: 10001, want: "limit must be between 1 and 10000"},
		{name: "long cursor", cursor: tooLongCursor, limit: 1000, want: "cursor must be at most 4096 bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := cli.ClusterFiles(context.Background(), tc.database, tc.cursor, tc.limit)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
	if requests != 0 {
		t.Errorf("invalid arguments made %d requests", requests)
	}
}

func TestClusterFiles_StandaloneAndHTTPErrorsStayDistinct(t *testing.T) {
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, standaloneBody)
	})
	_, err := cli.ClusterFiles(context.Background(), "", "", 1000)
	var disabled *ClusterDisabledError
	if !errors.As(err, &disabled) || disabled.Reason != "Enterprise license not configured" {
		t.Fatalf("standalone error = %v", err)
	}

	for _, tc := range []struct {
		status int
		body   string
		want   string
	}{
		{http.StatusForbidden, `{"error":"admin permission required"}`, "admin permission required"},
		{http.StatusInternalServerError, `{"error":"manifest pagination failed"}`, "manifest pagination failed"},
	} {
		cli, _ = newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = fmt.Fprint(w, tc.body)
		})
		_, err = cli.ClusterFiles(context.Background(), "", "", 1000)
		var httpErr *HTTPError
		if !errors.As(err, &httpErr) || httpErr.Status != tc.status || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("status %d error = %v", tc.status, err)
		}
	}
}

func TestClusterFiles_RequiresPaginatedResponse(t *testing.T) {
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"files":[],"total":0}`)
	})
	_, err := cli.ClusterFiles(context.Background(), "", "", 1000)
	if err == nil || !strings.Contains(err.Error(), "response is missing next_cursor") {
		t.Fatalf("unpaginated response error = %v", err)
	}
}
