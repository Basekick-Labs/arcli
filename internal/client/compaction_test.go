package client

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestCompaction_DisabledIs404CannotGet(t *testing.T) {
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_, _ = w.Write([]byte(`{"status":"ok","time":"t","uptime":"1s"}`))
			return
		}
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":"Cannot ` + r.Method + ` ` + r.URL.Path + `"}`))
	})
	ctx := context.Background()
	var cde *CompactionDisabledError
	if _, err := cli.CompactionStatus(ctx); !errors.As(err, &cde) {
		t.Errorf("status: %v", err)
	}
	if _, err := cli.CompactionStats(ctx); !errors.As(err, &cde) {
		t.Errorf("stats: %v", err)
	}
	if _, _, err := cli.CompactionCandidates(ctx); !errors.As(err, &cde) {
		t.Errorf("candidates: %v", err)
	}
	if _, err := cli.CompactionHistory(ctx, 10); !errors.As(err, &cde) {
		t.Errorf("history: %v", err)
	}
	if _, err := cli.TriggerCompaction(ctx, nil, ""); !errors.As(err, &cde) {
		t.Errorf("trigger: %v", err)
	}
}

// The same "Cannot GET" 404 from a host whose /health is not Arc's must
// NOT be reported as "compaction disabled" — it is a wrong base path or
// a different service.
func TestCompaction_CannotGetOnNonArcIsNotDisabled(t *testing.T) {
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":"Cannot ` + r.Method + ` ` + r.URL.Path + `"}`))
	})
	_, err := cli.CompactionStatus(context.Background())
	var cde *CompactionDisabledError
	if errors.As(err, &cde) {
		t.Fatalf("misclassified as disabled: %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "check the endpoint path") {
		t.Errorf("err = %v", err)
	}
}

// A 404 whose message is not Fiber's "Cannot ..." is a real not-found
// from a handler and must NOT be reported as "compaction disabled".
func TestCompaction_Other404IsNotDisabled(t *testing.T) {
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":"something else"}`))
	})
	_, err := cli.CompactionStatus(context.Background())
	var cde *CompactionDisabledError
	if errors.As(err, &cde) {
		t.Errorf("misclassified as disabled: %v", err)
	}
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 404 {
		t.Errorf("err = %v", err)
	}
}

func TestCompactionStatus_NullActiveJobsAndLocalOffsetNextRun(t *testing.T) {
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertAuthHeaders(t, r)
		_, _ = w.Write([]byte(`{"manager":{"active_jobs":null,"total_completed":3,"total_failed":1},"schedulers":{"hourly":{"enabled":true,"running":true,"schedule":"5 * * * *","next_run":"2026-09-07T13:05:00-06:00"},"daily":{"enabled":false,"running":false,"schedule":"0 3 * * *"}}}`))
	})
	st, err := cli.CompactionStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Manager.ActiveJobs != nil || st.Manager.TotalCompleted != 3 {
		t.Errorf("manager = %+v", st.Manager)
	}
	h := st.Schedulers["hourly"]
	if h.NextRun == nil || h.NextRun.UTC().Hour() != 19 {
		t.Errorf("next_run with -06:00 offset should be 19:05 UTC, got %v", h.NextRun)
	}
	if d := st.Schedulers["daily"]; d.NextRun != nil || d.Enabled {
		t.Errorf("daily = %+v", d)
	}
}

func TestCompactionStats_TiersAbsentAndPresent(t *testing.T) {
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"total_jobs_completed":0,"total_jobs_failed":0,"total_files_compacted":0,"total_bytes_saved":0,"total_bytes_saved_mb":0,"total_manifests_recover":0,"cycle_running":false,"current_cycle_id":0,"recent_jobs":[]}`))
	})
	st, err := cli.CompactionStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Tiers != nil {
		t.Errorf("absent tiers must decode as nil, got %#v", st.Tiers)
	}
	if st.RecentJobs == nil {
		t.Error("recent_jobs must be non-nil")
	}

	cli2, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"cycle_running":true,"current_cycle_id":7,"recent_jobs":[{"database":"m","measurement":"cpu","partition_path":"m/cpu/2026/09/07/10","tier":"hourly","files_compacted":12,"bytes_before":1000,"bytes_after":400,"success":true,"compression_ratio":0.6}],"tiers":[{"tier":"hourly","enabled":true,"min_age_hours":1,"min_files":10,"total_compactions":1,"total_files_compacted":12,"total_bytes_saved":600,"total_bytes_saved_mb":0.0005}]}`))
	})
	st2, err := cli2.CompactionStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(st2.Tiers) != 1 || st2.Tiers[0].MinFiles != 10 || !st2.CycleRunning || st2.CurrentCycleID != 7 {
		t.Errorf("st2 = %+v", st2)
	}
	j := st2.RecentJobs[0]
	if j.FilesCompacted == nil || *j.FilesCompacted != 12 || j.CompressionRatio == nil || *j.CompressionRatio != 0.6 || j.Success == nil || !*j.Success {
		t.Errorf("job = %+v", j)
	}
}

func TestCompactionHistory_LimitSentAndValidated(t *testing.T) {
	var q string
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		q = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"total_jobs":2,"recent_jobs":[{"database":"a","measurement":"b","partition_path":"p","tier":"daily","error":"boom","success":false}]}`))
	})
	if _, err := cli.CompactionHistory(context.Background(), 0); err == nil {
		t.Error("limit 0 must be rejected")
	}
	h, err := cli.CompactionHistory(context.Background(), 5)
	if err != nil {
		t.Fatal(err)
	}
	if q != "limit=5" || h.TotalJobs != 2 || len(h.RecentJobs) != 1 || h.RecentJobs[0].Error != "boom" || h.RecentJobs[0].FilesCompacted != nil {
		t.Errorf("q=%q h=%+v", q, h)
	}
}

func TestTriggerCompaction_QueryValidationAnd409(t *testing.T) {
	var seen string
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.Method + " " + r.URL.String()
		if r.Body != nil {
			b := make([]byte, 1)
			if n, _ := r.Body.Read(b); n != 0 {
				t.Error("trigger must send no body")
			}
		}
		if strings.Contains(r.URL.RawQuery, "database=busy") {
			w.WriteHeader(409)
			_, _ = w.Write([]byte(`{"error":"Compaction cycle already running","cycle_id":4,"is_running":true}`))
			return
		}
		_, _ = w.Write([]byte(`{"message":"Compaction triggered","status":"running","tiers":null,"cycle_id":5}`))
	})
	ctx := context.Background()
	if _, err := cli.TriggerCompaction(ctx, []string{"weekly"}, ""); err == nil || !strings.Contains(err.Error(), `invalid tier "weekly"`) {
		t.Errorf("err = %v", err)
	}
	if _, err := cli.TriggerCompaction(ctx, nil, "1bad"); err == nil || !strings.Contains(err.Error(), "invalid database name") {
		t.Errorf("err = %v", err)
	}
	res, err := cli.TriggerCompaction(ctx, []string{"hourly", "daily"}, "metrics")
	if err != nil {
		t.Fatal(err)
	}
	if seen != "POST /api/v1/compaction/trigger?database=metrics&tier=hourly%2Cdaily" {
		t.Errorf("seen = %q", seen)
	}
	if res.CycleID != 5 || res.Tiers == nil || len(res.Tiers) != 0 {
		t.Errorf("res = %+v (null tiers must normalise to [])", res)
	}
	_, err = cli.TriggerCompaction(ctx, nil, "busy")
	var cre *CompactionRunningError
	if !errors.As(err, &cre) || cre.CycleID != 4 {
		t.Errorf("err = %v", err)
	}
}
