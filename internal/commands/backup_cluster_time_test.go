package commands

import (
	"encoding/csv"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBackupClusterCSVTimeOutput(t *testing.T) {
	const response = `{"backups":[{"backup_id":"backup-20260917-000000-aaaaaaaa","created_at":"2026-09-17T03:00:00.123456789+03:00","backup_type":"full","skipped_files":3,"skipped_reconciled":2,"unregistered_skipped":4,"manifest_only_files":5,"left_manifest_during_run":6,"cluster_manifest_checked":true}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(response)) }))
	defer srv.Close()
	writeTestConfig(t, srv.URL, "tok")
	for _, tc := range []struct{ mode, want string }{
		{"rfc3339", "2026-09-17T00:00:00Z"},
		{"rfc3339nano", "2026-09-17T00:00:00.123456789Z"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			out, _, err := execCmd(t, newBackupListCmd(), "-o", "csv", "--time-output", tc.mode)
			if err != nil {
				t.Fatal(err)
			}
			rows, err := csv.NewReader(strings.NewReader(out)).ReadAll()
			if err != nil || len(rows) != 2 || len(rows[0]) != 13 || len(rows[1]) != 13 {
				t.Fatalf("invalid CSV: %v %s", err, out)
			}
			if rows[1][1] != tc.want {
				t.Fatalf("timestamp %q, want %q", rows[1][1], tc.want)
			}
			for i, want := range []string{"4", "5", "6", "2"} {
				if rows[1][9+i] != want {
					t.Errorf("cluster field %s=%s, want %s", rows[0][9+i], rows[1][9+i], want)
				}
			}
		})
	}
}
