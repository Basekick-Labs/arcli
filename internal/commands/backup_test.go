package commands

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeDeleteServer emulates POST /api/v1/delete/, its config, and the
// query endpoint used to disambiguate zero matches.
type fakeDeleteServer struct {
	mu       sync.Mutex
	requests []map[string]any
	rows     int64 // rows matching a "real" predicate
	srv      *httptest.Server
}

func newFakeDeleteServer(t *testing.T, enabled bool) *fakeDeleteServer {
	t.Helper()
	f := &fakeDeleteServer{rows: 7}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/delete/config", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"enabled":true,"confirmation_threshold":10000,"max_rows_per_delete":1000000,"implementation":"rewrite-based","performance_impact":{}}`))
	})
	mux.HandleFunc("/api/v1/query", func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			SQL string `json:"sql"`
		}
		_ = json.NewDecoder(r.Body).Decode(&q)
		if strings.Contains(q.SQL, "nosuchcol") {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"success":false,"error":"Binder Error: Referenced column \"nosuchcol\" not found"}`))
			return
		}
		_, _ = w.Write([]byte(`{"success":true,"columns":["count_star()"],"data":[[0]],"row_count":1}`))
	})
	mux.HandleFunc("/api/v1/delete/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(403)
			_, _ = w.Write([]byte(`{"success":false,"error":"Permission denied: admin required"}`))
			return
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.requests = append(f.requests, req)
		if !enabled {
			w.WriteHeader(403)
			_, _ = w.Write([]byte(`{"success":false,"deleted_count":0,"affected_files":0,"rewritten_files":0,"execution_time_ms":0,"dry_run":false,"files_processed":null,"error":"Delete operations are disabled. Set delete.enabled=true in arc.toml to enable."}`))
			return
		}
		where, _ := req["where"].(string)
		dry, _ := req["dry_run"].(bool)
		switch {
		case strings.Contains(where, "nosuchcol") || strings.Contains(where, "nomatch"):
			_, _ = w.Write([]byte(`{"success":true,"deleted_count":0,"affected_files":0,"rewritten_files":0,"execution_time_ms":1,"dry_run":` + boolS(dry) + `,"files_processed":[]}`))
		case strings.Contains(where, "partial"):
			w.WriteHeader(207)
			_, _ = w.Write([]byte(`{"success":false,"deleted_count":4,"affected_files":2,"rewritten_files":1,"execution_time_ms":9,"dry_run":false,"files_processed":["a.parquet"],"failed_files":["b.parquet"],"error":"1 of 2 files failed to process"}`))
		default:
			n := f.rows
			if !dry {
				f.rows = 0
			}
			_, _ = w.Write([]byte(`{"success":true,"deleted_count":` + itoa(n) + `,"affected_files":2,"rewritten_files":1,"execution_time_ms":17,"dry_run":` + boolS(dry) + `,"files_processed":["a.parquet","b.parquet"]}`))
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func boolS(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func TestDelete_FlagValidationBeforeHTTP(t *testing.T) {
	writeTestConfig(t, unreachable, "tok")
	base := []string{"--database", "d", "--measurement", "cpu"}
	_, _, err := execCmd(t, newDeleteCmd(), base...)
	if err == nil || !strings.Contains(err.Error(), `required flag(s) "where" not set`) {
		t.Errorf("err = %v", err)
	}
	for _, bad := range []string{"", "a; b", "x -- y", "host = 'a"} {
		_, _, err := execCmd(t, newDeleteCmd(), append(base, "--where", bad, "--dry-run")...)
		if err == nil {
			t.Errorf("where %q should fail client-side", bad)
		}
	}
	_, _, err = execCmd(t, newDeleteCmd(), "--database", "a/b", "--measurement", "cpu", "--where", "1=1", "--dry-run")
	if err == nil || !strings.Contains(err.Error(), "invalid characters") {
		t.Errorf("err = %v", err)
	}
}

func TestDelete_DryRunSendsNormalisedWhereWithConfirm(t *testing.T) {
	f := newFakeDeleteServer(t, true)
	writeTestConfig(t, f.srv.URL, "tok")
	out, _, err := execCmd(t, newDeleteCmd(), "--database", "d", "--measurement", "cpu", "--where", "WHERE host = 'a'", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Would delete 7 rows from d.cpu across 2 file(s)") {
		t.Errorf("out = %q", out)
	}
	req := f.requests[0]
	if req["where"] != "(host = 'a') IS TRUE" || req["dry_run"] != true || req["confirm"] != true {
		t.Errorf("request = %v", req)
	}
}

func TestDelete_InteractivePreflightPromptAndRealCall(t *testing.T) {
	f := newFakeDeleteServer(t, true)
	writeTestConfig(t, f.srv.URL, "tok")
	c := newDeleteCmd()
	c.SetIn(strings.NewReader("y\n"))
	out, errOut, err := execCmd(t, c, "--database", "d", "--measurement", "cpu", "--where", "host = 'a'")
	if err != nil {
		t.Fatalf("err=%v stderr=%q", err, errOut)
	}
	if !strings.Contains(errOut, "Delete 7 rows from d.cpu by rewriting 2 file(s)? Server limits: confirmation threshold 10000 rows, max 1000000 rows per delete.") {
		t.Errorf("prompt = %q", errOut)
	}
	if !strings.Contains(out, "Deleted 7 rows from d.cpu (1 file(s) rewritten, 2 affected)") {
		t.Errorf("out = %q", out)
	}
	if len(f.requests) != 2 || f.requests[0]["dry_run"] != true || f.requests[1]["dry_run"] != false || f.requests[1]["confirm"] != true {
		t.Errorf("requests = %v", f.requests)
	}
	// Full-table wording.
	f.rows = 3
	beforeN := len(f.requests)
	c2 := newDeleteCmd()
	c2.SetIn(strings.NewReader("n\n"))
	_, errOut, err = execCmd(t, c2, "--database", "d", "--measurement", "cpu", "--where", "1=1")
	if err == nil || err.Error() != "aborted" || !strings.Contains(errOut, "Delete ALL 3 rows of d.cpu") {
		t.Errorf("err=%v stderr=%q", err, errOut)
	}
	// Answering N must leave exactly one request behind: the dry-run preflight.
	if len(f.requests) != beforeN+1 || f.requests[beforeN]["dry_run"] != true {
		t.Errorf("after N: requests=%v", f.requests[beforeN:])
	}
	if f.rows != 3 {
		t.Error("rows must be untouched after N")
	}
	// Non-TTY stdin is refused before any request.
	before := len(f.requests)
	devnull, _ := os.Open(os.DevNull)
	defer devnull.Close()
	c3 := newDeleteCmd()
	c3.SetIn(devnull)
	_, _, err = execCmd(t, c3, "--database", "d", "--measurement", "cpu", "--where", "host = 'a'")
	if err == nil || !strings.Contains(err.Error(), "stdin is not a terminal") || len(f.requests) != before {
		t.Errorf("err=%v requests=%d", err, len(f.requests)-before)
	}
}

func TestDelete_ZeroMatchDisambiguation(t *testing.T) {
	f := newFakeDeleteServer(t, true)
	writeTestConfig(t, f.srv.URL, "tok")
	_, _, err := execCmd(t, newDeleteCmd(), "--database", "d", "--measurement", "cpu", "--where", "nosuchcol = 1", "--dry-run")
	if err == nil || !strings.Contains(err.Error(), "does not evaluate against d.cpu") || !strings.Contains(err.Error(), "nosuchcol") {
		t.Errorf("err = %v", err)
	}
	out, _, err := execCmd(t, newDeleteCmd(), "--database", "d", "--measurement", "cpu", "--where", "host = 'nomatch'", "--yes")
	if err != nil || !strings.Contains(out, "Deleted 0 rows") {
		t.Errorf("err=%v out=%q", err, out)
	}
	c := newDeleteCmd()
	c.SetIn(strings.NewReader("y\n"))
	out, _, err = execCmd(t, c, "--database", "d", "--measurement", "cpu", "--where", "host = 'nomatch'")
	if err != nil || !strings.Contains(out, "No rows in d.cpu match the predicate") {
		t.Errorf("err=%v out=%q", err, out)
	}
}

func TestDelete_PartialAndDisabled(t *testing.T) {
	f := newFakeDeleteServer(t, true)
	writeTestConfig(t, f.srv.URL, "tok")
	_, errOut, err := execCmd(t, newDeleteCmd(), "--database", "d", "--measurement", "cpu", "--where", "partial", "--yes")
	if err == nil || !strings.Contains(err.Error(), "4 rows deleted in 1 file(s), 1 file(s) failed") {
		t.Errorf("err = %v", err)
	}
	if !strings.Contains(errOut, "failed files: b.parquet") {
		t.Errorf("stderr = %q", errOut)
	}
	f2 := newFakeDeleteServer(t, false)
	writeTestConfig(t, f2.srv.URL, "tok")
	_, _, err = execCmd(t, newDeleteCmd(), "--database", "d", "--measurement", "cpu", "--where", "1=1", "--dry-run")
	if err == nil || !strings.Contains(err.Error(), "delete.enabled=true") {
		t.Errorf("err = %v", err)
	}
	if def := newDeleteCmd().Flags().Lookup("timeout").DefValue; def != "30m0s" {
		t.Errorf("delete --timeout default = %s", def)
	}
}

// fakeBackupServer emulates the backup routes with a sticky status that
// is only published after `publishDelay` (models the goroutine race).
type fakeBackupServer struct {
	mu           sync.Mutex
	backups      map[string]string // id -> manifest JSON
	status       string            // raw status JSON
	publishDelay time.Duration
	counter      int32
	srv          *httptest.Server
}

func newFakeBackupServer(t *testing.T) *fakeBackupServer {
	t.Helper()
	f := &fakeBackupServer{backups: map[string]string{}, status: `{"status":"idle"}`, publishDelay: 300 * time.Millisecond}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok","time":"t","uptime":"1s"}`))
	})
	manifest := func(id string) string {
		return `{"version":"dev","backup_id":"` + id + `","created_at":"2026-09-07T20:00:00Z","backup_type":"full","databases":[{"name":"smoke","measurements":[{"name":"cpu","file_count":1,"size_bytes":1064}],"file_count":1,"size_bytes":1064}],"total_files":1,"total_size_bytes":1064,"has_metadata":true,"has_config":false}`
	}
	publish := func(op, id string) {
		time.Sleep(f.publishDelay)
		started := time.Now().UTC().Format(time.RFC3339Nano) // constant per op, like the server
		f.mu.Lock()
		f.status = `{"operation":"` + op + `","backup_id":"` + id + `","status":"running","total_files":1,"processed_files":0,"skipped_files":0,"total_bytes":1064,"processed_bytes":0,"started_at":"` + started + `"}`
		f.mu.Unlock()
		time.Sleep(f.publishDelay)
		f.mu.Lock()
		f.status = `{"operation":"` + op + `","backup_id":"` + id + `","status":"completed","total_files":1,"processed_files":1,"skipped_files":0,"total_bytes":1064,"processed_bytes":1064,"started_at":"` + started + `","completed_at":"` + time.Now().UTC().Format(time.RFC3339Nano) + `"}`
		if op == "backup" {
			f.backups[id] = manifest(id)
		}
		f.mu.Unlock()
	}
	mux.HandleFunc("/api/v1/backup/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(403)
			_, _ = w.Write([]byte(`{"success":false,"error":"Permission denied: admin required"}`))
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, "/api/v1/backup/")
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case rest == "" && r.Method == http.MethodPost:
			n := atomic.AddInt32(&f.counter, 1)
			id := "backup-20260907-20000" + itoa(int64(n)) + "-0000000" + itoa(int64(n))
			go publish("backup", id)
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"message":"Backup started","status":"running"}`))
		case rest == "" && r.Method == http.MethodGet:
			list := []string{}
			for id := range f.backups {
				list = append(list, `{"backup_id":"`+id+`","created_at":"2026-09-07T20:00:00Z","backup_type":"full","total_files":1,"total_size_bytes":1064,"database_count":1}`)
			}
			_, _ = w.Write([]byte(`{"backups":[` + strings.Join(list, ",") + `],"count":` + itoa(int64(len(list))) + `}`))
		case rest == "status":
			_, _ = w.Write([]byte(f.status))
		case rest == "restore" && r.Method == http.MethodPost:
			var req struct {
				BackupID string `json:"backup_id"`
				Confirm  bool   `json:"confirm"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if !req.Confirm {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"error":"Restore is a destructive operation. Set confirm: true to proceed."}`))
				return
			}
			go publish("restore", req.BackupID)
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"backup_id":"` + req.BackupID + `","message":"Restore started","status":"running","restart_required":true,"staged":true}`))
		case r.Method == http.MethodGet:
			m, ok := f.backups[rest]
			if !ok {
				w.WriteHeader(404)
				_, _ = w.Write([]byte(`{"error":"Backup not found"}`))
				return
			}
			_, _ = w.Write([]byte(m))
		case r.Method == http.MethodDelete:
			if _, ok := f.backups[rest]; !ok {
				w.WriteHeader(500)
				_, _ = w.Write([]byte(`{"error":"Failed to delete backup"}`))
				return
			}
			delete(f.backups, rest)
			_, _ = w.Write([]byte(`{"backup_id":"` + rest + `","message":"Backup deleted"}`))
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func TestBackup_CreateWaitsForPublishedIDAndCompletion(t *testing.T) {
	fastPolls(t)
	f := newFakeBackupServer(t)
	writeTestConfig(t, f.srv.URL, "tok")
	out, _, err := execCmd(t, newBackupCreateCmd())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Backup backup-20260907-200001-00000001 started") {
		t.Errorf("out = %q", out)
	}
	time.Sleep(f.publishDelay * 3)
	// A second create right after a completed one must report the NEW id,
	// not the sticky previous status.
	out, _, err = execCmd(t, newBackupCreateCmd(), "--wait", "--wait-timeout", "10s")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Backup backup-20260907-200002-00000002 completed: 1 files, 1.0 KiB") {
		t.Errorf("out = %q", out)
	}
	out, _, err = execCmd(t, newBackupListCmd())
	if err != nil || !strings.Contains(out, "backup-20260907-200002-00000002") || !strings.Contains(out, "backup-20260907-200001-00000001") {
		t.Errorf("err=%v out=%s", err, out)
	}
	out, _, err = execCmd(t, newBackupShowCmd(), "backup-20260907-200001-00000001")
	if err != nil || !strings.Contains(out, "metadata true, config false") || !strings.Contains(out, "│ smoke") {
		t.Errorf("err=%v out=%s", err, out)
	}
	out, _, err = execCmd(t, newBackupStatusCmd())
	if err != nil || !strings.Contains(out, "status:     completed") {
		t.Errorf("err=%v out=%s", err, out)
	}
}

