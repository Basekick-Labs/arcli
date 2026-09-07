package commands

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

const standaloneJSON = `{"enabled":false,"mode":"standalone","reason":"Enterprise license not configured"}`

// fakeClusterServer emulates a 2-node cluster where the local node n1
// is a follower and n2 is the leader. removeStatus lets tests choose
// the DELETE outcome.
func fakeClusterServer(t *testing.T, removeStatus int, removeBody string) *httptest.Server {
	t.Helper()
	nodes := `[{"id":"n1","name":"alpha","role":"writer","state":"healthy","address":"10.0.0.1:9000","api_address":"10.0.0.1:8000","cluster_name":"prod","version":"26.09","started_at":"2026-09-07T09:00:00Z","joined_at":"2026-09-07T09:00:01Z","last_heartbeat":"2026-09-07T10:00:00.5Z","failed_checks":0,"stats":{"cpu_usage":12.5,"memory_usage":40,"storage_used":2147483648}},
	 {"id":"n2","name":"beta","role":"reader","state":"unhealthy","address":"10.0.0.2:9000","api_address":"10.0.0.2:8000","cluster_name":"prod","version":"26.09","started_at":"0001-01-01T00:00:00Z","joined_at":"0001-01-01T00:00:00Z","last_heartbeat":"0001-01-01T00:00:00Z","failed_checks":3,"stats":{}}]`
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/cluster", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"enabled":true,"mode":"cluster","running":true,"cluster_name":"prod","local_node_id":"n1","local_role":"writer","node_count":2,"healthy_count":1,"writers":1,"readers":1,"compactors":0,"nodes":` + nodes + `,"raft":{"enabled":true,"is_leader":false,"leader_addr":"10.0.0.2:9000","leader_id":"n2","state":"Follower","stats":null},"license":{"valid":true,"tier":"enterprise","features":null}}`))
	})
	mux.HandleFunc("/api/v1/cluster/nodes", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"nodes":` + nodes + `,"total":2}`))
	})
	mux.HandleFunc("/api/v1/cluster/nodes/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/cluster/nodes/")
		if r.Method == http.MethodDelete {
			w.WriteHeader(removeStatus)
			_, _ = w.Write([]byte(removeBody))
			return
		}
		if id != "n1" {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":"Node not found"}`))
			return
		}
		var list []json.RawMessage
		_ = json.Unmarshal([]byte(nodes), &list)
		_, _ = w.Write(list[0])
	})
	mux.HandleFunc("/api/v1/cluster/local", func(w http.ResponseWriter, r *http.Request) {
		var list []json.RawMessage
		_ = json.Unmarshal([]byte(nodes), &list)
		body := strings.TrimSuffix(string(list[0]), "}") + `,"capabilities":{"can_ingest":true,"can_query":true,"can_compact":false,"can_coordinate":true},"is_local":true}`
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("/api/v1/cluster/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"healthy":1,"unhealthy":1,"total":2,"health_checker":{"running":true,"check_interval_ms":5000,"check_timeout_ms":2000,"unhealthy_threshold":3}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func fakeStandaloneServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(standaloneJSON))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestClusterStatus_Standalone(t *testing.T) {
	srv := fakeStandaloneServer(t)
	writeTestConfig(t, srv.URL, "tok")
	out, _, err := execCmd(t, newClusterStatusCmd())
	if err != nil {
		t.Fatalf("status on standalone must exit 0: %v", err)
	}
	if !strings.Contains(out, "clustering:  disabled (Enterprise license not configured)") {
		t.Errorf("out = %q", out)
	}
	out, _, err = execCmd(t, newClusterStatusCmd(), "-o", "json")
	if err != nil || !strings.Contains(out, `"enabled": false`) || !strings.Contains(out, `"reason"`) {
		t.Errorf("json = %s err=%v", out, err)
	}
}

func TestClusterOthers_StandaloneExit1(t *testing.T) {
	srv := fakeStandaloneServer(t)
	writeTestConfig(t, srv.URL, "tok")
	for name, c := range map[string]func() *cobra.Command{
		"nodes": newClusterNodesCmd, "health": newClusterHealthCmd,
	} {
		_, _, err := execCmd(t, c())
		if err == nil || !strings.Contains(err.Error(), "clustering is not enabled on this server: Enterprise license not configured") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	_, _, err := execCmd(t, newClusterNodeShowCmd(), "n1")
	if err == nil || !strings.Contains(err.Error(), "clustering is not enabled") {
		t.Errorf("node show: err = %v", err)
	}
	_, _, err = execCmd(t, newClusterNodeRemoveCmd(), "n1", "--yes")
	if err == nil || !strings.Contains(err.Error(), "clustering is not enabled") {
		t.Errorf("node remove: err = %v", err)
	}
}

func TestClusterStatus_Enabled(t *testing.T) {
	srv := fakeClusterServer(t, 200, `{"success":true}`)
	writeTestConfig(t, srv.URL, "tok")
	out, _, err := execCmd(t, newClusterStatusCmd())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"clustering:  enabled", "cluster:     prod", "local node:  n1 (writer)", "2 total, 1 healthy", "leader n2, this node is leader: false", "license:     enterprise", "│ n1 ", "│ n2 "} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	out, _, err = execCmd(t, newClusterStatusCmd(), "-o", "json")
	if err != nil || !strings.Contains(out, `"features": null`) {
		t.Errorf("json must pass raw null through: err=%v\n%s", err, out)
	}
}

func TestClusterNodes_TableCSVAndFilters(t *testing.T) {
	srv := fakeClusterServer(t, 200, "")
	writeTestConfig(t, srv.URL, "tok")
	out, _, err := execCmd(t, newClusterNodesCmd())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "2026-09-07T10:00:00Z") || !strings.Contains(out, "never") {
		t.Errorf("heartbeat rendering: %s", out)
	}
	out, _, err = execCmd(t, newClusterNodesCmd(), "-o", "csv")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[2], "n2,beta,reader,unhealthy,10.0.0.2:9000,10.0.0.2:8000,26.09,,3") {
		t.Errorf("csv = %q", out)
	}
	_, _, err = execCmd(t, newClusterNodesCmd(), "--role", "boss")
	if err == nil || !strings.Contains(err.Error(), `invalid --role "boss"`) {
		t.Errorf("err = %v", err)
	}
}

