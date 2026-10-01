package commands

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// Fixtures in the shapes Arc 26.09.3 (arc#977) produces: a backup with 21
// data files of which 3 were skipped (1 for an overlong key), 1 metadata file
// skipped, 2 unaddressable files; its completed backup status; and a failed
// restore of it. This file uses only symbols that exist before the arcli
// change, so it compiles against the old renderings and fails there.
const (
	incompleteID   = "backup-20260917-000000-aaaaaaaa"
	metadataOnlyID = "backup-20260915-000000-cccccccc"
)

func newIncompleteBackupServer(t *testing.T) *httptest.Server {
	t.Helper()
	long := "mydb/cpu/2026/09/17/00/" + strings.Repeat("x", 960) + ".parquet"
	var restored atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok","time":"t","uptime":"1s"}`))
	})
	mux.HandleFunc("/api/v1/backup/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/v1/backup/")
		switch {
		case rest == "" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"backups":[{"backup_id":"` + incompleteID + `","created_at":"2026-09-17T00:00:00Z","backup_type":"full","total_files":21,"total_size_bytes":2086,"database_count":1,"skipped_files":3,"skipped_metadata_files":1,"unaddressable_files":2},{"backup_id":"backup-20260916-000000-bbbbbbbb","created_at":"2026-09-16T00:00:00Z","backup_type":"full","total_files":20,"total_size_bytes":2000,"database_count":1},{"backup_id":"` + metadataOnlyID + `","created_at":"2026-09-15T00:00:00Z","backup_type":"full","total_files":20,"total_size_bytes":2000,"database_count":1,"skipped_metadata_files":1}],"count":3}`))
		case rest == "status":
			if restored.Load() {
				_, _ = w.Write([]byte(`{"operation":"restore","backup_id":"` + incompleteID + `","status":"failed","total_files":17,"processed_files":16,"skipped_files":1,"unaddressable_files":1,"unaddressable_sample":["` + incompleteID + `/data/mydb/cpu/2026/09/17/00/.hidden.parquet"],"skipped_sample":["` + incompleteID + `/data/mydb/cpu/2026/09/17/00/c.parquet"],"missing_files":1,"backup_skipped_files":3,"backup_unaddressable_files":2,"iceberg_warehouse_files_skipped":4,"total_bytes":2086,"processed_bytes":1600,"started_at":"2026-09-17T01:00:00Z","completed_at":"2026-09-17T01:00:02Z","error":"restore incomplete: 1 objects could not be read from backup storage (skipped_sample); 1 data files are in backup storage under names no listing returns (unaddressable_sample; rename them and re-run); 1 data files the backup inventoried are absent from backup storage (missing_files); the files that could be restored are in place"}`))
				return
			}
			_, _ = w.Write([]byte(`{"operation":"backup","backup_id":"` + incompleteID + `","status":"completed","total_files":23,"processed_files":19,"skipped_files":4,"unaddressable_files":2,"skipped_sample":["mydb/cpu/2026/09/17/00/a.parquet","mydb/cpu/2026/09/17/00/b.parquet","` + long + `","mydb/cpu/metadata/00003-5f2c.metadata.json"],"total_bytes":2086,"processed_bytes":1790,"started_at":"2026-09-17T00:00:00Z","completed_at":"2026-09-17T00:00:03Z"}`))
		case rest == "restore" && r.Method == http.MethodPost:
			restored.Store(true)
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"backup_id":"` + incompleteID + `","message":"Restore started","status":"running"}`))
		case rest == metadataOnlyID && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"version":"26.09.3","backup_id":"` + metadataOnlyID + `","created_at":"2026-09-15T00:00:00Z","backup_type":"full","databases":[{"name":"mydb","measurements":[{"name":"cpu","file_count":20,"size_bytes":2000}],"file_count":20,"size_bytes":2000}],"total_files":20,"total_size_bytes":2000,"skipped_metadata_files":1,"skipped_sample":["mydb/cpu/metadata/00004-aa11.metadata.json"],"skipped_overlong_keys":1,"has_metadata":true,"has_config":false}`))
		case rest == incompleteID && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"version":"26.09.3","backup_id":"` + incompleteID + `","created_at":"2026-09-17T00:00:00Z","backup_type":"full","databases":[{"name":"mydb","measurements":[{"name":"cpu","file_count":21,"size_bytes":2086}],"file_count":21,"size_bytes":2086}],"total_files":21,"total_size_bytes":2086,"skipped_files":3,"skipped_metadata_files":1,"skipped_sample":["mydb/cpu/2026/09/17/00/a.parquet","mydb/cpu/2026/09/17/00/b.parquet","` + long + `","mydb/cpu/metadata/00003-5f2c.metadata.json"],"skipped_overlong_keys":1,"unaddressable_files":2,"unaddressable_sample":["mydb/cpu/2026/09/17/00/.hidden.parquet","mydb/cpu/2026/09/17/00/bad key.parquet"],"iceberg_warehouse":{"path":"/srv/wh","file_count":4,"size_bytes":100,"skipped_files":1},"has_metadata":true,"has_config":false}`))
		default:
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":"Backup not found"}`))
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestBackup_IncompleteIsVisible(t *testing.T) {
	fastPolls(t)
	srv := newIncompleteBackupServer(t)
	writeTestConfig(t, srv.URL, "tok")

	// list: the table names the three populations apart and never sums them;
	// the complete entry reads "-".
	out, _, err := execCmd(t, newBackupListCmd())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"INCOMPLETE", "3 skipped, 1 metadata, 2 unaddressable", "│ -"} {
		if !strings.Contains(out, want) {
			t.Errorf("list table lacks %q:\n%s", want, out)
		}
	}
	out, _, err = execCmd(t, newBackupListCmd(), "-o", "csv")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 4 || lines[0] != "backup_id,created_at,backup_type,database_count,total_files,total_size_bytes,skipped_files,skipped_metadata_files,unaddressable_files" {
		t.Errorf("csv header = %q (%d lines)", lines[0], len(lines))
	}
	if len(lines) == 4 && (!strings.HasSuffix(lines[1], ",21,2086,3,1,2") || !strings.HasSuffix(lines[2], ",20,2000,0,0,0") || !strings.HasSuffix(lines[3], ",20,2000,0,1,0")) {
		t.Errorf("csv rows = %q", lines[1:])
	}
	out, _, err = execCmd(t, newBackupListCmd(), "-o", "csv", "--no-header")
	if err != nil || strings.Contains(out, "backup_id,") || !strings.Contains(out, ",3,1,2") {
		t.Errorf("csv --no-header: err=%v out=%q", err, out)
	}
	// json is the server's body, untouched.
	out, _, err = execCmd(t, newBackupListCmd(), "-o", "json")
	if err != nil || !strings.Contains(out, `"skipped_metadata_files": 1`) || strings.Contains(out, "INCOMPLETE") || strings.Contains(out, "2 unaddressable") {
		t.Errorf("json passthrough: err=%v out=%s", err, out)
	}
	// A backup whose only gap is one metadata file, skipped for an overlong
	// key: singular wording, and the overlong clause survives without any
	// data-file skip to hang on.
	out, _, err = execCmd(t, newBackupListCmd())
	if err != nil || !strings.Contains(out, "│ 1 metadata") {
		t.Errorf("metadata-only list cell: err=%v out=%s", err, out)
	}
	out, _, err = execCmd(t, newBackupShowCmd(), metadataOnlyID)
	if err != nil || !strings.Contains(out, "INCOMPLETE:  1 metadata file was skipped while backing up (1 for a key too long to store)\n  skipped:       mydb/cpu/metadata/00004-aa11.metadata.json\n") {
		t.Errorf("metadata-only show: err=%v out=%s", err, out)
	}

	// show: the counts in words, each in its own domain, then the names.
	out, _, err = execCmd(t, newBackupShowCmd(), incompleteID)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"INCOMPLETE:  3 of 21 files and 1 metadata file were skipped while backing up (1 for a key too long to store); 2 files could not be listed (unaddressable); 1 Iceberg warehouse file was skipped\n",
		"  skipped:       mydb/cpu/2026/09/17/00/a.parquet\n",
		"  skipped:       mydb/cpu/metadata/00003-5f2c.metadata.json\n",
		"  unaddressable: mydb/cpu/2026/09/17/00/bad key.parquet\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("show lacks %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "  skipped:") != 4 || strings.Count(out, "  unaddressable:") != 2 {
		t.Errorf("show sample line counts wrong:\n%s", out)
	}

	// status of the completed backup: the names, and the unaddressable
	// count whose names only the manifest has.
	out, _, err = execCmd(t, newBackupStatusCmd())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"files:      19 / 23 (4 skipped)\n",
		"  skipped:       mydb/cpu/2026/09/17/00/b.parquet\n",
		"  unaddressable: 2 files (names: arcli backup show " + incompleteID + ")\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("backup status lacks %q:\n%s", want, out)
		}
	}

	// restore: the pre-flight warning covers every gap the manifest records.
	out, stderr, err := execCmd(t, newBackupRestoreCmd(), incompleteID, "--yes")
	if err != nil {
		t.Fatalf("restore: %v (stderr %s)", err, stderr)
	}
	if !strings.Contains(stderr, "warning: backup "+incompleteID+" is incomplete (3 files and 1 metadata file were skipped when it was taken, 1 of them for a key too long to store; 2 files could not be listed; 1 Iceberg warehouse file was skipped)\n") {
		t.Errorf("restore warning missing or wrong:\n%s", stderr)
	}
	if !strings.Contains(out, "Restore of "+incompleteID+" started") {
		t.Errorf("restore out = %q", out)
	}

	// status of the failed restore: the server's error already carries the
	// counts; arcli adds the names, what the backup lacked, and the
	// warehouse files left out.
	out, _, err = execCmd(t, newBackupStatusCmd())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"status:     failed\n",
		"error:      restore incomplete: 1 objects could not be read",
		"backup had: 3 skipped, 2 unaddressable when it was taken\n",
		"iceberg:    4 warehouse files not restored (this node has no outside-root warehouse)\n",
		"  skipped:       " + incompleteID + "/data/mydb/cpu/2026/09/17/00/c.parquet\n",
		"  unaddressable: " + incompleteID + "/data/mydb/cpu/2026/09/17/00/.hidden.parquet\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("restore status lacks %q:\n%s", want, out)
		}
	}

	// The metadata-only backup's pre-flight warning: singular wording, and
	// the overlong clause survives without a data-file skip to hang on. (Runs
	// last: the fake server's status is the failed restore from here on, so
	// this restore is "accepted but not yet published", which is fine.)
	_, stderr0, err := execCmd(t, newBackupRestoreCmd(), metadataOnlyID, "--yes")
	if err != nil || !strings.Contains(stderr0, "warning: backup "+metadataOnlyID+" is incomplete (1 metadata file was skipped when it was taken, 1 of them for a key too long to store)\n") {
		t.Errorf("metadata-only restore warning: err=%v stderr=%s", err, stderr0)
	}
}

// While a backup runs, or after it failed, no manifest exists, so the hint for
// unaddressable names must point at the server log, not at "backup show".
func TestBackup_StatusFailedBackupNamesAreInTheLog(t *testing.T) {
	fastPolls(t)
	for _, status := range []string{"failed", "running"} {
		mux := http.NewServeMux()
		mux.HandleFunc("/api/v1/backup/status", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"operation":"backup","backup_id":"` + incompleteID + `","status":"` + status + `","total_files":24,"processed_files":19,"skipped_files":5,"unaddressable_files":2,"skipped_sample":["smoke/m1/a.parquet"],"total_bytes":2086,"processed_bytes":1600,"started_at":"2026-09-17T00:00:00Z","error":"backup failed: 5 of 24 files skipped (>10%): 5 could not be read at copy time (check source storage)"}`))
		})
		srv := httptest.NewServer(mux)
		writeTestConfig(t, srv.URL, "tok")
		out, _, err := execCmd(t, newBackupStatusCmd())
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "  skipped:       smoke/m1/a.parquet\n  unaddressable: 2 files (names in the server log)\n") || strings.Contains(out, "backup show") {
			t.Errorf("%s backup status: %s", status, out)
		}
	}
}
