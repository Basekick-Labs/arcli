package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ClusterDisabledError is returned by every cluster method when the
// server runs standalone. Arc registers the cluster routes
// unconditionally and answers HTTP 200 {"enabled":false,"mode":
// "standalone","reason":"..."} when no coordinator exists (no
// enterprise license, feature missing, or cluster.enabled=false), so
// this is the only signal a client gets.
type ClusterDisabledError struct {
	Reason string
	// Raw is the server's body so `cluster status -o json` can pass it
	// through unchanged.
	Raw json.RawMessage
}

func (e *ClusterDisabledError) Error() string {
	return "clustering is not enabled on this server: " + e.Reason
}

// ClusterNodeRoles / ClusterNodeStates mirror the server's filter
// enums (arc/internal/api/cluster.go) so a typo fails before HTTP.
var (
	ClusterNodeRoles  = []string{"writer", "reader", "compactor", "standalone"}
	ClusterNodeStates = []string{"healthy", "unhealthy", "dead", "unknown", "joining", "leaving"}
)

// NodeStats is the per-node resource block (arc/internal/cluster/node.go#NodeStats).
type NodeStats struct {
	CPUUsage       float64 `json:"cpu_usage"`
	MemoryUsage    float64 `json:"memory_usage"`
	IngestRate     int64   `json:"ingest_rate"`
	QueryRate      int64   `json:"query_rate"`
	StorageUsed    int64   `json:"storage_used"`
	Connections    int     `json:"connections"`
	ActiveQueries  int     `json:"active_queries"`
	CompactionJobs int     `json:"compaction_jobs"`
}

