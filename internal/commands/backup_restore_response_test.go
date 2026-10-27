package commands

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/basekick-labs/arcli/internal/client"
)

// The response is independent of the requested flags: only Arc knows whether
// this restore stages anything, including on older servers.
func TestBackup_RestoreRestartAdviceFollowsResponse(t *testing.T) {
	const id = "backup-20260907-200009-00000009"
	for _, tc := range []struct {
		name, response string
		flags          []string
		wantRestart    bool
	}{
		{"metadata requested but no restart reported", `{}`, []string{"--with-metadata"}, false},
		{"config requested but restart false", `{"restart_required":false,"staged":false}`, []string{"--with-config"}, false},
		{"default restart reported", `{"restart_required":true}`, nil, true},
		{"staged without restart field", `{"staged":true}`, nil, true},
		{"data only follows response", `{"restart_required":true}`, []string{"--data-only"}, true},
	} {
		for _, mode := range []string{"background", "wait", "json", "unpublished", "failed"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				fastPolls(t)
				var posted atomic.Bool
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/api/v1/backup/" + id:
						_, _ = w.Write([]byte(`{"backup_id":"` + id + `","has_metadata":true,"has_config":true}`))
					case "/api/v1/backup/status":
						if !posted.Load() || mode == "unpublished" {
							_, _ = w.Write([]byte(`{"status":"idle"}`))
							return
						}
						status := "completed"
						if mode == "failed" {
							status = "failed"
						}
						_, _ = w.Write([]byte(`{"operation":"restore","backup_id":"` + id + `","status":"` + status + `","started_at":"2026-10-26T00:00:00Z","error":"fixture failure"}`))
					case "/api/v1/backup/restore":
						posted.Store(true)
						w.WriteHeader(http.StatusAccepted)
						_, _ = w.Write([]byte(tc.response))
					default:
						http.NotFound(w, r)
					}
				}))
				defer srv.Close()
				writeTestConfig(t, srv.URL, "tok")
				args := append([]string{id, "--yes"}, tc.flags...)
				if mode == "wait" || mode == "failed" {
					args = append(args, "--wait")
				}
				if mode == "json" {
					args = append(args, "--output=json")
				}
				stdout, stderr, err := execCmd(t, newBackupRestoreCmd(), args...)
				if (err != nil) != (mode == "failed") {
					t.Fatalf("unexpected result: %v, stderr %q", err, stderr)
				}
				if got := strings.Contains(stderr, "Restart the server"); got != tc.wantRestart {
					t.Errorf("restart advice = %v, want %v: %q", got, tc.wantRestart, stderr)
				}
				if strings.Contains(stderr, "data files are already overwritten") {
					t.Errorf("acceptance does not establish data completion: %q", stderr)
				}
				if mode == "json" && !json.Valid([]byte(stdout)) {
					t.Errorf("invalid JSON output: %q", stdout)
				}
			})
		}
	}
}

func TestBackup_RestorePreservesServerRejection(t *testing.T) {
	const id = "backup-20260907-200009-00000009"
	const diagnostic = "restore_metadata is not available on a cluster node; restore data only (restore_metadata: false)"
	var posts, polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/backup/" + id:
			_, _ = w.Write([]byte(`{"backup_id":"` + id + `","has_metadata":true}`))
		case "/api/v1/backup/status":
			polls.Add(1)
			_, _ = w.Write([]byte(`{"status":"idle"}`))
		case "/api/v1/backup/restore":
			posts.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": diagnostic})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	writeTestConfig(t, srv.URL, "tok")
	stdout, stderr, err := execCmd(t, newBackupRestoreCmd(), id, "--with-metadata", "--yes", "--wait")
	var httpErr *client.HTTPError
	if !errors.As(err, &httpErr) || !strings.Contains(err.Error(), diagnostic) {
		t.Fatalf("server diagnostic lost: %v", err)
	}
	if posts.Load() != 1 || polls.Load() != 1 {
		t.Errorf("rejection retried or polled: posts=%d polls=%d", posts.Load(), polls.Load())
	}
	if stdout != "" || strings.Contains(stderr, "Restart the server") {
		t.Errorf("rejected operation reported success: stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestBackup_RestoreWaitReportsCompactionPauseTransitions(t *testing.T) {
	fastPolls(t)
	const id = "backup-20260907-200009-00000009"
	var posted atomic.Bool
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/backup/" + id:
			_, _ = w.Write([]byte(`{"backup_id":"` + id + `","has_metadata":false,"has_config":false}`))
		case "/api/v1/backup/status":
			if !posted.Load() {
				_, _ = w.Write([]byte(`{"status":"idle"}`))
				return
			}
			state := polls.Add(1)
			status, pause := "running", "waiting"
			if state == 2 {
				pause = "paused"
			} else if state >= 3 {
				status, pause = "completed", "released"
			}
			_, _ = w.Write([]byte(`{"operation":"restore","backup_id":"` + id + `","status":"` + status + `","compaction_pause":"` + pause + `","started_at":"2026-10-26T00:00:00Z"}`))
		case "/api/v1/backup/restore":
			posted.Store(true)
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"status":"accepted"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	writeTestConfig(t, srv.URL, "tok")

	_, stderr, err := execCmd(t, newBackupRestoreCmd(), id, "--yes", "--wait")
	if err != nil {
		t.Fatalf("restore failed: %v; stderr=%q", err, stderr)
	}
	for _, want := range []string{"compaction pause: waiting", "compaction pause: paused", "compaction pause: released"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("missing %q in stderr:\n%s", want, stderr)
		}
	}
}

func TestBackup_RestoreFlagBoundaries(t *testing.T) {
	const id = "backup-20260907-200009-00000009"
	for _, tc := range []struct {
		name        string
		flags       []string
		hasMetadata bool
		wantError   string
	}{
		{"no metadata default", nil, false, ""},
		{"false flags leave default", []string{"--data-only=false", "--with-metadata=false"}, true, ""},
		{"metadata missing", []string{"--with-metadata"}, false, "does not contain the metadata store"},
		{"metadata conflict", []string{"--data-only", "--with-metadata"}, true, "mutually exclusive"},
		{"config conflict", []string{"--data-only", "--with-config"}, true, "mutually exclusive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRestoreDefaultsServer(t, false, tc.hasMetadata)
			args := append([]string{id, "--yes"}, tc.flags...)
			_, _, err := execCmd(t, newBackupRestoreCmd(), args...)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("err = %v; want %q", err, tc.wantError)
				}
				if f.lastRestoreRequest() != nil {
					t.Fatal("invalid flags or missing metadata must not mutate")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			req := f.lastRestoreRequest()
			if req == nil {
				t.Fatal("restore request missing")
			}
			if _, present := req["restore_metadata"]; present {
				t.Errorf("default must omit restore_metadata: %s", req["restore_metadata"])
			}
		})
	}
}
