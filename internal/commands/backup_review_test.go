package commands

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestBackupReviewRequestedModeMustBeConfirmed(t *testing.T) {
	const id = "backup-20260917-000000-aaaaaaaa"
	for _, tc := range []struct {
		name, accepted, final string
		fail                  bool
	}{
		{"old server", "", "", true}, {"wrong accepted", "merge", "merge", true},
		{"missing final", "replace", "", true}, {"wrong final", "replace", "merge", true},
		{"confirmed", "replace", "replace", false},
	} {
		for _, format := range []string{"table", "json"} {
			t.Run(tc.name+"/"+format, func(t *testing.T) {
				fastPolls(t)
				var posted atomic.Bool
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/api/v1/backup/" + id:
						_, _ = w.Write([]byte(`{"backup_id":"` + id + `","skipped_files":3,"skipped_reconciled":3}`))
					case "/api/v1/backup/status":
						if !posted.Load() {
							_, _ = w.Write([]byte(`{"status":"idle"}`))
							return
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"operation": "restore", "backup_id": id, "status": "completed", "started_at": "2026-11-11T00:00:00Z", "mode": tc.final, "backup_skipped_files": 3})
					case "/api/v1/backup/restore":
						posted.Store(true)
						w.WriteHeader(http.StatusAccepted)
						_ = json.NewEncoder(w).Encode(map[string]string{"mode": tc.accepted})
					default:
						http.NotFound(w, r)
					}
				}))
				defer srv.Close()
				writeTestConfig(t, srv.URL, "tok")
				out, stderr, err := execCmd(t, newBackupRestoreCmd(), id, "--mode", "replace", "--yes", "--wait", "-o", format)
				if (err != nil) != tc.fail {
					t.Fatalf("out=%q stderr=%q err=%v", out, stderr, err)
				}
				if tc.fail && (!strings.Contains(stderr, "warning:") || !strings.Contains(err.Error(), "not confirmed")) {
					t.Fatalf("missing warning: %q %v", stderr, err)
				}
				if format == "json" && !json.Valid([]byte(out)) {
					t.Errorf("invalid JSON: %q", out)
				}
				if !tc.fail && format == "table" && (strings.Count(out, "mode:") != 1 || strings.Contains(out, "backup had:")) {
					t.Errorf("duplicate mode or reconciled gap: %q", out)
				}
			})
		}
	}
}

func TestBackupReviewReplaceRejectsKnownGaps(t *testing.T) {
	const id = "backup-20260917-000000-aaaaaaaa"
	for _, field := range []string{"skipped_files", "unaddressable_files", "manifest_only_files"} {
		t.Run(field, func(t *testing.T) {
			writes := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					writes++
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"backup_id": id, field: 1})
			}))
			defer srv.Close()
			writeTestConfig(t, srv.URL, "tok")
			_, stderr, err := execCmd(t, newBackupRestoreCmd(), id, "--mode", "replace", "--yes")
			if err == nil || !strings.Contains(err.Error(), "complete backup") || writes != 0 || strings.Contains(stderr, "[y/N]") {
				t.Fatalf("%v writes=%d stderr=%q", err, writes, stderr)
			}
		})
	}
}

func TestBackupReviewCreateShowsManifestOnlyGap(t *testing.T) {
	const id = "backup-20260917-000000-aaaaaaaa"
	fastPolls(t)
	var posted atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posted.Store(true)
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		if !posted.Load() {
			_, _ = w.Write([]byte(`{"status":"idle"}`))
			return
		}
		_, _ = w.Write([]byte(`{"operation":"backup","backup_id":"` + id + `","status":"completed","started_at":"2026-11-11T00:00:00Z","manifest_only_files":2,"manifest_only_sample":["metrics/cpu/gap.parquet"]}`))
	}))
	defer srv.Close()
	writeTestConfig(t, srv.URL, "tok")
	out, _, err := execCmd(t, newBackupCreateCmd(), "--wait")
	if err != nil || !strings.Contains(out, "2 files were present only in the cluster manifest") || !strings.Contains(out, "the backup is incomplete") || !strings.Contains(out, "metrics/cpu/gap.parquet") {
		t.Fatalf("%q %v", out, err)
	}
}

func TestBackupReviewStatusReconcilesWithoutChangingRawJSON(t *testing.T) {
	const id = "backup-20260917-000000-aaaaaaaa"
	const raw = `{"operation":"restore","backup_id":"` + id + `","status":"completed","backup_skipped_files":3,"backup_manifest_only_files":1,"future_field":"preserved"}`
	for _, lookupFails := range []bool{false, true} {
		for _, format := range []string{"table", "json"} {
			t.Run(format+"/lookup-fails="+strconv.FormatBool(lookupFails), func(t *testing.T) {
				var lookups atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/api/v1/backup/status":
						_, _ = w.Write([]byte(raw))
					case "/api/v1/backup/" + id:
						lookups.Add(1)
						if lookupFails {
							w.WriteHeader(http.StatusNotFound)
							_, _ = w.Write([]byte(`{"error":"Backup not found"}`))
							return
						}
						_, _ = w.Write([]byte(`{"backup_id":"` + id + `","skipped_files":3,"skipped_reconciled":3}`))
					default:
						t.Errorf("unexpected request %s", r.URL.Path)
					}
				}))
				defer srv.Close()
				writeTestConfig(t, srv.URL, "tok")
				out, _, err := execCmd(t, newBackupStatusCmd(), "-o", format)
				if err != nil {
					t.Fatal(err)
				}
				if format == "json" {
					var got map[string]any
					if err := json.Unmarshal([]byte(out), &got); err != nil {
						t.Fatal(err)
					}
					if got["backup_skipped_files"] != float64(3) || got["future_field"] != "preserved" || lookups.Load() != 0 {
						t.Fatalf("raw JSON changed or enriched: %q lookups=%d", out, lookups.Load())
					}
					return
				}
				if lookups.Load() != 1 || strings.Contains(out, "backup had: 3 skipped") || !strings.Contains(out, "backup had: 1 manifest-only") {
					t.Fatalf("incorrect gaps: %q lookups=%d", out, lookups.Load())
				}
				if strings.Contains(out, "before reconciliation") != lookupFails {
					t.Errorf("incorrect fallback: %q", out)
				}
			})
		}
	}
}
