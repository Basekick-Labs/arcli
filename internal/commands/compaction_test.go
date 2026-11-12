package commands

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/basekick-labs/arcli/internal/client"
)

// fakeCompactionServer emulates the compaction routes. Each trigger
// bumps the cycle id; cycle_running stays true for `busyPolls` stats
// calls after a trigger so --wait has something to wait for.
func fakeCompactionServer(t *testing.T, busyPolls int32) (*httptest.Server, *int32) {
	t.Helper()
	var cycle int64
	var triggers int32
	var remaining int32
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok","time":"t","uptime":"1s"}`))
	})
	mux.HandleFunc("/api/v1/compaction/status", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"manager":{"active_jobs":null,"total_completed":2,"total_failed":1},"schedulers":{"hourly":{"enabled":true,"running":true,"schedule":"5 * * * *","next_run":"2026-09-07T13:05:00-06:00"},"daily":{"enabled":false,"running":false,"schedule":"0 3 * * *","next_run":"0001-01-01T00:00:00Z"}}}`))
	})
	mux.HandleFunc("/api/v1/compaction/stats", func(w http.ResponseWriter, r *http.Request) {
		running := "false"
		cur := atomic.LoadInt64(&cycle)
		// last_cycle is the latest FINISHED cycle; its counts deliberately
		// differ from the lifetime totals (2 jobs, 1 failed, 1.5 MiB), so a
		// result line built from the totals cannot pass for the cycle's.
		last := cur
		if atomic.LoadInt32(&remaining) > 0 {
			atomic.AddInt32(&remaining, -1)
			running = "true"
			last = cur - 1
		}
		lastCycle := `{"cycle_id":` + itoa(last) + `,"status":"completed","discovered_batches":3,"started_batches":3,"succeeded_batches":3,"failed_batches":0,"interrupted_batches":0,"unstarted_batches":0,"discovery_errors":0}`
		_, _ = w.Write([]byte(`{"last_cycle":` + lastCycle + `,"total_jobs_completed":2,"total_jobs_failed":1,"total_files_compacted":30,"total_bytes_saved":1572864,"total_bytes_saved_mb":1.5,"total_manifests_recover":0,"cycle_running":` + running + `,"current_cycle_id":` + itoa(atomic.LoadInt64(&cycle)) + `,
		 "recent_jobs":[{"database":"m","measurement":"cpu","partition_path":"m/cpu/2026/09/07/10","tier":"hourly","files_compacted":12,"bytes_before":1048576,"bytes_after":524288,"success":true,"compression_ratio":0.5},{"database":"m","measurement":"mem","partition_path":"m/mem/2026/09/07/10","tier":"hourly","success":false,"error":"disk full"}],
		 "tiers":[{"tier":"hourly","enabled":true,"min_age_hours":1,"min_files":10,"total_compactions":2,"total_files_compacted":30,"total_bytes_saved":1572864},{"tier":"daily","enabled":false,"min_age_hours":24,"min_files":12}]}`))
	})
	mux.HandleFunc("/api/v1/compaction/candidates", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"count":2,"candidates":[{"database":"z","measurement":"a","partition_path":"z/a/p","file_count":11,"tier":"hourly"},{"database":"m","measurement":"cpu","partition_path":"m/cpu/p","file_count":15,"tier":"hourly"}]}`))
	})
	mux.HandleFunc("/api/v1/compaction/history", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"total_jobs":2,"recent_jobs":[{"database":"m","measurement":"cpu","partition_path":"p","tier":"hourly","files_compacted":12,"bytes_before":1048576,"bytes_after":524288,"success":true,"compression_ratio":0.5}]}`))
	})
	mux.HandleFunc("/api/v1/compaction/trigger", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(403)
			_, _ = w.Write([]byte(`{"success":false,"error":"Permission denied: admin required"}`))
			return
		}
		atomic.AddInt32(&triggers, 1)
		if strings.Contains(r.URL.RawQuery, "database=paused") {
			w.WriteHeader(409)
			_, _ = w.Write([]byte(`{"error":"compaction is paused cluster-wide","message":"a restore is running; retry when it ends","paused":true}`))
			return
		}
		if strings.Contains(r.URL.RawQuery, "database=busy") {
			w.WriteHeader(409)
			_, _ = w.Write([]byte(`{"error":"Compaction cycle already running","cycle_id":41,"is_running":true}`))
			return
		}
		next := atomic.AddInt64(&cycle, 1)
		atomic.StoreInt32(&remaining, busyPolls)
		tiers := `["hourly","daily"]`
		if strings.Contains(r.URL.RawQuery, "tier=hourly") {
			tiers = `["hourly"]`
		}
		_, _ = w.Write([]byte(`{"message":"Compaction triggered","status":"running","tiers":` + tiers + `,"cycle_id":` + itoa(next) + `}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &triggers
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestCompactionStatus_TableNormalisesToUTCAndZeroNextRun(t *testing.T) {
	srv, _ := fakeCompactionServer(t, 0)
	writeTestConfig(t, srv.URL, "tok")
	out, _, err := execCmd(t, newCompactionStatusCmd())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "jobs:        2 completed, 1 failed, active -") {
		t.Errorf("out = %s", out)
	}
	if !strings.Contains(out, "2026-09-07T19:05:00Z") {
		t.Errorf("next_run must be rendered in UTC: %s", out)
	}
	// daily has a zero next_run → "-"
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "daily") && !strings.Contains(line, "│ -") {
			t.Errorf("zero next_run must render as -: %s", line)
		}
	}
}

