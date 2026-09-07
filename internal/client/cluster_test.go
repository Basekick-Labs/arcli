package client

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

const standaloneBody = `{"enabled":false,"mode":"standalone","reason":"Enterprise license not configured"}`

func TestCluster_StandaloneIsClusterDisabledError(t *testing.T) {
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertAuthHeaders(t, r)
		_, _ = w.Write([]byte(standaloneBody))
	})
	ctx := context.Background()
	var cde *ClusterDisabledError

	_, err := cli.ClusterStatus(ctx)
	if !errors.As(err, &cde) || cde.Reason != "Enterprise license not configured" || string(cde.Raw) != standaloneBody {
		t.Errorf("status: err = %v raw=%s", err, cde.Raw)
	}
	_, _, err = cli.ClusterLocal(ctx)
	if !errors.As(err, &cde) {
		t.Errorf("local: err = %v", err)
	}
	_, _, err = cli.ClusterNodes(ctx, "", "")
	if !errors.As(err, &cde) {
		t.Errorf("nodes: err = %v", err)
	}
	_, _, err = cli.ClusterNode(ctx, "n1")
	if !errors.As(err, &cde) {
		t.Errorf("node: err = %v", err)
	}
	_, err = cli.ClusterHealth(ctx)
	if !errors.As(err, &cde) {
		t.Errorf("health: err = %v", err)
	}
	// DELETE also answers 200 + standalone body on a standalone server.
	err = cli.RemoveClusterNode(ctx, "n1")
	if !errors.As(err, &cde) {
		t.Errorf("remove: err = %v", err)
	}
}

func TestClusterStatus_EnabledDecodesAndKeepsRaw(t *testing.T) {
	body := `{"enabled":true,"mode":"cluster","running":true,"cluster_name":"prod","local_node_id":"n1","local_role":"writer",
	 "node_count":2,"healthy_count":2,"writers":1,"readers":1,"compactors":0,
	 "nodes":[{"id":"n1","name":"a","role":"writer","state":"healthy","address":"10.0.0.1:9000","api_address":"10.0.0.1:8000","version":"26.09","last_heartbeat":"2026-09-07T10:00:00.123456789Z","stats":{"cpu_usage":1.5,"storage_used":1024}}],
	 "raft":{"enabled":true,"is_leader":false,"leader_addr":"10.0.0.2:9000","leader_id":"n2","state":"Follower","stats":null},
	 "router":{"strategy":"round_robin"},"license":{"valid":true,"tier":"enterprise","features":null}}`
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/cluster" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(body))
	})
	st, err := cli.ClusterStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.Enabled || st.ClusterName != "prod" || st.LocalNodeID != "n1" || st.NodeCount != 2 || st.Raft.LeaderID != "n2" || st.Raft.IsLeader {
		t.Errorf("st = %+v", st)
	}
	if len(st.Nodes) != 1 || st.Nodes[0].Stats.StorageUsed != 1024 || st.Nodes[0].LastHeartbeat.IsZero() {
		t.Errorf("nodes = %+v", st.Nodes)
	}
	if st.License == nil || st.License.Features == nil || len(st.License.Features) != 0 {
		t.Errorf("license.features null must normalise to []: %+v", st.License)
	}
	if !strings.Contains(string(st.Raw), `"router"`) {
		t.Error("raw body must keep optional blocks")
	}
}