// ClusterNode is one node as returned by /api/v1/cluster/nodes and
// /api/v1/cluster/nodes/:id (nodeToMap). The narrower node entries
// embedded in /api/v1/cluster lack cluster_name, started_at, joined_at
// and failed_checks; those decode as zero values.
type ClusterNode struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Role          string    `json:"role"`
	State         string    `json:"state"`
	Address       string    `json:"address"`
	APIAddress    string    `json:"api_address"`
	ClusterName   string    `json:"cluster_name,omitempty"`
	Version       string    `json:"version"`
	StartedAt     time.Time `json:"started_at"`
	JoinedAt      time.Time `json:"joined_at"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
	FailedChecks  int       `json:"failed_checks"`
	Stats         NodeStats `json:"stats"`
}

// RaftStatus is the raft block of /api/v1/cluster. Stats is kept raw
// because the server sends `null` when raft is not running. A cluster
// with raft disabled omits the "raft" key entirely, so Enabled=false
// there comes from the zero value.
type RaftStatus struct {
	Enabled    bool            `json:"enabled"`
	IsLeader   bool            `json:"is_leader"`
	LeaderAddr string          `json:"leader_addr"`
	LeaderID   string          `json:"leader_id"`
	State      string          `json:"state"`
	Stats      json.RawMessage `json:"stats"`
}

// ClusterLicense is the license block of /api/v1/cluster. Features may
// be null on the wire.
type ClusterLicense struct {
	Valid    bool     `json:"valid"`
	Tier     string   `json:"tier"`
	Features []string `json:"features"`
}

// ClusterStatus is the typed view of GET /api/v1/cluster on a
// cluster-enabled server. Raw holds the complete body for JSON output
// so optional blocks (router, core counts) are never lost.
type ClusterStatus struct {
	Enabled      bool            `json:"enabled"`
	Mode         string          `json:"mode"`
	Running      bool            `json:"running"`
	ClusterName  string          `json:"cluster_name"`
	LocalNodeID  string          `json:"local_node_id"`
	LocalRole    string          `json:"local_role"`
	NodeCount    int             `json:"node_count"`
	HealthyCount int             `json:"healthy_count"`
	Writers      int             `json:"writers"`
	Readers      int             `json:"readers"`
	Compactors   int             `json:"compactors"`
	Nodes        []ClusterNode   `json:"nodes"`
	Raft         RaftStatus      `json:"raft"`
	License      *ClusterLicense `json:"license,omitempty"`
	Raw          json.RawMessage `json:"-"`
}

// ClusterHealth is GET /api/v1/cluster/health.
type ClusterHealth struct {
	Healthy       int `json:"healthy"`
	Unhealthy     int `json:"unhealthy"`
	Total         int `json:"total"`
	HealthChecker struct {
		Running            bool  `json:"running"`
		CheckIntervalMs    int64 `json:"check_interval_ms"`
		CheckTimeoutMs     int64 `json:"check_timeout_ms"`
		UnhealthyThreshold int   `json:"unhealthy_threshold"`
	} `json:"health_checker"`
	Raw json.RawMessage `json:"-"`
}

// standaloneProbe is the shape every cluster handler returns when no
// coordinator exists. `enabled` is a pointer so a body without the key
// (every real cluster payload except /cluster itself) is not mistaken
// for enabled:false.
type standaloneProbe struct {
	Enabled *bool  `json:"enabled"`
	Mode    string `json:"mode"`
	Reason  string `json:"reason"`
}

// checkStandalone returns a ClusterDisabledError when body is the
// standalone sentinel.
func checkStandalone(body []byte) error {
	var p standaloneProbe
	if err := json.Unmarshal(body, &p); err != nil {
		return nil
	}
	if p.Enabled != nil && !*p.Enabled && p.Mode == "standalone" {
		return &ClusterDisabledError{Reason: p.Reason, Raw: append(json.RawMessage(nil), body...)}
	}
	return nil
}

// getRaw performs an authenticated GET on an absolute API path and
// returns the body on 2xx. The read is bounded by limit.
func (c *Client) getRaw(ctx context.Context, path string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.Endpoint+path, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	c.setCrossDBHeaders(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, decodeWriteError(resp.StatusCode, body)
	}
	if t := strings.TrimSpace(string(body)); t == "" || t == "null" {
		return nil, fmt.Errorf("GET %s: empty response from server", path)
	}
	return body, nil
}

// ClusterStatus calls GET /api/v1/cluster.
func (c *Client) ClusterStatus(ctx context.Context) (*ClusterStatus, error) {
	body, err := c.getRaw(ctx, "/api/v1/cluster", 4<<20)
	if err != nil {
		return nil, err
	}
	if err := checkStandalone(body); err != nil {
		return nil, err
	}
	var out ClusterStatus
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode cluster status: %w", err)
	}
	if out.Nodes == nil {
		out.Nodes = []ClusterNode{}
	}
	if out.License != nil && out.License.Features == nil {
		out.License.Features = []string{}
	}
	out.Raw = body
	return &out, nil
}

// ClusterNodes calls GET /api/v1/cluster/nodes with optional role /
// state filters (validated client-side against the server's enums).
func (c *Client) ClusterNodes(ctx context.Context, role, state string) ([]ClusterNode, json.RawMessage, error) {
	if role != "" && !contains(ClusterNodeRoles, role) {
		return nil, nil, fmt.Errorf("invalid role %q (valid: %s)", role, strings.Join(ClusterNodeRoles, ", "))
	}
	if state != "" && !contains(ClusterNodeStates, state) {
		return nil, nil, fmt.Errorf("invalid state %q (valid: %s)", state, strings.Join(ClusterNodeStates, ", "))
	}
	q := url.Values{}
	if role != "" {
		q.Set("role", role)
	}
	if state != "" {
		q.Set("state", state)
	}
	path := "/api/v1/cluster/nodes"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	body, err := c.getRaw(ctx, path, 16<<20)
	if err != nil {
		return nil, nil, err
	}
	if err := checkStandalone(body); err != nil {
		return nil, nil, err
	}
	var out struct {
		Nodes []ClusterNode `json:"nodes"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, nil, fmt.Errorf("decode cluster nodes: %w", err)
	}
	if out.Nodes == nil {
		out.Nodes = []ClusterNode{}
	}
	return out.Nodes, body, nil
}