func TestCompactionStats_Table(t *testing.T) {
	srv, _ := fakeCompactionServer(t, 0)
	writeTestConfig(t, srv.URL, "tok")
	out, _, err := execCmd(t, newCompactionStatsCmd())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"bytes saved:     1.5 MiB", "cycle:           idle (current id 0)", "│ hourly │ true", "│ daily  │ false", "1.0 MiB", "512.0 KiB", "50.0%", "failed: disk full", "│ ok "} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestCompactionCandidates_FilterSortAndCSV(t *testing.T) {
	srv, _ := fakeCompactionServer(t, 0)
	writeTestConfig(t, srv.URL, "tok")
	out, _, err := execCmd(t, newCompactionCandidatesCmd(), "-o", "csv")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[1], "m,cpu,") || !strings.HasPrefix(lines[2], "z,a,") {
		t.Errorf("csv must be sorted by database: %q", out)
	}
	out, _, err = execCmd(t, newCompactionCandidatesCmd(), "--database", "z", "-o", "json")
	if err != nil || strings.Contains(out, `"m"`) || !strings.Contains(out, `"count": 1`) {
		t.Errorf("filtered json: err=%v\n%s", err, out)
	}
	out, _, err = execCmd(t, newCompactionCandidatesCmd(), "--database", "none")
	if err != nil || !strings.Contains(out, "(no compaction candidates)") {
		t.Errorf("err=%v out=%q", err, out)
	}
}

func TestCompactionHistory_LimitNoteAndValidation(t *testing.T) {
	srv, _ := fakeCompactionServer(t, 0)
	writeTestConfig(t, srv.URL, "tok")
	_, _, err := execCmd(t, newCompactionHistoryCmd(), "--limit", "0")
	if err == nil || !strings.Contains(err.Error(), "--limit must be >= 1") {
		t.Errorf("err = %v", err)
	}
	out, errOut, err := execCmd(t, newCompactionHistoryCmd(), "--limit", "50")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut, "at most the 10 most recent jobs") {
		t.Errorf("stderr = %q", errOut)
	}
	if !strings.Contains(out, "total: 2 successful jobs") || !strings.Contains(out, "50.0%") {
		t.Errorf("out = %s", out)
	}
}

