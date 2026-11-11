package commands

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/basekick-labs/arcli/internal/client"
)

func TestCompactionWaitTimeoutBoundsRequestsAndPolls(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		block, command         bool
		requestWait, totalWait time.Duration
		wantErr                string // exact message; "" = not checked
	}{
		// The published message, byte for byte (no ": context deadline exceeded").
		{"poll delay", false, false, 5 * time.Second, 50 * time.Millisecond, "cycle 1 still running after 50ms; it continues on the server"},
		{"blocked request", true, false, 5 * time.Second, 50 * time.Millisecond, "gave up after 50ms waiting for cycle 1 (no status received in time); it may still run on the server"},
		{"shorter request timeout", true, false, 50 * time.Millisecond, 5 * time.Second, ""},
		{"command JSON output", true, true, 5 * time.Second, 50 * time.Millisecond, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldInterval, oldMax := waitPollInterval, waitPollMax
			waitPollInterval, waitPollMax = 5*time.Second, 5*time.Second
			t.Cleanup(func() { waitPollInterval, waitPollMax = oldInterval, oldMax })
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/compaction/trigger" {
					_, _ = io.WriteString(w, `{"status":"running","tiers":["hourly"],"cycle_id":1}`)
					return
				}
				if tc.block {
					select {
					case <-r.Context().Done():
					case <-release:
					}
					return
				}
				_, _ = io.WriteString(w, `{"cycle_running":true,"current_cycle_id":1,"recent_jobs":[]}`)
			}))
			t.Cleanup(func() { close(release); srv.Close() })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cli, err := client.New(client.Config{Endpoint: srv.URL})
			if err != nil {
				t.Fatal(err)
			}
			if tc.command {
				writeTestConfig(t, srv.URL, "tok")
			}
			type result struct {
				out string
				err error
			}
			done := make(chan result, 1)
			go func() {
				if tc.command {
					cmd := newCompactionTriggerCmd()
					cmd.SetContext(ctx)
					out, _, err := execCmd(t, cmd, "--wait", "--wait-timeout", tc.totalWait.String(), "--timeout", tc.requestWait.String(), "-o", "json")
					done <- result{out, err}
					return
				}
				_, err := waitForCycle(ctx, cli, 1, tc.requestWait, tc.totalWait)
				done <- result{err: err}
			}()
			select {
			case got := <-done:
				if !errors.Is(got.err, context.DeadlineExceeded) {
					t.Errorf("error = %v; want deadline exceeded", got.err)
				}
				if tc.wantErr != "" && (got.err == nil || got.err.Error() != tc.wantErr) {
					t.Errorf("error = %v; want exactly %q", got.err, tc.wantErr)
				}
				if got.out != "" {
					t.Errorf("timed-out command printed a result: %q", got.out)
				}
			case <-time.After(2 * time.Second):
				cancel()
				<-done
				t.Fatal("the shorter timeout did not interrupt waiting within 2s")
			}
		})
	}
}