func TestClusterNodes_FiltersValidatedAndSent(t *testing.T) {
	var gotQuery string
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"nodes":[],"total":0}`))
	})
	if _, _, err := cli.ClusterNodes(context.Background(), "boss", ""); err == nil || !strings.Contains(err.Error(), `invalid role "boss"`) {
		t.Errorf("err = %v", err)
	}
	if _, _, err := cli.ClusterNodes(context.Background(), "", "sleepy"); err == nil || !strings.Contains(err.Error(), `invalid state "sleepy"`) {
		t.Errorf("err = %v", err)
	}
	nodes, _, err := cli.ClusterNodes(context.Background(), "writer", "healthy")
	if err != nil {
		t.Fatal(err)
	}
	if nodes == nil || len(nodes) != 0 {
		t.Errorf("nodes = %#v", nodes)
	}
	if gotQuery != "role=writer&state=healthy" {
		t.Errorf("query = %q", gotQuery)
	}
}

func TestClusterNode_ZeroTimeAnd404(t *testing.T) {
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/cluster/nodes/n1":
			_, _ = w.Write([]byte(`{"id":"n1","name":"a","role":"reader","state":"joining","joined_at":"0001-01-01T00:00:00Z","last_heartbeat":"0001-01-01T00:00:00Z","failed_checks":3,"stats":{}}`))
		default:
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":"Node not found"}`))
		}
	})
	n, _, err := cli.ClusterNode(context.Background(), "n1")
	if err != nil {
		t.Fatal(err)
	}
	if !n.JoinedAt.IsZero() || !n.LastHeartbeat.IsZero() || n.FailedChecks != 3 {
		t.Errorf("n = %+v", n)
	}
	_, _, err = cli.ClusterNode(context.Background(), "nope")
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 404 || he.Message != "Node not found" {
		t.Errorf("err = %v", err)
	}
	if _, _, err := cli.ClusterNode(context.Background(), ""); err == nil {
		t.Error("empty id must be rejected client-side")
	}
	if _, _, err := cli.ClusterNode(context.Background(), strings.Repeat("x", 257)); err == nil {
		t.Error("overlong id must be rejected client-side")
	}
}

func TestValidateNodeID(t *testing.T) {
	for _, ok := range []string{"n1", "node-a.b_c", "ünïcödé", strings.Repeat("x", 256)} {
		if err := ValidateNodeID(ok); err != nil {
			t.Errorf("%q should be valid: %v", ok, err)
		}
	}
	for _, bad := range []string{"", ".", "..", "a/b", "a?b", "a#b", "a%2Fb", "a b", "a\x1bb", strings.Repeat("x", 257)} {
		if err := ValidateNodeID(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

// Ids that pass validation are percent-encoded so they stay a single
// path segment on the wire.
func TestClusterNode_IDIsPathEscaped(t *testing.T) {
	var seen string
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.EscapedPath()
		_, _ = w.Write([]byte(`{"id":"x"}`))
	})
	if _, _, err := cli.ClusterNode(context.Background(), "ünï:cödé"); err != nil {
		t.Fatal(err)
	}
	if seen != "/api/v1/cluster/nodes/%C3%BCn%C3%AF:c%C3%B6d%C3%A9" {
		t.Errorf("escaped path = %q", seen)
	}
}

func TestRemoveClusterNode_NotLeaderAndSelf(t *testing.T) {
	var seen string
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.Method + " " + r.URL.Path
		switch r.URL.Path {
		case "/api/v1/cluster/nodes/follower-target":
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"success":false,"error":"not the leader"}`))
		case "/api/v1/cluster/nodes/me":
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"success":false,"error":"cannot remove self from cluster — use graceful shutdown instead"}`))
		default:
			_, _ = w.Write([]byte(`{"success":true,"message":"node removed from cluster","node_id":"n3"}`))
		}
	})
	err := cli.RemoveClusterNode(context.Background(), "follower-target")
	if !errors.Is(err, ErrNotLeader) {
		t.Errorf("err = %v", err)
	}
	err = cli.RemoveClusterNode(context.Background(), "me")
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 400 || !strings.Contains(he.Message, "cannot remove self") {
		t.Errorf("err = %v", err)
	}
	if err := cli.RemoveClusterNode(context.Background(), "n3"); err != nil {
		t.Errorf("err = %v", err)
	}
	if seen != "DELETE /api/v1/cluster/nodes/n3" {
		t.Errorf("seen = %q", seen)
	}
}