func TestCompactionTrigger_ValidationWarningsAnd409(t *testing.T) {
	srv, triggers := fakeCompactionServer(t, 0)
	writeTestConfig(t, srv.URL, "tok")
	_, _, err := execCmd(t, newCompactionTriggerCmd(), "--tier", "weekly")
	if err == nil || !strings.Contains(err.Error(), `invalid --tier "weekly"`) {
		t.Errorf("err = %v", err)
	}
	_, _, err = execCmd(t, newCompactionTriggerCmd(), "--database", "1bad")
	if err == nil || !strings.Contains(err.Error(), "invalid database name") {
		t.Errorf("err = %v", err)
	}
	if *triggers != 0 {
		t.Fatal("validation failures must not reach the server")
	}
	out, errOut, err := execCmd(t, newCompactionTriggerCmd(), "--tier", "daily,hourly")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut, `warning: tier "daily" is disabled on the server`) {
		t.Errorf("stderr = %q", errOut)
	}
	if !strings.Contains(out, "Compaction cycle requested (expected cycle id 1)") {
		t.Errorf("out = %q", out)
	}
	_, _, err = execCmd(t, newCompactionTriggerCmd(), "--database", "busy")
	if err == nil || !strings.Contains(err.Error(), "already running (cycle 41)") {
		t.Errorf("err = %v", err)
	}
	_, _, err = execCmd(t, newCompactionTriggerCmd(), "--database", "paused")
	if err == nil || err.Error() != "compaction is paused cluster-wide: a restore is running; retry when it ends" {
		t.Errorf("paused response should be preserved: err = %v", err)
	}
	writeTestConfig(t, srv.URL, "reader")
	_, _, err = execCmd(t, newCompactionTriggerCmd())
	if err == nil || !strings.Contains(err.Error(), "admin required") {
		t.Errorf("err = %v", err)
	}
}

func fastPolls(t *testing.T) {
	t.Helper()
	oldI, oldM := waitPollInterval, waitPollMax
	waitPollInterval, waitPollMax = 5*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { waitPollInterval, waitPollMax = oldI, oldM })
}

func TestCompactionTrigger_WaitPollsUntilCycleDone(t *testing.T) {
	fastPolls(t)
	srv, _ := fakeCompactionServer(t, 2)
	writeTestConfig(t, srv.URL, "tok")
	out, _, err := execCmd(t, newCompactionTriggerCmd(), "--wait", "--wait-timeout", "30s")
	if err != nil {
		t.Fatal(err)
	}
	// The waited cycle's own batch counts, never the lifetime totals the
	// same response carries (arcli#96).
	if !strings.Contains(out, "Cycle 1 finished: 3 of 3 batches compacted, 0 failed\n") {
		t.Errorf("out = %q", out)
	}
	if strings.Contains(out, "jobs completed") || strings.Contains(out, "saved") {
		t.Errorf("lifetime totals printed as the cycle result: %q", out)
	}
}

