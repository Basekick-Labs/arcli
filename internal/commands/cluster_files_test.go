package commands

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClusterFiles_TableJSONAndCSV(t *testing.T) {
	const response = `{"files":[{"path":"metrics/cpu/part.parquet","sha256":"abc123","size_bytes":1536,"database":"metrics","measurement":"cpu","partition_time":"2026-10-27T00:00:00.123Z","origin_node_id":"node-1","tier":"hot","created_at":"2026-10-27T01:00:00Z","lsn":7}],"total":1,"next_cursor":"metrics/cpu/next part.parquet","future_field":"preserved"}`
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/cluster/files" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.URL.Query().Get("database"); got != "metrics" {
			t.Errorf("database = %q", got)
		}
		if got := r.URL.Query().Get("cursor"); got != "metrics/cpu/first part.parquet" {
			t.Errorf("cursor = %q", got)
		}
		if got := r.URL.Query().Get("limit"); got != "2" {
			t.Errorf("limit = %q", got)
		}
		_, _ = fmt.Fprint(w, response)
	}))
	defer srv.Close()
	writeTestConfig(t, srv.URL, "admin-token")
	args := []string{"--database", "metrics", "--limit", "2", "--cursor", "metrics/cpu/first part.parquet"}

	out, stderr, err := execCmd(t, newClusterFilesCmd(), args...)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"metrics/cpu/part.parquet", "cpu", "1.5 KiB", "next cursor: metrics/cpu/next part.parquet"} {
		if !strings.Contains(out, want) {
			t.Errorf("table output missing %q:\n%s", want, out)
		}
	}
	if stderr != "" {
		t.Errorf("table stderr = %q", stderr)
	}

	out, stderr, err = execCmd(t, newClusterFilesCmd(), append(args, "-o", "csv")...)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(strings.NewReader(out)).ReadAll()
	if err != nil {
		t.Fatalf("invalid CSV: %v\n%s", err, out)
	}
	if len(rows) != 2 || rows[1][0] != "metrics/cpu/part.parquet" || rows[1][1] != "metrics" {
		t.Errorf("CSV rows = %#v", rows)
	}
	if !strings.Contains(stderr, "next cursor: metrics/cpu/next part.parquet") {
		t.Errorf("CSV stderr should expose the continuation cursor, got %q", stderr)
	}

	out, _, err = execCmd(t, newClusterFilesCmd(), append(args, "-o", "json")...)
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Files      []map[string]any `json:"files"`
		Total      int              `json:"total"`
		NextCursor string           `json:"next_cursor"`
		Future     string           `json:"future_field"`
	}
	if err := json.Unmarshal([]byte(out), &page); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if page.Total != 1 || page.NextCursor != "metrics/cpu/next part.parquet" || page.Future != "preserved" || len(page.Files) != 1 {
		t.Errorf("JSON page = %+v", page)
	}
	if requests != 3 {
		t.Errorf("got %d requests, want 3", requests)
	}
}

func TestClusterFiles_StandaloneAndFlagValidation(t *testing.T) {
	srv := fakeStandaloneServer(t)
	writeTestConfig(t, srv.URL, "token")
	_, _, err := execCmd(t, newClusterFilesCmd())
	if err == nil || !strings.Contains(err.Error(), "clustering is not enabled on this server") {
		t.Errorf("standalone error = %v", err)
	}

	_, _, err = execCmd(t, newClusterFilesCmd(), "--limit", "10001")
	if err == nil || !strings.Contains(err.Error(), "limit must be between 1 and 10000") {
		t.Errorf("limit error = %v", err)
	}
	_, _, err = execCmd(t, newClusterFilesCmd(), "--database", "1metrics")
	if err == nil || !strings.Contains(err.Error(), "invalid database name") {
		t.Errorf("database error = %v", err)
	}
}