func TestBackup_DeleteRestoreAndGuards(t *testing.T) {
	fastPolls(t)
	f := newFakeBackupServer(t)
	writeTestConfig(t, f.srv.URL, "tok")
	f.mu.Lock()
	f.backups["backup-20260907-200009-00000009"] = `{"version":"dev","backup_id":"backup-20260907-200009-00000009","created_at":"2026-09-07T20:00:00Z","backup_type":"full","databases":[{"name":"smoke","measurements":[],"file_count":1,"size_bytes":10}],"total_files":1,"total_size_bytes":10,"skipped_files":2,"has_metadata":true,"has_config":false}`
	f.mu.Unlock()
	const id = "backup-20260907-200009-00000009"

	_, _, err := execCmd(t, newBackupShowCmd(), "not-an-id")
	if err == nil || !strings.Contains(err.Error(), "invalid backup id") {
		t.Errorf("err = %v", err)
	}
	_, _, err = execCmd(t, newBackupDeleteCmd(), "backup-00000000-000000-00000000", "--yes")
	if err == nil || !strings.Contains(err.Error(), "Backup not found") {
		t.Errorf("missing must be caught before the prompt: %v", err)
	}
	_, _, err = execCmd(t, newBackupRestoreCmd(), id, "--with-config", "--yes")
	if err == nil || !strings.Contains(err.Error(), "does not contain the server config") {
		t.Errorf("err = %v", err)
	}
	_, _, err = execCmd(t, newBackupRestoreCmd(), id, "--data-only", "--with-config", "--yes")
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("err = %v", err)
	}
	f.mu.Lock()
	statusBefore := f.status
	f.mu.Unlock()
	c := newBackupRestoreCmd()
	c.SetIn(strings.NewReader("n\n"))
	_, errOut, err := execCmd(t, c, id)
	if err == nil || err.Error() != "aborted" {
		t.Errorf("err = %v", err)
	}
	time.Sleep(f.publishDelay * 3)
	f.mu.Lock()
	if f.status != statusBefore {
		t.Errorf("answering N must not start a restore; status changed to %s", f.status)
	}
	f.mu.Unlock()
	for _, want := range []string{"warning: backup " + id + " is incomplete (2 files were skipped", "over the live storage of database(s) smoke", "metadata store will be staged", "Stop writers and compaction first"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("prompt missing %q: %q", want, errOut)
		}
	}
	out, errOut, err := execCmd(t, newBackupRestoreCmd(), id, "--yes", "--wait", "--wait-timeout", "10s")
	if err != nil {
		t.Fatalf("err=%v stderr=%q", err, errOut)
	}
	if !strings.Contains(out, "Restore of "+id+" completed: 1 files") || !strings.Contains(errOut, "applied at the next server start") {
		t.Errorf("out=%q stderr=%q", out, errOut)
	}
	_, errOut, err = execCmd(t, newBackupRestoreCmd(), id, "--data-only", "--yes")
	if err != nil || strings.Contains(errOut, "applied at the next server start") {
		t.Errorf("data-only must not print the restart note: err=%v stderr=%q", err, errOut)
	}
	time.Sleep(f.publishDelay * 3)
	out, _, err = execCmd(t, newBackupDeleteCmd(), id, "--yes")
	if err != nil || !strings.Contains(out, "Deleted backup "+id) {
		t.Errorf("err=%v out=%q", err, out)
	}
	writeTestConfig(t, f.srv.URL, "reader")
	_, _, err = execCmd(t, newBackupListCmd())
	if err == nil || !strings.Contains(err.Error(), "admin required") {
		t.Errorf("err = %v", err)
	}
}

