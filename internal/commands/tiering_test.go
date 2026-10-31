package commands

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTieringFilesFormatsAndFilters(t *testing.T) {
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/tiering/files" {
			t.Errorf("path = %s", r.URL.Path)
		}
		queries = append(queries, r.URL.RawQuery)
		_, _ = w.Write([]byte(`{"files":[{"id":1,"path":"metrics/cpu/1.parquet","database":"metrics","measurement":"cpu","partition_time":"2026-10-31T00:00:00Z","tier":"cold","size_bytes":1024,"created_at":"2026-10-31T00:00:00Z"}],"count":1}`))
	}))
	defer srv.Close()
	writeTestConfig(t, srv.URL, "tok")

	for _, tc := range []struct {
		format  string
		filters []string
		want    string
	}{
		{"table", []string{"--tier", "cold"}, "PATH"},
		{"csv", []string{"--database", "metrics"}, "PATH,DATABASE,MEASUREMENT,TIER,SIZE BYTES"},
	} {
		args := append([]string{"tiering", "files"}, tc.filters...)
		args = append(args, "--limit", "2", "--output", tc.format)
		out, _, err := execCmd(t, NewRoot(BuildInfo{}), args...)
		if err != nil || !strings.Contains(out, tc.want) || !strings.Contains(out, "metrics/cpu/1.parquet") {
			t.Errorf("%s output=%q err=%v", tc.format, out, err)
		}
	}
	out, _, err := execCmd(t, NewRoot(BuildInfo{}), "tiering", "files", "--output", "json", "--limit", "2")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil || got["count"] != float64(1) {
		t.Fatalf("json = %q err=%v", out, err)
	}
	for _, query := range queries {
		if !strings.Contains(query, "limit=2") {
			t.Errorf("query = %q", query)
		}
	}
	if !strings.Contains(queries[0], "tier=cold") || !strings.Contains(queries[1], "database=metrics") || queries[2] != "limit=2" {
		t.Errorf("queries = %v", queries)
	}
}

