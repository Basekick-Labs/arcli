package commands

import (
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

const commandTestQueryID = "6bce82c4-244"

func fakeQueryManagementServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/queries/active", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("active method = %s", r.Method)
		}
		_, _ = w.Write([]byte(`{"success":true,"count":1,"queries":[{"id":"6bce82c4-244","sql":"SELECT 1","token_id":7,"token_name":"admin","remote_addr":"127.0.0.1","status":"running","start_time":"2026-10-27T10:00:00Z","duration_ms":12.5,"is_parallel":false}]}`))
	})
	mux.HandleFunc("/api/v1/queries/history", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("limit"); got != "" && got != "2" {
			t.Errorf("history limit = %q", got)
		}
		_, _ = w.Write([]byte(`{"success":true,"count":1,"queries":[{"id":"6bce82c4-244","sql":"SELECT 1","token_name":"admin","remote_addr":"127.0.0.1","status":"completed","start_time":"2026-10-27T10:00:00Z","end_time":"2026-10-27T10:00:01Z","duration_ms":1000,"row_count":1,"is_parallel":false}]}`))
	})
	mux.HandleFunc("/api/v1/queries/"+commandTestQueryID, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			_, _ = w.Write([]byte(`{"success":true,"message":"Query cancelled"}`))
			return
		}
		_, _ = w.Write([]byte(`{"success":true,"query":{"id":"6bce82c4-244","sql":"SELECT 1","token_name":"admin","remote_addr":"127.0.0.1","status":"completed","start_time":"2026-10-27T10:00:00Z","end_time":"2026-10-27T10:00:01Z","duration_ms":1000,"row_count":1,"is_parallel":false}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestQueriesListFormatsAndShow(t *testing.T) {
	srv := fakeQueryManagementServer(t)
	writeTestConfig(t, srv.URL, "admin")
	out, _, err := execCmd(t, newQueriesActiveCmd())
	if err != nil || !strings.Contains(out, "running") || !strings.Contains(out, "SELECT 1") || !strings.Contains(out, "admin (7)") {
		t.Fatalf("active table = %q, err = %v", out, err)
	}
	out, _, err = execCmd(t, newQueriesActiveCmd(), "-o", "csv", "--no-header")
	if err != nil || !strings.HasPrefix(out, commandTestQueryID+",running,admin (7),127.0.0.1,2026-10-27T10:00:00Z,12.5,SELECT 1") {
		t.Fatalf("active CSV = %q, err = %v", out, err)
	}
	out, _, err = execCmd(t, newQueriesHistoryCmd(), "--limit", "2", "-o", "json")
	if err != nil || !strings.Contains(out, `"row_count": 1`) {
		t.Fatalf("history JSON = %q, err = %v", out, err)
	}
	out, _, err = execCmd(t, newQueriesShowCmd(), commandTestQueryID)
	if err != nil || !strings.Contains(out, "status") || !strings.Contains(out, "completed") || !strings.Contains(out, "SELECT 1") {
		t.Fatalf("show = %q, err = %v", out, err)
	}
	out, _, err = execCmd(t, newQueriesShowCmd(), commandTestQueryID, "-o", "json")
	if err != nil || !strings.Contains(out, `"query"`) {
		t.Fatalf("show JSON = %q, err = %v", out, err)
	}
}

func TestQueriesCancelConfirmsAndSupportsYes(t *testing.T) {
	requests := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/queries/"+commandTestQueryID, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s", r.Method)
		}
		_, _ = w.Write([]byte(`{"success":true,"message":"Query cancelled"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	writeTestConfig(t, srv.URL, "admin")
	c := newQueriesCancelCmd()
	c.SetIn(strings.NewReader("n\n"))
	_, stderr, err := execCmd(t, c, commandTestQueryID)
	if err == nil || !strings.Contains(stderr, `Cancel query "`+commandTestQueryID+`" on `+srv.URL+`? [y/N]`) || requests != 0 {
		t.Fatalf("declined cancel: stderr=%q err=%v requests=%d", stderr, err, requests)
	}
	out, _, err := execCmd(t, newQueriesCancelCmd(), commandTestQueryID, "--yes")
	if err != nil || !strings.Contains(out, "Query cancelled") || requests != 1 {
		t.Fatalf("--yes cancel: out=%q err=%v requests=%d", out, err, requests)
	}
}

func TestQueriesRejectInvalidInputsBeforeNetwork(t *testing.T) {
	cases := []struct {
		command func() *cobra.Command
		args    []string
		want    string
	}{
		{newQueriesShowCmd, []string{"../bad"}, "invalid query id"},
		{newQueriesCancelCmd, []string{"../bad", "--yes"}, "invalid query id"},
		{newQueriesHistoryCmd, []string{"--limit", "1001"}, "--limit must be between 1 and 1000"},
	}
	for _, tc := range cases {
		_, _, err := execCmd(t, tc.command(), append(tc.args, "--endpoint", "http://127.0.0.1:1")...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("args %v: err = %v, want %q", tc.args, err, tc.want)
		}
	}
}

func TestQueriesServerErrorsRemainDistinct(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/queries/active", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"success":false,"error":"Query management requires an enterprise license with the 'query_management' feature enabled"}`))
	})
	mux.HandleFunc("/api/v1/queries/history", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"success":false,"error":"Permission denied: admin required"}`))
	})
	mux.HandleFunc("/api/v1/queries/"+commandTestQueryID, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"success":false,"error":"Query already completed"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"success":false,"error":"Query not found"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	cases := []struct {
		name  string
		cmd   func() *cobra.Command
		args  []string
		token string
		want  string
	}{
		{"licence", newQueriesActiveCmd, nil, "admin", "HTTP 403"},
		{"admin", newQueriesHistoryCmd, nil, "reader", "admin required (HTTP 403)"},
		{"missing", newQueriesShowCmd, []string{commandTestQueryID}, "admin", "Query not found (HTTP 404)"},
		{"finished", newQueriesCancelCmd, []string{commandTestQueryID, "--yes"}, "admin", "Query already completed (HTTP 409)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			writeTestConfig(t, srv.URL, tc.token)
			_, _, err := execCmd(t, tc.cmd(), tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if tc.name == "licence" && !strings.Contains(err.Error(), "query_management") {
				t.Errorf("licence error lost its reason: %v", err)
			}
		})
	}
}

func TestAdminQueryRegistration(t *testing.T) {
	root := NewRoot(BuildInfo{})
	for _, action := range []string{"active", "history", "show", "cancel"} {
		cmd, _, err := root.Find([]string{"admin", "query", action})
		if err != nil || cmd.CommandPath() != "arcli admin query "+action {
			t.Fatalf("%s: cmd=%v err=%v", action, cmd, err)
		}
	}
	cmd, _, err := root.Find([]string{"query"})
	if err != nil || cmd.Use != newQueryCmd().Use {
		t.Errorf("SQL query command changed: %v %v", cmd, err)
	}
	for _, cmd := range root.Commands() {
		if cmd.Name() == "queries" {
			t.Error("legacy queries registered")
		}
	}
}

func TestManagedQueryPrecisionAndMultilineCSV(t *testing.T) {
	const stamp = "2026-10-27T03:04:05.123456789+02:30"
	const utc = "2026-10-27T00:34:05Z"
	const utcNano = "2026-10-27T00:34:05.123456789Z"
	const sql = "SELECT 1, 'quoted'\nFROM metrics\tWHERE x = 1"
	query := map[string]any{"id": commandTestQueryID, "sql": sql, "token_name": "admin", "status": "completed", "start_time": stamp, "end_time": stamp}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, commandTestQueryID) {
			_ = json.NewEncoder(w).Encode(map[string]any{"query": query})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"queries": []any{query}, "count": 1})
	}))
	defer srv.Close()
	writeTestConfig(t, srv.URL, "admin")
	for _, nano := range []bool{false, true} {
		var extra []string
		wantUTC := utc
		if nano {
			extra = []string{"--time-output", "rfc3339nano"}
			wantUTC = utcNano
		}
		for _, action := range []string{"active", "history"} {
			for _, format := range []string{"table", "csv", "json"} {
				out, _, err := execCmd(t, NewRoot(BuildInfo{}), append([]string{"admin", "query", action, "-o", format}, extra...)...)
				want := wantUTC
				if format == "json" {
					want = stamp
				}
				if err != nil || !strings.Contains(out, want) {
					t.Fatalf("%s/%s/nano=%t: %q %v", action, format, nano, out, err)
				}
				if !nano && format != "json" && strings.Contains(out, ".123456789") {
					t.Errorf("%s/%s: default output shows fractional seconds: %q", action, format, out)
				}
				if format == "csv" {
					rows, err := csv.NewReader(strings.NewReader(out)).ReadAll()
					if err != nil || len(rows) != 2 || rows[1][len(rows[1])-2] != sql {
						t.Errorf("CSV changed SQL: %v %v", rows, err)
					}
				}
			}
		}
		for _, format := range []string{"table", "json"} {
			out, _, err := execCmd(t, NewRoot(BuildInfo{}), append([]string{"admin", "query", "show", commandTestQueryID, "-o", format}, extra...)...)
			want := wantUTC
			if format == "json" {
				want = stamp
			}
			if err != nil || !strings.Contains(out, want) {
				t.Errorf("show/%s/nano=%t: %q %v", format, nano, out, err)
			}
		}
	}
}

func TestManagedQueryCSVHeadersAndUnsetTimes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"count":1,"queries":[{"id":"6bce82c4-244","sql":"SELECT 1","status":"running","start_time":"0001-01-01T00:00:00Z","duration_ms":2}]}`))
	}))
	defer srv.Close()
	writeTestConfig(t, srv.URL, "admin")
	for _, tc := range []struct {
		action, header, row string
	}{
		{"active", "id,status,token,remote_addr,start_time,duration_ms,sql,forwarded_to", commandTestQueryID + ",running,,,,2,SELECT 1,"},
		{"history", "id,status,token,remote_addr,start_time,end_time,duration_ms,row_count,sql,forwarded_to", commandTestQueryID + ",running,,,,,2,0,SELECT 1,"},
	} {
		out, _, err := execCmd(t, NewRoot(BuildInfo{}), "admin", "query", tc.action, "-o", "csv")
		if err != nil || out != tc.header+"\n"+tc.row+"\n" {
			t.Errorf("%s CSV = %q, err = %v", tc.action, out, err)
		}
		out, _, err = execCmd(t, NewRoot(BuildInfo{}), "admin", "query", tc.action)
		if err != nil || !strings.Contains(out, "STARTED") || !strings.Contains(out, "DURATION MS") {
			t.Errorf("%s table headers changed: %q, err = %v", tc.action, out, err)
		}
	}
}