func TestClusterNodeShow_ByIDAndLocal(t *testing.T) {
	srv := fakeClusterServer(t, 200, "")
	writeTestConfig(t, srv.URL, "tok")
	out, _, err := execCmd(t, newClusterNodeShowCmd(), "n1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "id:             n1") || !strings.Contains(out, "storage 2.0 GiB") {
		t.Errorf("out = %s", out)
	}
	out, _, err = execCmd(t, newClusterNodeShowCmd(), "--local")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "capabilities:   coordinate, ingest, query") {
		t.Errorf("out = %s", out)
	}
	_, _, err = execCmd(t, newClusterNodeShowCmd())
	if err == nil || !strings.Contains(err.Error(), "exactly one of a node id or --local") {
		t.Errorf("err = %v", err)
	}
	_, _, err = execCmd(t, newClusterNodeShowCmd(), "nope")
	if err == nil || !strings.Contains(err.Error(), "Node not found") {
		t.Errorf("err = %v", err)
	}
}

func TestClusterNodeRemove_SelfNotLeaderAndSuccess(t *testing.T) {
	// Follower answers "not the leader": the hint must name n2's API
	// address, never raft.leader_addr.
	srv := fakeClusterServer(t, 500, `{"success":false,"error":"not the leader"}`)
	writeTestConfig(t, srv.URL, "tok")
	_, errOut, err := execCmd(t, newClusterNodeRemoveCmd(), "n2", "--yes")
	if err == nil || !strings.Contains(err.Error(), "not the raft leader") || !strings.Contains(err.Error(), "http://10.0.0.2:8000") {
		t.Errorf("err = %v", err)
	}
	if strings.Contains(err.Error(), "10.0.0.2:9000") {
		t.Error("hint must not point at the raft transport address")
	}
	if !strings.Contains(errOut, "note: this node is not the raft leader (leader: n2)") {
		t.Errorf("stderr = %q", errOut)
	}

	_, _, err = execCmd(t, newClusterNodeRemoveCmd(), "n1", "--yes")
	if err == nil || !strings.Contains(err.Error(), "refuses self-removal") {
		t.Errorf("self: err = %v", err)
	}

	srv2 := fakeClusterServer(t, 200, `{"success":true,"message":"node removed from cluster","node_id":"n2"}`)
	writeTestConfig(t, srv2.URL, "tok")
	out, _, err := execCmd(t, newClusterNodeRemoveCmd(), "n2", "--yes")
	if err != nil || !strings.Contains(out, `Removed node "n2" from cluster "prod"`) {
		t.Errorf("err=%v out=%q", err, out)
	}
}

func TestClusterHealth_Table(t *testing.T) {
	srv := fakeClusterServer(t, 200, "")
	writeTestConfig(t, srv.URL, "tok")
	out, _, err := execCmd(t, newClusterHealthCmd())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "2 total, 1 healthy, 1 unhealthy") || !strings.Contains(out, "interval=5s timeout=2s unhealthy after 3 failed checks") {
		t.Errorf("out = %s", out)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 1536: "1.5 KiB", 2147483648: "2.0 GiB"}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}