func TestClusterFiles_DefaultRequestIsBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/cluster/files" || r.URL.Query().Get("limit") != "1000" {
			t.Errorf("default request = %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		if r.URL.Query().Get("cursor") != "" || r.URL.Query().Get("database") != "" {
			t.Errorf("unexpected default filters: %s", r.URL.RawQuery)
		}
		_, _ = fmt.Fprint(w, `{"files":[],"total":0,"next_cursor":""}`)
	}))
	defer srv.Close()
	writeTestConfig(t, srv.URL, "admin-token")
	out, _, err := execCmd(t, newClusterFilesCmd())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "files on this page: 0") {
		t.Errorf("empty page output = %q", out)
	}
}

type clusterFilesFailWriter struct {
	prefix string
	err    error
}

func (w clusterFilesFailWriter) Write(p []byte) (int, error) {
	if strings.HasPrefix(string(p), w.prefix) {
		return 0, w.err
	}
	return len(p), nil
}

func TestClusterFiles_PropagatesPaginationOutputErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"files":[],"total":0,"next_cursor":"next.parquet"}`)
	}))
	defer srv.Close()
	writeTestConfig(t, srv.URL, "token")
	for _, format := range []string{"table", "csv"} {
		for _, prefix := range []string{"files on this page:", "next cursor:"} {
			t.Run(format+"/"+prefix, func(t *testing.T) {
				want := errors.New("pagination output failed")
				cmd := newClusterFilesCmd()
				cmd.SilenceUsage, cmd.SilenceErrors = true, true
				cmd.SetArgs([]string{"-o", format})
				cmd.SetOut(io.Discard)
				cmd.SetErr(io.Discard)
				writer := clusterFilesFailWriter{prefix: prefix, err: want}
				if format == "csv" {
					cmd.SetErr(writer)
				} else {
					cmd.SetOut(writer)
				}
				if err := cmd.Execute(); !errors.Is(err, want) {
					t.Fatalf("error = %v, want %v", err, want)
				}
			})
		}
	}
}

func TestClusterFilePrecisionRawCSVAndNoHeader(t *testing.T) {
	const stamp = "2026-10-27T03:04:05.123456789+02:30"
	const utc = "2026-10-27T00:34:05.123456789Z"
	const raw = "raw\nvalue\twith,comma\"quote\x1b[31m"
	file := map[string]any{"path": raw, "database": raw, "measurement": raw, "tier": raw, "sha256": raw, "origin_node_id": raw, "compaction_job": raw, "partition_time": stamp, "created_at": stamp}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"files": []any{file}, "total": 1, "next_cursor": "next", "extra": "kept"})
	}))
	defer srv.Close()
	writeTestConfig(t, srv.URL, "admin")
	for _, format := range []string{"table", "csv", "json"} {
		for _, noHeader := range []bool{false, true} {
			args := []string{"cluster", "file", "-o", format}
			if noHeader {
				args = append(args, "--no-header")
			}
			out, stderr, err := execCmd(t, NewRoot(BuildInfo{}), args...)
			want := utc
			if format == "json" {
				want = stamp
			}
			if err != nil || !strings.Contains(out, want) {
				t.Fatalf("%s/%t: %q %v", format, noHeader, out, err)
			}
			if format == "csv" {
				rows, err := csv.NewReader(strings.NewReader(out)).ReadAll()
				count := 2
				if noHeader {
					count = 1
				}
				if err != nil || len(rows) != count {
					t.Fatalf("CSV=%v err=%v", rows, err)
				}
				row := rows[len(rows)-1]
				for _, i := range []int{0, 1, 2, 3, 5, 7, 11} {
					if row[i] != raw {
						t.Errorf("column %d changed: %q", i, row[i])
					}
				}
				if row[6] != utc || row[8] != utc || !strings.Contains(stderr, "next cursor: next") {
					t.Errorf("CSV timestamps/cursor: %v %q", row, stderr)
				}
			}
			if format == "table" && strings.Contains(out, "PARTITION TIME") == noHeader {
				t.Errorf("table header=%t: %q", noHeader, out)
			}
			if format == "json" && (!strings.Contains(out, `"extra": "kept"`) || !strings.Contains(out, `"next_cursor": "next"`)) {
				t.Errorf("JSON changed: %q", out)
			}
		}
	}
	for _, cmd := range newClusterCmd().Commands() {
		if cmd.Name() == "files" {
			t.Error("legacy files registered")
		}
	}
}