func TestManagedQueryRenderingRealFields(t *testing.T) {
	const payload = `{"success":true,"queries":[{"id":"dadf4a56-099","sql":"SELECT\nlong\tstatement","status":"running","start_time":"2026-11-11T01:00:00Z","forwarded_to":"reader-1"},{"id":"c0f60411-9a6","sql":"SELECT 1","status":"running","start_time":"2026-11-11T00:00:00Z"}],"count":2}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(payload)) }))
	defer srv.Close()
	writeTestConfig(t, srv.URL, "admin")
	for _, format := range []string{"table", "csv"} {
		out, _, err := execCmd(t, newQueriesActiveCmd(), "-o", format)
		if err != nil || !strings.Contains(out, "reader-1") || strings.Index(out, "c0f60411-9a6") > strings.Index(out, "dadf4a56-099") {
			t.Fatalf("%s: %q %v", format, out, err)
		}
		if format == "table" && !strings.Contains(out, "SELECT long statement") {
			t.Errorf("SQL words glued: %q", out)
		}
	}
	out, _, err := execCmd(t, newQueriesActiveCmd(), "-o", "json")
	if err != nil || strings.Index(out, "c0f60411-9a6") < strings.Index(out, "dadf4a56-099") {
		t.Errorf("raw JSON reordered: %q %v", out, err)
	}
	if got := managedQuerySQL(strings.Repeat("世", 130), 120); len([]rune(got)) != 120 || !strings.HasSuffix(got, "…") {
		t.Errorf("truncation: %q", got)
	}
	if got := managedQuerySQL(strings.Repeat("x", 200), 0); len(got) != 200 {
		t.Errorf("show truncated: %q", got)
	}
}

func TestQueryCancelNotFoundNodeHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"success":false,"error":"Query not found"}`))
	}))
	defer srv.Close()
	writeTestConfig(t, srv.URL, "admin")
	_, _, err := execCmd(t, newQueriesCancelCmd(), commandTestQueryID, "--yes")
	if err == nil || !strings.Contains(err.Error(), "target the node that runs the query") {
		t.Fatalf("error: %v", err)
	}
}

