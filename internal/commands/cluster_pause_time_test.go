package commands

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClusterPauseExpiryTimeOutput(t *testing.T) {
	const response = `{"enabled":true,"nodes":[],"compaction_pause":{"active":true,"expires_at":"2026-10-27T15:06:00.123456789+03:00"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(response)) }))
	defer srv.Close()
	writeTestConfig(t, srv.URL, "tok")
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"default", nil, "lease expires 2026-10-27T12:06:00Z"},
		{"nano", []string{"--time-output", "rfc3339nano"}, "lease expires 2026-10-27T12:06:00.123456789Z"},
		{"json", []string{"-o", "json"}, "2026-10-27T15:06:00.123456789+03:00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, _, err := execCmd(t, newClusterStatusCmd(), tc.args...)
			if err != nil || !strings.Contains(out, tc.want) {
				t.Fatalf("err=%v output=%s, want %s", err, out, tc.want)
			}
		})
	}
}