// -o json with --wait must keep stdout as a single JSON document; the
// progress line moves to stderr.
func TestCompactionTrigger_WaitJSONKeepsStdoutClean(t *testing.T) {
	fastPolls(t)
	srv, _ := fakeCompactionServer(t, 1)
	writeTestConfig(t, srv.URL, "tok")
	out, errOut, err := execCmd(t, newCompactionTriggerCmd(), "--wait", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(strings.TrimSpace(out), "{") || !strings.Contains(out, `"current_cycle_id": 1`) {
		t.Errorf("stdout must be only the final stats JSON: %q", out)
	}
	if !strings.Contains(errOut, "Compaction cycle requested") {
		t.Errorf("progress line must go to stderr: %q", errOut)
	}
}

// When the server over-predicts the cycle id (or the trigger raced a
// scheduled cycle), --wait must not sleep to the deadline.
func TestCompactionTrigger_WaitGivesUpWhenServerIdleBelowExpected(t *testing.T) {
	fastPolls(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/compaction/trigger", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"message":"Compaction triggered","status":"running","tiers":["hourly"],"cycle_id":9}`))
	})
	mux.HandleFunc("/api/v1/compaction/stats", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"cycle_running":false,"current_cycle_id":8,"recent_jobs":[]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	writeTestConfig(t, srv.URL, "tok")
	start := time.Now()
	_, _, err := execCmd(t, newCompactionTriggerCmd(), "--wait", "--wait-timeout", "1h")
	if err == nil || !strings.Contains(err.Error(), "no cycle 9 was started (server is idle at cycle 8)") {
		t.Errorf("err = %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("gave up too slowly")
	}
}

func TestCompactionTrigger_UnconfiguredTierWarnsAndStatsFailureIsAdvisory(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/compaction/stats", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":"busy"}`))
	})
	mux.HandleFunc("/api/v1/compaction/trigger", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"message":"Compaction triggered","status":"running","tiers":["daily"],"cycle_id":1}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	writeTestConfig(t, srv.URL, "tok")
	_, errOut, err := execCmd(t, newCompactionTriggerCmd(), "--tier", "daily")
	if err != nil {
		t.Fatalf("a failing advisory stats call must not block the trigger: %v", err)
	}
	if !strings.Contains(errOut, "could not check tier state") {
		t.Errorf("stderr = %q", errOut)
	}

	// A server with only hourly configured: --tier daily must warn.
	mux3 := http.NewServeMux()
	mux3.HandleFunc("/api/v1/compaction/stats", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"cycle_running":false,"current_cycle_id":0,"recent_jobs":[],"tiers":[{"tier":"hourly","enabled":true}]}`))
	})
	mux3.HandleFunc("/api/v1/compaction/trigger", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"message":"Compaction triggered","status":"running","tiers":["daily"],"cycle_id":1}`))
	})
	srv3 := httptest.NewServer(mux3)
	defer srv3.Close()
	writeTestConfig(t, srv3.URL, "tok")
	_, errOut, err = execCmd(t, newCompactionTriggerCmd(), "--tier", "daily")
	if err != nil || !strings.Contains(errOut, `tier "daily" is not configured on the server`) {
		t.Errorf("err=%v stderr=%q", err, errOut)
	}
}

func TestCompaction_DisabledServer(t *testing.T) {
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
	for name, c := range map[string]func() *cobra.Command{
		"status": newCompactionStatusCmd, "stats": newCompactionStatsCmd, "candidates": newCompactionCandidatesCmd,
		"history": newCompactionHistoryCmd, "trigger": newCompactionTriggerCmd,
	} {
		_, _, err := execCmd(t, c())
		if err == nil || !strings.Contains(err.Error(), "compaction is disabled on this server") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestCompactionStats_ShowsClusterWidePause(t *testing.T) {
	for _, tc := range []struct{ paused, want string }{
		{`,"paused":true`, "cycle:           idle, paused cluster-wide by a restore (current id 8)\n"},
		{`,"paused":false`, "cycle:           idle (current id 8)\n"},
		{``, "cycle:           idle (current id 8)\n"}, // Arc 26.09 has no "paused"
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"active_jobs":0,"current_cycle_id":8,"cycle_running":false,"exclude_databases":[]` + tc.paused + `,"recent_jobs":[]}`))
		}))
		writeTestConfig(t, srv.URL, "tok")
		out, _, err := execCmd(t, newCompactionStatsCmd())
		srv.Close()
		if err != nil || !strings.Contains(out, tc.want) {
			t.Errorf("paused=%q: err=%v, want %q in:\n%s", tc.paused, err, tc.want, out)
		}
	}
}