func TestQueryCancelConfirmationInputs(t *testing.T) {
	const nonInteractive = "confirmation required but stdin is not a terminal; pass --yes"
	for _, tc := range []struct {
		input   string
		yes     bool
		wantErr string
	}{
		{input: "decline", wantErr: "aborted"},
		{input: "eof", wantErr: "aborted"},
		{input: "pipe", wantErr: nonInteractive},
		{input: "file", wantErr: nonInteractive},
		{input: "null", wantErr: nonInteractive},
		{input: "accept"},
		{input: "decline", yes: true},
		{input: "pipe", yes: true},
		{input: "null", yes: true},
	} {
		name := tc.input
		if tc.yes {
			name += "/--yes"
		}
		t.Run(name, func(t *testing.T) {
			requests := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Method != http.MethodDelete || r.URL.Path != "/api/v1/queries/"+commandTestQueryID {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
				}
				_, _ = w.Write([]byte(`{"success":true,"message":"Query cancelled"}`))
			}))
			defer srv.Close()
			writeTestConfig(t, srv.URL, "admin")
			args := []string{"admin", "query", "cancel", commandTestQueryID}
			if tc.yes {
				args = append(args, "--yes")
			}
			root := NewRoot(BuildInfo{Version: "test"})
			root.SetIn(confirmationInput(t, tc.input))
			out, errOut, err := execCmd(t, root, args...)
			wantOut, wantRequests := "Query cancelled ("+commandTestQueryID+")\n", 1
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				wantOut, wantRequests = "", 0
			} else if err != nil {
				t.Fatal(err)
			}
			if out != wantOut {
				t.Errorf("stdout = %q, want %q", out, wantOut)
			}
			wantPrompt := ""
			if !tc.yes && (tc.input == "decline" || tc.input == "eof" || tc.input == "accept") {
				wantPrompt = "Cancel query \"" + commandTestQueryID + "\" on " + srv.URL + "? [y/N] "
			}
			if errOut != wantPrompt {
				t.Errorf("stderr = %q, want %q", errOut, wantPrompt)
			}
			if requests != wantRequests {
				t.Errorf("requests = %d, want %d", requests, wantRequests)
			}
		})
	}
}

