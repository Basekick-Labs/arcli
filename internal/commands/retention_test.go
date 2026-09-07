package commands

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"

	"github.com/basekick-labs/arcli/internal/client"
)

// fakeRetentionCQServer emulates the retention, CQ, and scheduler
// routes with in-memory tables. Token "tok" is admin; "reader" is not.
type fakeRCServer struct {
	mu       sync.Mutex
	policies map[int64]client.RetentionPolicy
	cqs      map[int64]client.ContinuousQuery
	nextID   int64
	execs    map[int64][]client.RetentionExecution
	lastPut  map[string]json.RawMessage
	srv      *httptest.Server
}

func newFakeRCServer(t *testing.T) *fakeRCServer {
	t.Helper()
	f := &fakeRCServer{policies: map[int64]client.RetentionPolicy{}, cqs: map[int64]client.ContinuousQuery{}, nextID: 1, execs: map[int64][]client.RetentionExecution{}, lastPut: map[string]json.RawMessage{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok","time":"t","uptime":"1s"}`))
	})
	mux.HandleFunc("/api/v1/schedulers/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"cq_scheduler":{"enabled":false,"reason":"Enterprise license required"},"retention_scheduler":{"enabled":false,"reason":"Enterprise license required"}}`))
	})
	admin := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(403)
			_, _ = w.Write([]byte(`{"success":false,"error":"Permission denied: admin required"}`))
			return false
		}
		return true
	}
	idOf := func(rest string) (int64, string) {
		parts := strings.SplitN(rest, "/", 2)
		id, _ := strconv.ParseInt(parts[0], 10, 64)
		action := ""
		if len(parts) == 2 {
			action = parts[1]
		}
		return id, action
	}
	mux.HandleFunc("/api/v1/retention/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		rest := strings.TrimPrefix(r.URL.Path, "/api/v1/retention/")
		if rest == "" {
			switch r.Method {
			case http.MethodGet:
				if len(f.policies) == 0 {
					_, _ = w.Write([]byte("null"))
					return
				}
				list := []client.RetentionPolicy{}
				for _, p := range f.policies {
					list = append(list, p)
				}
				_ = json.NewEncoder(w).Encode(list)
			case http.MethodPost:
				if !admin(w, r) {
					return
				}
				var req client.RetentionPolicyRequest
				_ = json.NewDecoder(r.Body).Decode(&req)
				for _, p := range f.policies {
					if p.Name == req.Name {
						w.WriteHeader(400)
						_, _ = w.Write([]byte(`{"error":"Retention policy with name '` + req.Name + `' already exists"}`))
						return
					}
				}
				p := client.RetentionPolicy{ID: f.nextID, Name: req.Name, Database: req.Database, Measurement: req.Measurement, RetentionDays: req.RetentionDays, BufferDays: req.BufferDays, IsActive: req.IsActive, CreatedAt: "2026-09-07T19:00:00Z", UpdatedAt: "2026-09-07T19:00:00Z"}
				f.nextID++
				f.policies[p.ID] = p
				w.WriteHeader(201)
				_ = json.NewEncoder(w).Encode(p)
			}
			return
		}
		id, action := idOf(rest)
		p, ok := f.policies[id]
		if !ok {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":"Retention policy not found"}`))
			return
		}
		switch {
		case r.Method == http.MethodGet && action == "":
			_ = json.NewEncoder(w).Encode(p)
		case r.Method == http.MethodPut:
			if !admin(w, r) {
				return
			}
			raw, _ := readAll(r)
			f.lastPut["retention"] = raw
			var req client.RetentionPolicyRequest
			_ = json.Unmarshal(raw, &req)
			p.Name, p.Database, p.Measurement, p.RetentionDays, p.BufferDays, p.IsActive = req.Name, req.Database, req.Measurement, req.RetentionDays, req.BufferDays, req.IsActive
			f.policies[id] = p
			_ = json.NewEncoder(w).Encode(p)
		case r.Method == http.MethodDelete:
			if !admin(w, r) {
				return
			}
			delete(f.policies, id)
			_, _ = w.Write([]byte(`{"message":"Retention policy deleted successfully"}`))
		case r.Method == http.MethodPost && action == "execute":
			if !admin(w, r) {
				return
			}
			var req struct {
				DryRun  bool `json:"dry_run"`
				Confirm bool `json:"confirm"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if !p.IsActive {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"error":"Retention policy is not active"}`))
				return
			}
			if !req.DryRun && !req.Confirm {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"error":"Confirmation required for retention policy execution. Set confirm=true"}`))
				return
			}
			if !req.DryRun {
				f.execs[id] = append(f.execs[id], client.RetentionExecution{ID: 1, PolicyID: id, ExecutionTime: "2026-09-07T19:05:00Z", Status: "completed", DeletedCount: 42, ExecutionDurationMs: 1500})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"policy_id": id, "policy_name": p.Name, "deleted_count": 42, "files_deleted": 3, "execution_time_ms": 1500, "dry_run": req.DryRun, "cutoff_date": "2026-08-01T00:00:00Z", "affected_measurements": []string{"cpu", "mem"}})
		case r.Method == http.MethodGet && action == "executions":
			ex := f.execs[id]
			if len(ex) == 0 {
				_, _ = w.Write([]byte(`{"executions":null,"policy_id":` + strconv.FormatInt(id, 10) + `}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"policy_id": id, "executions": ex})
		}
	})
	mux.HandleFunc("/api/v1/continuous_queries/", func(w http.ResponseWriter, r *http.Request) {
		if !admin(w, r) {
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		rest := strings.TrimPrefix(r.URL.Path, "/api/v1/continuous_queries/")
		if rest == "" {
			switch r.Method {
			case http.MethodGet:
				if len(f.cqs) == 0 {
					_, _ = w.Write([]byte("null"))
					return
				}
				list := []client.ContinuousQuery{}
				for _, q := range f.cqs {
					if db := r.URL.Query().Get("database"); db != "" && q.Database != db {
						continue
					}
					if ia := r.URL.Query().Get("is_active"); ia != "" && q.IsActive != (ia == "true") {
						continue
					}
					list = append(list, q)
				}
				_ = json.NewEncoder(w).Encode(list)
			case http.MethodPost:
				var req client.ContinuousQueryRequest
				_ = json.NewDecoder(r.Body).Decode(&req)
				q := client.ContinuousQuery{ID: f.nextID, Name: req.Name, Description: req.Description, Database: req.Database, SourceMeasurement: req.SourceMeasurement, DestinationMeasurement: req.DestinationMeasurement, Query: req.Query, Interval: req.Interval, IsActive: req.IsActive, CreatedAt: "2026-09-07T19:00:00Z", UpdatedAt: "2026-09-07T19:00:00Z"}
				if len(req.TagColumns) > 0 {
					q.TagColumns = req.TagColumns
				}
				f.nextID++
				f.cqs[q.ID] = q
				w.WriteHeader(201)
				_ = json.NewEncoder(w).Encode(q)
			}
			return
		}
		id, action := idOf(rest)
		q, ok := f.cqs[id]
		if !ok {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":"Continuous query not found"}`))
			return
		}
		switch {
		case r.Method == http.MethodGet && action == "":
			_ = json.NewEncoder(w).Encode(q)
		case r.Method == http.MethodPut:
			raw, _ := readAll(r)
			f.lastPut["cq"] = raw
			var req client.ContinuousQueryRequest
			_ = json.Unmarshal(raw, &req)
			q.Name, q.Description, q.Database, q.SourceMeasurement, q.DestinationMeasurement, q.Query, q.Interval, q.IsActive = req.Name, req.Description, req.Database, req.SourceMeasurement, req.DestinationMeasurement, req.Query, req.Interval, req.IsActive
			q.TagColumns = nil
			if len(req.TagColumns) > 0 {
				q.TagColumns = req.TagColumns
			}
			f.cqs[id] = q
			_ = json.NewEncoder(w).Encode(q)
		case r.Method == http.MethodDelete:
			delete(f.cqs, id)
			_, _ = w.Write([]byte(`{"message":"Continuous query deleted successfully"}`))
		case r.Method == http.MethodPost && action == "execute":
			var req struct {
				DryRun bool `json:"dry_run"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			status := "completed"
			eq := ""
			if req.DryRun {
				status, eq = "dry_run", "SELECT 1\nFROM d.cpu"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"query_id": id, "query_name": q.Name, "execution_id": "cq-exec-1", "status": status, "start_time": "2026-09-07T18:00:00Z", "end_time": "2026-09-07T19:00:00Z", "records_read": nil, "records_written": 5, "execution_time_seconds": 0.25, "destination_measurement": q.DestinationMeasurement, "dry_run": req.DryRun, "executed_at": "2026-09-07T19:00:00Z", "executed_query": eq})
		case r.Method == http.MethodGet && action == "executions":
			_, _ = w.Write([]byte(`{"executions":null,"query_id":` + strconv.FormatInt(id, 10) + `}`))
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func readAll(r *http.Request) ([]byte, error) { return io.ReadAll(r.Body) }

const validCQSQL = "SELECT time_bucket(INTERVAL '1 minute', time) AS time, host, avg(usage) AS usage\nFROM d.cpu WHERE time >= {start_time} AND time < {end_time} GROUP BY 1, 2"

func TestRetention_ListEmptyAndCreateWithHint(t *testing.T) {
	f := newFakeRCServer(t)
	writeTestConfig(t, f.srv.URL, "tok")
	out, _, err := execCmd(t, newRetentionListCmd())
	if err != nil || !strings.Contains(out, "(no retention policies)") {
		t.Errorf("err=%v out=%q", err, out)
	}
	out, _, _ = execCmd(t, newRetentionListCmd(), "-o", "json")
	if strings.TrimSpace(out) != "[]" {
		t.Errorf("json empty list = %q", out)
	}
	out, errOut, err := execCmd(t, newRetentionCreateCmd(), "--name", "p", "--database", "d", "--retention-days", "30", "--buffer-days", "7")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `Created retention policy "p" (id 1)`) || !strings.Contains(out, "retention:      30 days (+7 buffer)") || !strings.Contains(out, "active:         true") {
		t.Errorf("out = %s", out)
	}
	if !strings.Contains(errOut, "retention scheduler is not running on this server (Enterprise license required)") {
		t.Errorf("hint missing: %q", errOut)
	}
	_, _, err = execCmd(t, newRetentionCreateCmd(), "--name", "q", "--database", "d", "--retention-days", "5", "--buffer-days", "7")
	if err == nil || !strings.Contains(err.Error(), "must be greater than buffer-days") {
		t.Errorf("err = %v", err)
	}
	_, _, err = execCmd(t, newRetentionCreateCmd(), "--name", "p", "--database", "d", "--retention-days", "30")
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("duplicate: err = %v", err)
	}
}

func TestRetention_UpdateReadMergeWrite(t *testing.T) {
	f := newFakeRCServer(t)
	writeTestConfig(t, f.srv.URL, "tok")
	if _, _, err := execCmd(t, newRetentionCreateCmd(), "--name", "p", "--database", "d", "--retention-days", "30", "--buffer-days", "7", "--measurement", "cpu"); err != nil {
		t.Fatal(err)
	}
	_, _, err := execCmd(t, newRetentionUpdateCmd(), "p")
	if err == nil || !strings.Contains(err.Error(), "nothing to update") {
		t.Errorf("err = %v", err)
	}
	out, _, err := execCmd(t, newRetentionUpdateCmd(), "p", "--buffer-days", "3")
	if err != nil {
		t.Fatal(err)
	}
	var put map[string]any
	_ = json.Unmarshal(f.lastPut["retention"], &put)
	if put["name"] != "p" || put["database"] != "d" || put["measurement"] != "cpu" || put["retention_days"] != float64(30) || put["buffer_days"] != float64(3) || put["is_active"] != true {
		t.Errorf("PUT must carry the merged full object: %v", put)
	}
	if !strings.Contains(out, "measurement:    cpu") {
		t.Errorf("out = %s", out)
	}
	if _, _, err := execCmd(t, newRetentionUpdateCmd(), "1", "--measurement", ""); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(f.lastPut["retention"], &put)
	if put["measurement"] != nil {
		t.Errorf(`--measurement "" must send null, got %v`, put["measurement"])
	}
	// A merge that violates the rules names the stored values.
	_, _, err = execCmd(t, newRetentionUpdateCmd(), "p", "--buffer-days", "40")
	if err == nil || !strings.Contains(err.Error(), "stored: retention-days 30") {
		t.Errorf("err = %v", err)
	}
	// Rename collision is caught before the PUT.
	if _, _, err := execCmd(t, newRetentionCreateCmd(), "--name", "other", "--database", "d", "--retention-days", "1"); err != nil {
		t.Fatal(err)
	}
	_, _, err = execCmd(t, newRetentionUpdateCmd(), "p", "--name", "other")
	if err == nil || !strings.Contains(err.Error(), `already exists (id 2)`) {
		t.Errorf("err = %v", err)
	}
	_, _, err = execCmd(t, newRetentionUpdateCmd(), "p", "--active", "--inactive")
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("err = %v", err)
	}
}

func TestRetention_ExecutePreflightPromptAndDryRun(t *testing.T) {
	f := newFakeRCServer(t)
	writeTestConfig(t, f.srv.URL, "tok")
	if _, _, err := execCmd(t, newRetentionCreateCmd(), "--name", "p", "--database", "d", "--retention-days", "30"); err != nil {
		t.Fatal(err)
	}
	out, _, err := execCmd(t, newRetentionExecuteCmd(), "p", "--dry-run")
	if err != nil || !strings.Contains(out, "Would delete 3 files (42 rows) older than 2026-08-01T00:00:00Z") || !strings.Contains(out, "measurements: cpu, mem") {
		t.Errorf("err=%v out=%s", err, out)
	}
	// Interactive "n": preflight ran, prompt carries the server numbers, nothing executed.
	c := newRetentionExecuteCmd()
	c.SetIn(strings.NewReader("n\n"))
	_, errOut, err := execCmd(t, c, "p")
	if err == nil || err.Error() != "aborted" {
		t.Errorf("err = %v", err)
	}
	if !strings.Contains(errOut, "Delete 3 files (42 rows) whose newest row is older than 2026-08-01T00:00:00Z") {
		t.Errorf("prompt = %q", errOut)
	}
	if len(f.execs[1]) != 0 {
		t.Error("nothing must be executed after N")
	}
	// A non-TTY stdin is refused before the preflight scan.
	fh, _ := os.CreateTemp(t.TempDir(), "stdin")
	defer fh.Close()
	c2 := newRetentionExecuteCmd()
	c2.SetIn(fh)
	_, _, err = execCmd(t, c2, "p")
	if err == nil || !strings.Contains(err.Error(), "stdin is not a terminal") {
		t.Errorf("err = %v", err)
	}
	devnull, _ := os.Open(os.DevNull)
	defer devnull.Close()
	c3 := newRetentionExecuteCmd()
	c3.SetIn(devnull)
	_, _, err = execCmd(t, c3, "p")
	if err == nil || !strings.Contains(err.Error(), "stdin is not a terminal") {
		t.Errorf("/dev/null: err = %v", err)
	}
	out, _, err = execCmd(t, newRetentionExecuteCmd(), "p", "--yes")
	if err != nil || !strings.Contains(out, "Deleted 3 files (42 rows)") {
		t.Errorf("err=%v out=%s", err, out)
	}
	out, _, err = execCmd(t, newRetentionExecutionsCmd(), "p")
	if err != nil || !strings.Contains(out, "completed") || !strings.Contains(out, "1.5s") {
		t.Errorf("err=%v out=%s", err, out)
	}
	if _, _, err := execCmd(t, newRetentionUpdateCmd(), "p", "--inactive"); err != nil {
		t.Fatal(err)
	}
	_, _, err = execCmd(t, newRetentionExecuteCmd(), "p", "--yes")
	if err == nil || !strings.Contains(err.Error(), "not active") {
		t.Errorf("err = %v", err)
	}
}

func TestRetention_TimeoutDefaultsAndNonAdmin(t *testing.T) {
	c := newRetentionExecuteCmd()
	if def := c.Flags().Lookup("timeout").DefValue; def != "30m0s" {
		t.Errorf("retention execute --timeout default = %s", def)
	}
	if def := newCQExecuteCmd().Flags().Lookup("timeout").DefValue; def != "10m0s" {
		t.Errorf("cq execute --timeout default = %s", def)
	}
	if def := newRetentionListCmd().Flags().Lookup("timeout").DefValue; def != "1m0s" {
		t.Errorf("list --timeout default = %s", def)
	}
	f := newFakeRCServer(t)
	writeTestConfig(t, f.srv.URL, "reader")
	if _, _, err := execCmd(t, newRetentionListCmd()); err != nil {
		t.Errorf("non-admin list must work: %v", err)
	}
	_, _, err := execCmd(t, newRetentionCreateCmd(), "--name", "p", "--database", "d", "--retention-days", "1")
	if err == nil || !strings.Contains(err.Error(), "admin required") {
		t.Errorf("err = %v", err)
	}
	_, _, err = execCmd(t, newCQListCmd())
	if err == nil || !strings.Contains(err.Error(), "admin required") {
		t.Errorf("cq list non-admin: err = %v", err)
	}
}

func TestCQ_CreateValidationAndQueryFile(t *testing.T) {
	f := newFakeRCServer(t)
	writeTestConfig(t, f.srv.URL, "tok")
	base := []string{"--name", "cq", "--database", "d", "--source", "cpu", "--destination", "cpu_1m", "--interval", "1m"}
	_, _, err := execCmd(t, newCQCreateCmd(), base...)
	if err == nil || !strings.Contains(err.Error(), "one of --query or --query-file is required") {
		t.Errorf("err = %v", err)
	}
	_, _, err = execCmd(t, newCQCreateCmd(), append(base, "--query", "SELECT 1")...)
	if err == nil || !strings.Contains(err.Error(), "placeholders") {
		t.Errorf("err = %v", err)
	}
	_, _, err = execCmd(t, newCQCreateCmd(), append(base[:8], "--interval", "5", "--query", validCQSQL)...)
	if err == nil || !strings.Contains(err.Error(), "invalid interval") {
		t.Errorf("err = %v", err)
	}
	qf := filepath.Join(t.TempDir(), "q.sql")
	_ = os.WriteFile(qf, []byte(validCQSQL+"\n"), 0o600)
	out, errOut, err := execCmd(t, newCQCreateCmd(), append(base, "--query-file", qf, "--tag-column", "host")...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `Created continuous query "cq" (id 1)`) || !strings.Contains(out, "tag columns:     host") || !strings.Contains(out, "\nFROM d.cpu") {
		t.Errorf("out = %s", out)
	}
	if strings.Contains(errOut, "does not contain") || !strings.Contains(errOut, "cq scheduler is not running") {
		t.Errorf("stderr = %q", errOut)
	}
	_, errOut, err = execCmd(t, newCQCreateCmd(), "--name", "cq2", "--database", "d", "--source", "cpu", "--destination", "cpu_5m", "--interval", "5m", "--query", "SELECT 1 FROM cpu WHERE {start_time} {end_time}")
	if err != nil || !strings.Contains(errOut, "does not contain `FROM d.cpu`") {
		t.Errorf("err=%v stderr=%q", err, errOut)
	}
	bin := filepath.Join(t.TempDir(), "bin.sql")
	_ = os.WriteFile(bin, []byte("SELECT 1 FROM d.cpu WHERE {start_time} {end_time} \xff"), 0o600)
	_, _, err = execCmd(t, newCQCreateCmd(), append(base, "--query-file", bin)...)
	if err == nil || !strings.Contains(err.Error(), "not valid UTF-8") {
		t.Errorf("err = %v", err)
	}
	big := filepath.Join(t.TempDir(), "big.sql")
	_ = os.WriteFile(big, []byte(strings.Repeat("x", 70000)), 0o600)
	_, _, err = execCmd(t, newCQCreateCmd(), append(base, "--query-file", big)...)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("err = %v", err)
	}
}

func TestCQ_UpdateMergeClearTagsAndExecute(t *testing.T) {
	f := newFakeRCServer(t)
	writeTestConfig(t, f.srv.URL, "tok")
	if _, _, err := execCmd(t, newCQCreateCmd(), "--name", "cq", "--database", "d", "--source", "cpu", "--destination", "cpu_1m", "--interval", "1m", "--tag-column", "host", "--query", validCQSQL); err != nil {
		t.Fatal(err)
	}
	out, _, err := execCmd(t, newCQUpdateCmd(), "cq", "--interval", "5m")
	if err != nil || !strings.Contains(out, "interval:        5m") || !strings.Contains(out, "tag columns:     host") {
		t.Errorf("err=%v out=%s", err, out)
	}
	var put map[string]any
	_ = json.Unmarshal(f.lastPut["cq"], &put)
	if put["query"] != validCQSQL || put["source_measurement"] != "cpu" {
		t.Errorf("PUT must carry the merged full object: %v", put)
	}
	out, _, err = execCmd(t, newCQUpdateCmd(), "1", "--clear-tag-columns", "--description", "")
	if err != nil || !strings.Contains(out, "tag columns:     -") {
		t.Errorf("err=%v out=%s", err, out)
	}
	_ = json.Unmarshal(f.lastPut["cq"], &put)
	if tc, _ := put["tag_columns"].([]any); len(tc) != 0 || put["description"] != nil {
		t.Errorf("clear must send [] and null: %v", put)
	}
	_, _, err = execCmd(t, newCQUpdateCmd(), "cq", "--interval", "bogus")
	if err == nil || !strings.Contains(err.Error(), "merged query is invalid") {
		t.Errorf("err = %v", err)
	}
	out, _, err = execCmd(t, newCQExecuteCmd(), "cq", "--dry-run")
	if err != nil || !strings.Contains(out, "status:       dry_run") || !strings.Contains(out, "query:\nSELECT 1\nFROM d.cpu") {
		t.Errorf("err=%v out=%s", err, out)
	}
	out, _, err = execCmd(t, newCQExecuteCmd(), "cq")
	if err != nil || !strings.Contains(out, "written:      5 records") || !strings.Contains(out, "2026-09-07T18:00:00Z → 2026-09-07T19:00:00Z") {
		t.Errorf("err=%v out=%s", err, out)
	}
	_, _, err = execCmd(t, newCQExecuteCmd(), "cq", "--start", "2026-09-02T00:00:00Z", "--end", "2026-09-01T00:00:00Z")
	if err == nil || !strings.Contains(err.Error(), "--start must be before --end") {
		t.Errorf("err = %v", err)
	}
	out, _, err = execCmd(t, newCQExecutionsCmd(), "cq")
	if err != nil || !strings.Contains(out, "(no executions") {
		t.Errorf("err=%v out=%s", err, out)
	}
	out, _, err = execCmd(t, newCQDeleteCmd(), "cq", "--yes")
	if err != nil || !strings.Contains(out, "Deleted continuous query") {
		t.Errorf("err=%v out=%s", err, out)
	}
}

func TestCQ_ExecuteRewindWarning(t *testing.T) {
	f := newFakeRCServer(t)
	writeTestConfig(t, f.srv.URL, "tok")
	lp := "2026-09-07T12:00:00Z"
	f.cqs[9] = client.ContinuousQuery{ID: 9, Name: "old", Database: "d", SourceMeasurement: "cpu", DestinationMeasurement: "x", Query: validCQSQL, Interval: "1m", IsActive: true, LastProcessedTime: &lp}
	_, errOut, err := execCmd(t, newCQExecuteCmd(), "old", "--start", "2026-09-01T00:00:00Z", "--end", "2026-09-02T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut, "earlier than the query's last processed time 2026-09-07T12:00:00Z") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestSchedulerStatus_OSS(t *testing.T) {
	f := newFakeRCServer(t)
	writeTestConfig(t, f.srv.URL, "reader")
	out, _, err := execCmd(t, newSchedulerStatusCmd())
	if err != nil || !strings.Contains(out, "continuous queries:  not running (Enterprise license required)") || !strings.Contains(out, "retention:           not running") {
		t.Errorf("err=%v out=%s", err, out)
	}
}

func TestDescribeScheduler_RunningShapes(t *testing.T) {
	can := true
	b := client.SchedulerBlock{Running: true, LicenseValid: true, Schedule: "0 2 * * *", NextRun: "2026-09-08T02:00:00Z", CanRun: &can, GateRole: "writer"}
	if got := describeScheduler(b, false); got != "running, schedule 0 2 * * *, next run 2026-09-08T02:00:00Z, can run on this node: true (writer)" {
		t.Errorf("got %q", got)
	}
	b2 := client.SchedulerBlock{Running: false, LicenseValid: false, JobCount: 3}
	if got := describeScheduler(b2, true); got != "present but not running, license invalid, 3 scheduled job(s)" {
		t.Errorf("got %q", got)
	}
}

func TestFeatureDisabled_Commands(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok","time":"t","uptime":"1s"}`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":"Cannot ` + r.Method + ` ` + r.URL.Path + `"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	writeTestConfig(t, srv.URL, "tok")
	for name, c := range map[string]func() *cobra.Command{"retention list": newRetentionListCmd, "cq list": newCQListCmd} {
		_, _, err := execCmd(t, c())
		if err == nil || !strings.Contains(err.Error(), "are disabled on this server") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestFmtServerTime(t *testing.T) {
	if got := fmtServerTime("2026-09-07T13:05:00-06:00", "-"); got != "2026-09-07T19:05:00Z" {
		t.Errorf("got %q", got)
	}
	if got := fmtServerTime("0001-01-01T00:00:00Z", "never"); got != "never" {
		t.Errorf("zero: got %q", got)
	}
	if got := fmtServerTime("", "never"); got != "never" {
		t.Errorf("empty: got %q", got)
	}
	if got := fmtServerTime("not a time", "-"); got != "not a time" {
		t.Errorf("unparseable: got %q", got)
	}
}