// A failed backup must exit non-zero in JSON mode as well as table mode.
func TestBackup_CreateWaitJSONFailedExitsNonZero(t *testing.T) {
	fastPolls(t)
	mux := http.NewServeMux()
	var posted int32
	mux.HandleFunc("/api/v1/backup/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/v1/backup/")
		switch {
		case rest == "" && r.Method == http.MethodPost:
			atomic.StoreInt32(&posted, 1)
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"message":"Backup started","status":"running"}`))
		case rest == "status":
			if atomic.LoadInt32(&posted) == 0 {
				_, _ = w.Write([]byte(`{"status":"idle"}`))
				return
			}
			_, _ = w.Write([]byte(`{"operation":"backup","backup_id":"backup-20260907-200001-00000001","status":"failed","total_files":1,"processed_files":0,"skipped_files":0,"total_bytes":0,"processed_bytes":0,"started_at":"` + time.Now().UTC().Format(time.RFC3339Nano) + `","error":"disk full"}`))
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	writeTestConfig(t, srv.URL, "tok")
	out, _, err := execCmd(t, newBackupCreateCmd(), "--wait", "--wait-timeout", "10s", "-o", "json")
	if err == nil || !strings.Contains(err.Error(), "failed: disk full") {
		t.Errorf("err = %v", err)
	}
	if !strings.Contains(out, `"status": "failed"`) {
		t.Errorf("json must still be printed: %q", out)
	}
}

func TestBackup_DisabledServer(t *testing.T) {
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
	_, _, err := execCmd(t, newBackupListCmd())
	if err == nil || !strings.Contains(err.Error(), "backups are disabled on this server (backup.enabled=false)") {
		t.Errorf("err = %v", err)
	}
}