// List tables shorten long SQL; show, CSV and JSON keep it whole.
func TestManagedQueryLongSQLTruncatedOnlyInListTables(t *testing.T) {
	long := "SELECT host, avg(usage) AS avg_usage, max(usage) AS max_usage, min(usage) AS min_usage, count(*) AS samples_in_window_total\nFROM cpu\nWHERE usage > 1\nGROUP BY host ORDER BY host"
	oneLine := strings.ReplaceAll(long, "\n", " ")
	query := map[string]any{"id": commandTestQueryID, "sql": long, "token_id": 1, "token_name": "admin", "remote_addr": "127.0.0.1", "status": "completed", "start_time": "2026-11-11T22:41:42Z", "end_time": "2026-11-11T22:41:42Z", "duration_ms": 29, "row_count": 10, "is_parallel": false}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/"+commandTestQueryID) {
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "query": query})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "queries": []any{query}, "count": 1})
	}))
	defer srv.Close()
	writeTestConfig(t, srv.URL, "admin")
	for _, action := range []string{"active", "history"} {
		out, _, err := execCmd(t, NewRoot(BuildInfo{}), "admin", "query", action)
		if err != nil || !strings.Contains(out, string([]rune(oneLine)[:119])+"…") || strings.Contains(out, "ORDER BY host") {
			t.Errorf("%s table did not shorten SQL to 120 characters: %q %v", action, out, err)
		}
		out, _, err = execCmd(t, NewRoot(BuildInfo{}), "admin", "query", action, "-o", "csv")
		if err != nil || !strings.Contains(out, long) {
			t.Errorf("%s CSV lost SQL: %q %v", action, out, err)
		}
	}
	out, _, err := execCmd(t, NewRoot(BuildInfo{}), "admin", "query", "show", commandTestQueryID)
	if err != nil || !strings.Contains(out, oneLine) || strings.Contains(out, "…") {
		t.Errorf("show shortened SQL: %q %v", out, err)
	}
}

func TestManagedQueryEndpointDropsCredentials(t *testing.T) {
	for in, want := range map[string]string{
		"http://127.0.0.1:18000":                         "http://127.0.0.1:18000",
		"https://user:secret@arc.example.com:8000/x?t=1": "https://arc.example.com:8000",
	} {
		if got := managedQueryEndpoint(in); got != want {
			t.Errorf("managedQueryEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}
