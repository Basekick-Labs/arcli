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
	// accepted/final "" is a server before Arc 27.01.1: its 202 and status
	// carry no mode field at all, and it always merges.
	for _, tc := range []struct {
		name, requested, accepted, final string
		fail                             bool
	}{
		{"old server replace", "replace", "", "", true}, {"wrong accepted", "replace", "merge", "merge", true},
		{"missing final", "replace", "replace", "", true}, {"wrong final", "replace", "replace", "merge", true},
		{"confirmed", "replace", "replace", "replace", false},
		{"old server merge", "merge", "", "", false}, {"merge confirmed", "merge", "merge", "merge", false},
		{"merge answered with replace", "merge", "replace", "replace", true},
	} {
		for _, wait := range []bool{true, false} {
			if !wait && tc.accepted == tc.requested && tc.final != tc.accepted {
				continue // only --wait reads the final status
			}
			for _, format := range []string{"table", "json"} {
				t.Run(tc.name+"/wait="+strconv.FormatBool(wait)+"/"+format, func(t *testing.T) {
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
							st := map[string]any{"operation": "restore", "backup_id": id, "status": "completed", "started_at": "2026-11-11T00:00:00Z", "backup_skipped_files": 3}
							if tc.final != "" {
								st["mode"] = tc.final
							}
							_ = json.NewEncoder(w).Encode(st)
						case "/api/v1/backup/restore":
							posted.Store(true)
							w.WriteHeader(http.StatusAccepted)
							res := map[string]string{"backup_id": id, "message": "Restore started", "status": "running"}
							if tc.accepted != "" {
								res["mode"] = tc.accepted
							}
							_ = json.NewEncoder(w).Encode(res)
						default:
							http.NotFound(w, r)
						}
					}))
					defer srv.Close()
					writeTestConfig(t, srv.URL, "tok")
					args := []string{id, "--mode", tc.requested, "--yes", "-o", format}
					if wait {
						args = append(args, "--wait")
					}
					out, stderr, err := execCmd(t, newBackupRestoreCmd(), args...)
					if (err != nil) != tc.fail {
						t.Fatalf("out=%q stderr=%q err=%v", out, stderr, err)
					}
					if tc.fail && (!strings.Contains(stderr, "warning:") || !strings.Contains(err.Error(), "not confirmed")) {
						t.Fatalf("missing warning: %q %v", stderr, err)
					}
					if format == "json" && !json.Valid([]byte(out)) {
						t.Errorf("invalid JSON: %q", out)
					}
					if !tc.fail && format == "table" {
						wantMode := 0
						if tc.requested == "replace" {
							wantMode = 1
						}
						if strings.Count(out, "mode:") != wantMode || strings.Contains(out, "backup had:") {
							t.Errorf("want %d mode labels and no reconciled gap: %q", wantMode, out)
						}
					}
				})
			}
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
			if err == nil || !strings.Contains(err.Error(), "requires a complete backup, and backup "+id+" is incomplete (") || writes != 0 || strings.Contains(stderr, "[y/N]") {
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