func TestTieringObjectCommandsRenderEndpointResponses(t *testing.T) {
	paths := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths[r.URL.Path]++
		switch r.URL.Path {
		case "/api/v1/tiering/status":
			_, _ = w.Write([]byte(`{"enabled":true,"license_valid":true,"quarantined_files":1,"tiers":{"hot":{"tier":"hot","enabled":true,"backend":"local","file_count":4,"total_size_mb":2}},"last_scan":{"files_scanned":4,"truncated":false},"future":true}`))
		case "/api/v1/tiering/stats":
			_, _ = w.Write([]byte(`{"tier_stats":[{"tier":"cold","enabled":true,"backend":"s3","file_count":2,"total_size_mb":9}],"recent_migrations":[{"id":3,"file_path":"metrics/cpu/a.parquet","database":"metrics","from_tier":"hot","to_tier":"cold","size_bytes":2048,"started_at":"2026-10-31T00:00:00Z"}]}`))
		case "/api/v1/tiering/migrate":
			var req map[string]any
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode migrate request: %v", err)
			}
			if req["database"] != "metrics" || req["measurement"] != "cpu" || req["dry_run"] != true {
				t.Errorf("migrate request = %v", req)
			}
			_, _ = w.Write([]byte(`{"status":"dry_run","would_migrate":2,"total_bytes":2048,"from_tier":"hot","to_tier":"cold"}`))
		case "/api/v1/tiering/scan":
			_, _ = w.Write([]byte(`{"files_scanned":6,"files_registered":5,"files_skipped":1,"errors":0,"cold_synced":2,"hot_retired":1,"hot_rows_written":3,"truncated":false}`))
		case "/api/v1/tiering/policies/":
			_, _ = w.Write([]byte(`{"policies":[{"id":8,"database":"metrics","hot_only":false,"hot_max_age_days":30,"created_at":"2026-10-31T00:00:00Z","updated_at":"2026-10-31T00:00:00Z"}],"count":1}`))
		case "/api/v1/tiering/policies/metrics":
			_, _ = w.Write([]byte(`{"id":8,"database":"metrics","hot_only":false,"hot_max_age_days":30,"created_at":"2026-10-31T00:00:00Z","updated_at":"2026-10-31T00:00:00Z"}`))
		case "/api/v1/tiering/policies/metrics/effective":
			_, _ = w.Write([]byte(`{"database":"metrics","hot_only":false,"hot_max_age_days":14,"source":"global"}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	writeTestConfig(t, srv.URL, "tok")

	checks := []struct {
		args []string
		want string
	}{
		{[]string{"tiering", "status"}, "quarantined files"},
		{[]string{"tiering", "status", "--output", "json"}, `"future": true`},
		{[]string{"tiering", "stats"}, "metrics/cpu/a.parquet"},
		{[]string{"tiering", "migrate", "--database", "metrics", "--measurement", "cpu", "--dry-run"}, "would migrate"},
		{[]string{"tiering", "migrate", "--database", "metrics", "--measurement", "cpu", "--dry-run", "--output", "json"}, `"would_migrate": 2`},
		{[]string{"tiering", "scan"}, "files scanned"},
		{[]string{"tiering", "policy", "list", "--output", "csv"}, "DATABASE,HOT ONLY,HOT MAX AGE DAYS"},
		{[]string{"tiering", "policy", "list", "--output", "json"}, `"count": 1`},
		{[]string{"tiering", "policy", "show", "metrics"}, "hot max age days"},
		{[]string{"tiering", "policy", "show", "metrics", "--output", "json"}, `"hot_max_age_days": 30`},
		{[]string{"tiering", "policy", "effective", "metrics"}, "source"},
	}
	for _, tc := range checks {
		out, _, err := execCmd(t, NewRoot(BuildInfo{}), tc.args...)
		if err != nil || !strings.Contains(out, tc.want) {
			t.Errorf("%v output=%q err=%v, want %q", tc.args, out, err, tc.want)
		}
	}
	for _, path := range []string{
		"/api/v1/tiering/status", "/api/v1/tiering/stats", "/api/v1/tiering/migrate",
		"/api/v1/tiering/scan", "/api/v1/tiering/policies/", "/api/v1/tiering/policies/metrics",
		"/api/v1/tiering/policies/metrics/effective",
	} {
		if paths[path] == 0 {
			t.Errorf("endpoint not exercised: %s", path)
		}
	}
}

func TestTieringPolicySetValidatesAgeAndDeletePrompts(t *testing.T) {
	var deletes int
	var putBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/tiering/policies/metrics" {
			t.Errorf("path = %s", r.URL.Path)
		}
		switch r.Method {
		case http.MethodPut:
			if err := json.NewDecoder(r.Body).Decode(&putBody); err != nil {
				t.Errorf("decode request: %v", err)
			}
			_, _ = w.Write([]byte(`{"message":"Policy updated successfully","database":"metrics","policy":{"id":7,"database":"metrics","hot_only":true,"hot_max_age_days":0}}`))
		case http.MethodDelete:
			deletes++
			_, _ = w.Write([]byte(`{"message":"Policy deleted, database now uses global defaults","database":"metrics"}`))
		default:
			t.Errorf("method = %s", r.Method)
		}
	}))
	defer srv.Close()
	writeTestConfig(t, srv.URL, "tok")

	_, _, err := execCmd(t, NewRoot(BuildInfo{}), "tiering", "policy", "set", "metrics", "--hot-max-age-days", "-1")
	if err == nil || !strings.Contains(err.Error(), "must not be negative") {
		t.Fatalf("negative age error = %v", err)
	}
	if deletes != 0 || putBody != nil {
		t.Fatalf("invalid policy input reached server: put=%v deletes=%d", putBody, deletes)
	}

	out, _, err := execCmd(t, NewRoot(BuildInfo{}), "tiering", "policy", "set", "metrics", "--hot-only", "--hot-max-age-days", "0", "--output", "json")
	if err != nil || !strings.Contains(out, "Policy updated successfully") || putBody["hot_only"] != true || putBody["hot_max_age_days"] != float64(0) {
		t.Fatalf("set output=%q body=%v err=%v", out, putBody, err)
	}

	root := NewRoot(BuildInfo{})
	root.SetIn(confirmationInput(t, "decline"))
	out, stderr, err := execCmd(t, root, "tiering", "policy", "delete", "metrics")
	if err == nil || err.Error() != "aborted" || out != "" || !strings.Contains(stderr, `Delete the custom tiering policy for database "metrics"?`) || deletes != 0 {
		t.Fatalf("declined delete: out=%q stderr=%q err=%v deletes=%d", out, stderr, err, deletes)
	}

	out, _, err = execCmd(t, NewRoot(BuildInfo{}), "tiering", "policy", "delete", "metrics", "--yes", "--output", "json")
	if err != nil || !strings.Contains(out, "global defaults") || deletes != 1 {
		t.Fatalf("confirmed delete: out=%q err=%v deletes=%d", out, err, deletes)
	}
}

func TestTieringPolicyDeleteRefusesNonInteractiveStdin(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = w.Write([]byte(`{"message":"deleted"}`))
	}))
	defer srv.Close()
	writeTestConfig(t, srv.URL, "tok")
	root := NewRoot(BuildInfo{})
	root.SetIn(confirmationInput(t, "pipe"))
	_, _, err := execCmd(t, root, "tiering", "policy", "delete", "metrics")
	if err == nil || !strings.Contains(err.Error(), "stdin is not a terminal; pass --yes") || requests != 0 {
		t.Fatalf("err=%v requests=%d", err, requests)
	}
}

func TestTieringCommandsRegistered(t *testing.T) {
	root := NewRoot(BuildInfo{Version: "test"})
	for _, path := range [][]string{
		{"tiering", "status"}, {"tiering", "files"}, {"tiering", "stats"},
		{"tiering", "migrate"}, {"tiering", "scan"}, {"tiering", "policy", "list"},
		{"tiering", "policy", "show"}, {"tiering", "policy", "set"},
		{"tiering", "policy", "delete"}, {"tiering", "policy", "effective"},
	} {
		if _, _, err := root.Find(path); err != nil {
			t.Errorf("missing command %v: %v", path, err)
		}
	}
}