// cycleResultLine prints a cycle's counts only from a last_cycle record for
// that very cycle, names every non-zero bucket that is neither compacted nor
// failed, and never falls back to the lifetime totals (arcli#96).
func TestCycleResultLine(t *testing.T) {
	totals := client.CompactionStats{TotalJobsCompleted: 7, TotalJobsFailed: 5, TotalBytesSaved: 1 << 20}
	withCycle := func(c client.CompactionCycle) *client.CompactionStats {
		st := totals
		st.LastCycle = &c
		return &st
	}
	cases := []struct {
		name string
		id   int64
		st   *client.CompactionStats
		want string
	}{
		{"completed, nothing to do", 4, withCycle(client.CompactionCycle{CycleID: 4, Status: "completed"}),
			"Cycle 4 finished: 0 of 0 batches compacted, 0 failed"},
		{"failed batch", 7, withCycle(client.CompactionCycle{CycleID: 7, Status: "failed", Discovered: 6, Started: 6, Succeeded: 5, Failed: 1}),
			"Cycle 7 finished: 5 of 6 batches compacted, 1 failed (status failed)"},
		{"timed out", 9, withCycle(client.CompactionCycle{CycleID: 9, Status: "timed_out", Discovered: 6, Started: 4, Succeeded: 3, Interrupted: 1, Unstarted: 2}),
			"Cycle 9 finished: 3 of 6 batches compacted, 0 failed, 1 interrupted, 2 unstarted (status timed_out)"},
		{"interrupted only", 2, withCycle(client.CompactionCycle{CycleID: 2, Status: "cancelled", Discovered: 1, Started: 1, Interrupted: 1}),
			"Cycle 2 finished: 0 of 1 batch compacted, 0 failed, 1 interrupted (status cancelled)"},
		{"unstarted only", 2, withCycle(client.CompactionCycle{CycleID: 2, Status: "completed", Discovered: 2, Started: 1, Succeeded: 1, Unstarted: 1}),
			"Cycle 2 finished: 1 of 2 batches compacted, 0 failed, 1 unstarted"},
		{"skipped too wide", 5, withCycle(client.CompactionCycle{CycleID: 5, Status: "completed", Discovered: 2, Started: 2, Succeeded: 2, SkippedTooWide: 1}),
			"Cycle 5 finished: 2 of 2 batches compacted, 0 failed, 1 partition skipped as too wide"},
		{"discovery errors explain a failed status", 6, withCycle(client.CompactionCycle{CycleID: 6, Status: "failed", DiscoveryErrors: 2}),
			"Cycle 6 finished: 0 of 0 batches compacted, 0 failed, 2 discovery errors (status failed)"},
		{"every suffix, in order", 8, withCycle(client.CompactionCycle{CycleID: 8, Status: "timed_out", Discovered: 5, Started: 3, Succeeded: 1, Failed: 1, Interrupted: 1, Unstarted: 2, DiscoveryErrors: 1, SkippedTooWide: 3}),
			"Cycle 8 finished: 1 of 5 batches compacted, 1 failed, 1 interrupted, 2 unstarted, 1 discovery error, 3 partitions skipped as too wide (status timed_out)"},
		{"status sanitised", 3, withCycle(client.CompactionCycle{CycleID: 3, Status: "fail\x1b[31med"}),
			"Cycle 3 finished: 0 of 0 batches compacted, 0 failed (status fail[31med)"},
		{"later cycle overwrote the record", 4, withCycle(client.CompactionCycle{CycleID: 5, Status: "completed", Discovered: 9, Succeeded: 9}),
			"Cycle 4 finished; its counts were replaced by a later cycle (last recorded cycle: 5)"},
		{"older record", 4, withCycle(client.CompactionCycle{CycleID: 3, Status: "completed", Discovered: 9, Succeeded: 9}),
			"Cycle 4 finished; its counts were not reported by the server (last recorded cycle: 3)"},
		{"zero block", 4, withCycle(client.CompactionCycle{}),
			"Cycle 4 finished; its counts were not reported by the server (no cycle recorded yet)"},
		{"no last_cycle (Arc before 26.09.2)", 4, &totals,
			"Cycle 4 finished; this server does not report per-cycle counts (Arc 26.09.2+ does)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cycleResultLine(tc.id, tc.st); got != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}

// A last_cycle record for another cycle (here: one that finished between
// two polls) must not be printed as the waited cycle's result.
func TestCompactionTrigger_WaitIgnoresAnotherCyclesRecord(t *testing.T) {
	fastPolls(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/compaction/trigger", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"message":"Compaction triggered","status":"running","tiers":["hourly"],"cycle_id":4}`))
	})
	mux.HandleFunc("/api/v1/compaction/stats", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"total_jobs_completed":1,"total_jobs_failed":0,"total_bytes_saved":24883,"cycle_running":false,"current_cycle_id":5,"recent_jobs":[],
		 "last_cycle":{"cycle_id":5,"status":"completed","discovered_batches":2,"started_batches":2,"succeeded_batches":2,"failed_batches":0,"interrupted_batches":0,"unstarted_batches":0,"discovery_errors":0}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	writeTestConfig(t, srv.URL, "tok")
	out, _, err := execCmd(t, newCompactionTriggerCmd(), "--wait", "--wait-timeout", "30s")
	if err != nil {
		t.Fatal(err)
	}
	if out != "Compaction cycle requested (expected cycle id 4) for tiers [hourly] on all databases\nCycle 4 finished; its counts were replaced by a later cycle (last recorded cycle: 5)\n" {
		t.Errorf("out = %q", out)
	}
}
