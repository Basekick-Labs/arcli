package commands

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/basekick-labs/arcli/internal/client"
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

func TestClusterStatus_CompactionPause(t *testing.T) {
	response := `{"enabled":true,"mode":"cluster","running":true,"cluster_name":"prod","nodes":[],"raft":{"enabled":false},"compaction_pause":{"active":true,"generation":7,"requested_by":"n1","reason":"restore backup-20261027-120000-abcdef12","requested_at":"2026-10-27T12:00:00Z","expires_at":"2026-10-27T12:06:00Z","expired":false,"acks":["n1"],"pending_acks":["n2"]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(response))
	}))
	defer srv.Close()
	writeTestConfig(t, srv.URL, "tok")

	out, _, err := execCmd(t, newClusterStatusCmd())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"compaction:  paused by n1 for restore backup-20261027-120000-abcdef12",
		"lease expires 2026-10-27T12:06:00Z",
		"waiting for n2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}

	response = `{"enabled":true,"mode":"cluster","running":true,"cluster_name":"prod","nodes":[],"raft":{"enabled":false},"compaction_pause":{"active":false,"generation":7,"expired":true,"requested_by":"n1","reason":"restore backup-20261027-120000-abcdef12","requested_at":"2026-10-27T12:00:00Z","expires_at":"2026-10-27T12:06:00Z","acks":["n1","n2"],"pending_acks":[]}}`
	out, _, err = execCmd(t, newClusterStatusCmd())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "compaction:  pause lease expired (requested by n1 for restore backup-20261027-120000-abcdef12, lease expired 2026-10-27T12:06:00Z)") {
		t.Errorf("expired pause should be shown:\n%s", out)
	}

	response = `{"enabled":true,"mode":"cluster","running":true,"cluster_name":"prod","nodes":[],"raft":{"enabled":false},"compaction_pause":{"active":true,"generation":7,"requested_by":"n1","reason":"restore backup-20261027-120000-abcdef12","requested_at":"2026-10-27T12:00:00Z","expires_at":"2026-10-27T12:06:00Z","expired":false,"acks":["n1"],"pending_acks":["n2"]}}`
	out, _, err = execCmd(t, newClusterStatusCmd(), "-o", "json")
	if err != nil || !strings.Contains(out, `"compaction_pause"`) || !strings.Contains(out, `"pending_acks"`) || !strings.Contains(out, `"n2"`) {
		t.Errorf("JSON must preserve the pause object: err=%v\n%s", err, out)
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

// The leader hint derives a hostless API address ("", any unspecified IP)
// from the coordinator address, keeping the API port; an explicit or
// unparseable API address is shown unchanged; with no usable host the hint
// names the API port, and with no API address it keeps the legacy text
// (#94). Every case compares the whole string.
func TestLeaderHintFor_APIAddressShapes(t *testing.T) {
	// legacyHint is the pre-#94 format string, byte for byte.
	legacyHint := func(scheme, addr string) string {
		return fmt.Sprintf("re-run against the leader %q at %s://%s — verify that address against your inventory, then use a connection profile for it (`arcli config create`) or ARC_ENDPOINT/ARC_TOKEN", "n2", scheme, addr)
	}
	derived := func(scheme, addr string) string {
		return `re-run against the leader "n2" at ` + scheme + "://" + addr + " (host taken from the node's cluster address) — verify that address against your inventory, then use a connection profile for it (`arcli config create`) or ARC_ENDPOINT/ARC_TOKEN"
	}
	portOnly := func(port string) string {
		return `the leader is node "n2" (API port ` + port + "; its host is not published); find its address with `arcli cluster nodes`"
	}
	const legacyFallback = "the leader is node \"n2\"; find its API address with `arcli cluster nodes`"
	cases := []struct {
		name, api, addr, scheme, want string
	}{
		{"bind-only port", ":8000", "h:9100", "http", derived("http", "h:8000")},
		{"ipv6 unspecified api, ipv6 coordinator", "[::]:8000", "[fd00::1]:9100", "http", derived("http", "[fd00::1]:8000")},
		{"ipv4 unspecified api", "0.0.0.0:8000", "h:9100", "http", derived("http", "h:8000")},
		{"ipv6 unspecified api spelled ::0", "[::0]:8000", "h:9100", "http", derived("http", "h:8000")},
		{"https kept", ":8443", "arc-writer1:9100", "https", derived("https", "arc-writer1:8443")},
		{"unspecified api, no coordinator", "0.0.0.0:8000", "", "http", portOnly("8000")},
		{"hostless api, ipv4 unspecified coordinator", ":8000", "0.0.0.0:9100", "http", portOnly("8000")},
		{"hostless api, ipv6 unspecified coordinator", ":8000", "[::]:9100", "http", portOnly("8000")},
		{"hostless api, ::0 coordinator", ":8000", "[::0]:9100", "http", portOnly("8000")},
		{"hostless api, hostless coordinator", ":8000", ":9100", "http", portOnly("8000")},
		{"hostless api, zoned coordinator", ":8000", "[fe80::1%eth0]:9100", "http", portOnly("8000")},
		{"no api address", "", "h:9100", "http", legacyFallback},
		{"explicit host unchanged", "10.0.0.2:8000", "10.0.0.9:9000", "http", legacyHint("http", "10.0.0.2:8000")},
		{"explicit ipv6 unchanged", "[fd00::2]:8000", "x:9100", "http", legacyHint("http", "[fd00::2]:8000")},
		{"explicit zoned ipv6 unchanged", "[fe80::2%eth0]:8000", "x:9100", "http", legacyHint("http", "[fe80::2%eth0]:8000")},
		{"unparseable unchanged", "arc-api", "x:9100", "http", legacyHint("http", "arc-api")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &client.ClusterStatus{Nodes: []client.ClusterNode{
				{ID: "n1", Address: "10.0.0.1:9000", APIAddress: "10.0.0.1:8000"},
				{ID: "n2", Address: tc.addr, APIAddress: tc.api},
			}}
			st.Raft.LeaderID = "n2"
			if got := leaderHintFor(st, tc.scheme); got != tc.want {
				t.Errorf("hint = %q\nwant   %q", got, tc.want)
			}
		})
	}
}

// Duplicate leader entries: as before #94, an entry that yields no address
// does not end the scan.
func TestLeaderHintFor_KeepsScanningDuplicateLeaderEntries(t *testing.T) {
	st := &client.ClusterStatus{Nodes: []client.ClusterNode{
		{ID: "n2", Address: "", APIAddress: ":8000"},
		{ID: "n2", Address: "10.0.0.2:9000", APIAddress: "10.0.0.2:8000"},
	}}
	st.Raft.LeaderID = "n2"
	got := leaderHintFor(st, "http")
	if !strings.HasPrefix(got, `re-run against the leader "n2" at http://10.0.0.2:8000 — verify`) {
		t.Errorf("hint = %q", got)
	}
	st.Nodes = []client.ClusterNode{{ID: "n2", APIAddress: ":8000"}, {ID: "n2", APIAddress: ":8001"}}
	if got := leaderHintFor(st, "http"); !strings.Contains(got, "(API port 8000;") {
		t.Errorf("first hostless entry's port must win: %q", got)
	}
}

// End to end through `cluster node remove`: a follower answers "not the
// leader" and the leader publishes a bind-only API address.
func TestClusterNodeRemove_LeaderHintDerivesHost(t *testing.T) {
	nodes := `[{"id":"n1","address":"10.0.0.1:9000","api_address":":8000"},{"id":"n2","address":"[fd00::2]:9000","api_address":"[::]:8000"}]`
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/cluster", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"enabled":true,"mode":"cluster","running":true,"cluster_name":"prod","local_node_id":"n1","local_role":"writer","nodes":` + nodes + `,"raft":{"enabled":true,"is_leader":false,"leader_addr":"[fd00::2]:9000","leader_id":"n2","state":"Follower"}}`))
	})
	mux.HandleFunc("/api/v1/cluster/nodes/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"success":false,"error":"not the leader"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	writeTestConfig(t, srv.URL, "tok")
	_, _, err := execCmd(t, newClusterNodeRemoveCmd(), "n3", "--yes")
	want := `re-run against the leader "n2" at http://[fd00::2]:8000 (host taken from the node's cluster address) — verify`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want it to contain %q", err, want)
	}
}
