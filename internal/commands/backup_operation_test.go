package commands

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basekick-labs/arcli/internal/client"
)

func TestBackupWaitTracksOperationIdentity(t *testing.T) {
	const id = "backup-20260907-200009-00000009"
	started := time.Date(2026, 9, 7, 20, 0, 0, 123456789, time.UTC)
	for _, operation := range []string{"backup", "restore"} {
		for _, change := range []string{"none", "start time", "operation", "backup id", "failed"} {
			t.Run(operation+"/"+change, func(t *testing.T) {
				initial := client.BackupProgress{Operation: operation, BackupID: id, StartedAt: started, Status: "running"}
				final := initial
				final.Status = "completed"
				// Different offsets can describe the same operation's start.
				final.StartedAt = started.In(time.FixedZone("offset", 3600))
				switch change {
				case "start time":
					// A repeated restore can reuse both the kind and backup id.
					final.StartedAt = started.Add(time.Nanosecond)
				case "operation":
					final.Operation = "restore"
					if operation == "restore" {
						final.Operation = "backup"
					}
				case "backup id":
					final.BackupID = "backup-20260907-200010-00000010"
				case "failed":
					final.Status = "failed"
					final.Error = "original operation failed"
				}
				var statusCalls atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					switch {
					case r.URL.Path == "/api/v1/backup/status":
						switch statusCalls.Add(1) {
						case 1:
							_, _ = w.Write([]byte(`{"status":"idle"}`))
						case 2:
							_ = json.NewEncoder(w).Encode(initial)
						default:
							_ = json.NewEncoder(w).Encode(final)
						}
					case r.Method == http.MethodPost:
						w.WriteHeader(http.StatusAccepted)
						_ = json.NewEncoder(w).Encode(map[string]string{"backup_id": id, "status": "running"})
					case r.URL.Path == "/api/v1/backup/"+id:
						_ = json.NewEncoder(w).Encode(client.BackupManifest{BackupID: id, Databases: []client.BackupDatabase{}})
					default:
						http.NotFound(w, r)
					}
				}))
				defer srv.Close()
				writeTestConfig(t, srv.URL, "tok")
				for _, format := range []string{"table", "json"} {
					t.Run(format, func(t *testing.T) {
						statusCalls.Store(0)
						cmd := newBackupCreateCmd()
						args := []string{"--wait", "--wait-timeout", "1s", "-o", format}
						if operation == "restore" {
							cmd = newBackupRestoreCmd()
							args = append(args, id, "--data-only", "--yes")
						}
						out, _, err := execCmd(t, cmd, args...)
						switch change {
						case "none":
							if err != nil || !strings.Contains(out, "completed") {
								t.Errorf("tracked operation: err=%v out=%q", err, out)
							}
						case "failed":
							if err == nil || !strings.Contains(err.Error(), "original operation failed") {
								t.Errorf("tracked failure: err=%v out=%q", err, out)
							}
						default:
							if err == nil || !strings.Contains(err.Error(), "different operation") {
								t.Errorf("replacement operation: err=%v out=%q", err, out)
							}
							if out != "" {
								t.Errorf("replacement operation's result was printed: %q", out)
							}
						}
					})
				}
			})
		}
	}
}