// ValidateNodeID mirrors the server's 1..256 length rule and additionally
// rejects ids that could be re-interpreted as a different path by a
// normalising proxy in front of Arc ("." / ".." segments) or that
// contain characters with no business in a node id. Everything else is
// percent-encoded on the wire by url.PathEscape.
func ValidateNodeID(id string) error {
	if id == "" || len(id) > 256 {
		return fmt.Errorf("node id must be 1-256 characters")
	}
	if id == "." || id == ".." {
		return fmt.Errorf("node id %q is not valid", id)
	}
	for _, r := range id {
		if r < 0x21 || r == 0x7f || strings.ContainsRune("/?#%", r) {
			return fmt.Errorf("node id must not contain whitespace, control characters, or any of / ? # %%")
		}
	}
	return nil
}

// ClusterNode calls GET /api/v1/cluster/nodes/:id.
func (c *Client) ClusterNode(ctx context.Context, id string) (*ClusterNode, json.RawMessage, error) {
	if err := ValidateNodeID(id); err != nil {
		return nil, nil, err
	}
	body, err := c.getRaw(ctx, "/api/v1/cluster/nodes/"+url.PathEscape(id), 1<<20)
	if err != nil {
		return nil, nil, err
	}
	if err := checkStandalone(body); err != nil {
		return nil, nil, err
	}
	var out ClusterNode
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, nil, fmt.Errorf("decode cluster node: %w", err)
	}
	return &out, body, nil
}

// ClusterLocalNode is GET /api/v1/cluster/local: the node the client is
// talking to, plus its role capabilities.
type ClusterLocalNode struct {
	ClusterNode
	Capabilities map[string]bool `json:"capabilities"`
	IsLocal      bool            `json:"is_local"`
}

// ClusterLocal calls GET /api/v1/cluster/local.
func (c *Client) ClusterLocal(ctx context.Context) (*ClusterLocalNode, json.RawMessage, error) {
	body, err := c.getRaw(ctx, "/api/v1/cluster/local", 1<<20)
	if err != nil {
		return nil, nil, err
	}
	if err := checkStandalone(body); err != nil {
		return nil, nil, err
	}
	var out ClusterLocalNode
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, nil, fmt.Errorf("decode local node: %w", err)
	}
	if out.Capabilities == nil {
		out.Capabilities = map[string]bool{}
	}
	return &out, body, nil
}

// ClusterHealth calls GET /api/v1/cluster/health.
func (c *Client) ClusterHealth(ctx context.Context) (*ClusterHealth, error) {
	body, err := c.getRaw(ctx, "/api/v1/cluster/health", 1<<20)
	if err != nil {
		return nil, err
	}
	if err := checkStandalone(body); err != nil {
		return nil, err
	}
	var out ClusterHealth
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode cluster health: %w", err)
	}
	out.Raw = body
	return &out, nil
}

// ErrNotLeader is wrapped into the error when the server refuses a
// node removal because it is not the raft leader.
var ErrNotLeader = errors.New("this node is not the raft leader")

// RemoveClusterNode calls DELETE /api/v1/cluster/nodes/:id (admin).
// The server refuses self-removal (400) and, from a follower, returns
// 500 "not the leader"; the latter is surfaced as ErrNotLeader so the
// caller can point at the leader. With raft off the server returns 200
// even for unknown ids.
func (c *Client) RemoveClusterNode(ctx context.Context, id string) error {
	if err := ValidateNodeID(id); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.cfg.Endpoint+"/api/v1/cluster/nodes/"+url.PathEscape(id), nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	c.setCrossDBHeaders(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("remove node: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		herr := decodeWriteError(resp.StatusCode, body)
		var he *HTTPError
		if errors.As(herr, &he) && strings.Contains(he.Message, "not the leader") {
			return fmt.Errorf("%w (server said: %s)", ErrNotLeader, he.Message)
		}
		return herr
	}
	return checkStandalone(body)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
