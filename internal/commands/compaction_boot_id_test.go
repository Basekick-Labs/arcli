package commands

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// bootIDServer is an Arc 27.01.1 compaction API: the trigger answers a
// boot_id, and /cycles/{id} answers "running" for `runningPolls` polls,
// then the final record. mismatch makes /cycles answer the 404 a restarted
// (or another) process gives.
func bootIDServer(t *testing.T, runningPolls int32, mismatch bool) (*httptest.Server, *atomic.Int32, *atomic.Value) {
	t.Helper()
	var polls atomic.Int32
	var gotBoot atomic.Value
	gotBoot.Store("")
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/compaction/trigger", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"message":"Compaction triggered","status":"running","tiers":["hourly"],"cycle_id":1,"boot_id":"aaaa000011112222"}`))
	})
	mux.HandleFunc("/api/v1/compaction/cycles/1", func(w http.ResponseWriter, r *http.Request) {
		gotBoot.Store(r.URL.Query().Get("boot_id"))
		if mismatch {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":"no record of this compaction cycle in this process","cycle_id":1,"boot_id":"bbbb333344445555","boot_id_mismatch":true}`))
			return
		}
		if polls.Add(1) <= runningPolls {
			_, _ = w.Write([]byte(`{"cycle_id":1,"status":"running","boot_id":"aaaa000011112222","discovered_batches":4,"started_batches":1}`))
			return
		}
		_, _ = w.Write([]byte(`{"cycle_id":1,"status":"stopped","boot_id":"aaaa000011112222","discovered_batches":4,"started_batches":2,"succeeded_batches":2,"failed_batches":0,"unstarted_batches":2}`))
	})
	mux.HandleFunc("/api/v1/compaction/stats", func(w http.ResponseWriter, r *http.Request) {
		t.Error("a boot_id server must be polled through /cycles, not /stats")
		_, _ = w.Write([]byte(`{"cycle_running":false,"current_cycle_id":1,"recent_jobs":[]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &polls, &gotBoot
}

// Against Arc 27.01.1+, --wait polls the cycle itself, by id and boot_id,
// and prints its own record, including a "stopped" status.
func TestCompactionTrigger_WaitPollsTheCycleByBootID(t *testing.T) {
	fastPolls(t)
	srv, polls, gotBoot := bootIDServer(t, 2, false)
	writeTestConfig(t, srv.URL, "tok")
	out, _, err := execCmd(t, newCompactionTriggerCmd(), "--wait", "--wait-timeout", "30s")
	if err != nil {
		t.Fatal(err)
	}
	if gotBoot.Load() != "aaaa000011112222" {
		t.Errorf("poll sent boot_id %q", gotBoot.Load())
	}
	if polls.Load() != 3 {
		t.Errorf("polls = %d, want 3 (two running, one final)", polls.Load())
	}
	if !strings.Contains(out, "Compaction cycle 1 started (boot_id aaaa000011112222)") {
		t.Errorf("progress line: %q", out)
	}
	if !strings.Contains(out, "Cycle 1 finished: 2 of 4 batches compacted, 0 failed, 2 unstarted (status stopped)\n") {
		t.Errorf("result line: %q", out)
	}
}

// A poll that reaches another process reports that the cycle's outcome is
// not known, instead of another cycle's record under the same number.
func TestCompactionTrigger_WaitReportsAnotherProcess(t *testing.T) {
	fastPolls(t)
	srv, _, _ := bootIDServer(t, 0, true)
	writeTestConfig(t, srv.URL, "tok")
	_, _, err := execCmd(t, newCompactionTriggerCmd(), "--wait", "--wait-timeout", "30s")
	if err == nil || !strings.Contains(err.Error(), "was started by another Arc process (boot_id aaaa000011112222; the server now answers as bbbb333344445555)") {
		t.Fatalf("err = %v", err)
	}
}

// -o json with --wait prints the cycle's record.
func TestCompactionTrigger_WaitJSONPrintsTheCycleRecord(t *testing.T) {
	fastPolls(t)
	srv, _, _ := bootIDServer(t, 0, false)
	writeTestConfig(t, srv.URL, "tok")
	out, _, err := execCmd(t, newCompactionTriggerCmd(), "--wait", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"status": "stopped"`) || !strings.Contains(out, `"boot_id": "aaaa000011112222"`) {
		t.Errorf("stdout = %q", out)
	}
}

// A server shutting down answers 503 "stopping".
func TestCompactionTrigger_ServerStopping(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/compaction/trigger", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":"the server is shutting down","message":"compaction is not taking new cycles; retry once the server is back","stopping":true}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	writeTestConfig(t, srv.URL, "tok")
	_, _, err := execCmd(t, newCompactionTriggerCmd())
	if err == nil || !strings.Contains(err.Error(), "shutting down") {
		t.Fatalf("err = %v", err)
	}
}

// A 409 names the running cycle; stats shows the boot id.
func TestCompactionStats_ShowsBootID(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/compaction/stats", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"cycle_running":false,"current_cycle_id":3,"boot_id":"cccc666677778888","recent_jobs":[]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	writeTestConfig(t, srv.URL, "tok")
	out, _, err := execCmd(t, newCompactionStatsCmd())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "boot id:         cccc666677778888") {
		t.Errorf("out = %q", out)
	}
}
