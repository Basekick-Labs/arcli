package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// CompactionDisabledError is returned when the compaction routes are not
// registered on the server (compaction.enabled=false). Arc's router
// answers such paths with HTTP 404 {"error":"Cannot GET /api/v1/..."}.
type CompactionDisabledError struct{}

func (e *CompactionDisabledError) Error() string {
	return "compaction is disabled on this server (compaction.enabled=false)"
}

// CompactionRunningError is the 409 from POST /api/v1/compaction/trigger.
type CompactionRunningError struct {
	CycleID int64
}

func (e *CompactionRunningError) Error() string {
	return fmt.Sprintf("a compaction cycle is already running (cycle %d); wait for it to finish", e.CycleID)
}

// CompactionTiers is the set of tiers Arc implements. The server does
// NOT validate tier names on trigger (unknown ones are echoed back and
// ignored), so the client does.
var CompactionTiers = []string{"hourly", "daily"}

// databaseNameRe mirrors the server's isValidDatabaseName
// (arc/internal/api/databases.go): letter first, then [A-Za-z0-9_-],
// at most 64 characters.
var databaseNameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)

// ValidateDatabaseName applies the server's database-name grammar
// (letter first, then letters/digits/_/-, max 64) client-side. Routes
// that store a database name without checking it (retention, CQ) rely
// on this so a typo does not create a policy that scans nothing.
func ValidateDatabaseName(name string) error {
	if !databaseNameRe.MatchString(name) {
		return fmt.Errorf("invalid database name %q (letter first, then letters, digits, _ or -, max 64)", name)
	}
	return nil
}

// SchedulerStatus is one entry of /api/v1/compaction/status "schedulers".
// NextRun is only present while the scheduler is running; RoleGated and
// GateRole only in cluster mode.
type SchedulerStatus struct {
	Enabled   bool       `json:"enabled"`
	Running   bool       `json:"running"`
	Schedule  string     `json:"schedule"`
	NextRun   *time.Time `json:"next_run,omitempty"`
	RoleGated *bool      `json:"role_gated,omitempty"`
	GateRole  string     `json:"gate_role,omitempty"`
}

// CompactionStatus is GET /api/v1/compaction/status. ActiveJobs is a
// pointer because the server currently always sends null for it.
type CompactionStatus struct {
	Manager struct {
		ActiveJobs     *int `json:"active_jobs"`
		TotalCompleted int  `json:"total_completed"`
		TotalFailed    int  `json:"total_failed"`
	} `json:"manager"`
	Schedulers map[string]SchedulerStatus `json:"schedulers"`
	Raw        json.RawMessage            `json:"-"`
}

// TierStats is one entry of /api/v1/compaction/stats "tiers".
type TierStats struct {
	Tier                string `json:"tier"`
	Enabled             bool   `json:"enabled"`
	MinAgeHours         int    `json:"min_age_hours"`
	MinFiles            int    `json:"min_files"`
	TotalCompactions    int64  `json:"total_compactions"`
	TotalFilesCompacted int64  `json:"total_files_compacted"`
	TotalBytesSaved     int64  `json:"total_bytes_saved"`
}

// CompactionJob is one history entry. Only the first four fields are
// guaranteed; the rest are absent when the job produced no result.
// CompressionRatio is the fraction of bytes saved (1 - after/before),
// despite its name. There is no timestamp on the wire.
type CompactionJob struct {
	Database         string   `json:"database"`
	Measurement      string   `json:"measurement"`
	PartitionPath    string   `json:"partition_path"`
	Tier             string   `json:"tier"`
	FilesCompacted   *int     `json:"files_compacted,omitempty"`
	BytesBefore      *int64   `json:"bytes_before,omitempty"`
	BytesAfter       *int64   `json:"bytes_after,omitempty"`
	Success          *bool    `json:"success,omitempty"`
	CompressionRatio *float64 `json:"compression_ratio,omitempty"`
	Error            string   `json:"error,omitempty"`
}

// CompactionStats is GET /api/v1/compaction/stats. Tiers is nil when
// the server omits the key (no tiers enabled).
type CompactionStats struct {
	TotalJobsCompleted    int64           `json:"total_jobs_completed"`
	TotalJobsFailed       int64           `json:"total_jobs_failed"`
	TotalFilesCompacted   int64           `json:"total_files_compacted"`
	TotalBytesSaved       int64           `json:"total_bytes_saved"`
	TotalManifestsRecover int64           `json:"total_manifests_recover"`
	CycleRunning          bool            `json:"cycle_running"`
	CurrentCycleID        int64           `json:"current_cycle_id"`
	RecentJobs            []CompactionJob `json:"recent_jobs"`
	Tiers                 []TierStats     `json:"tiers"`
	Raw                   json.RawMessage `json:"-"`
}

// CompactionCandidate is one entry of /api/v1/compaction/candidates.
type CompactionCandidate struct {
	Database      string `json:"database"`
	Measurement   string `json:"measurement"`
	PartitionPath string `json:"partition_path"`
	FileCount     int    `json:"file_count"`
	Tier          string `json:"tier"`
}

// CompactionHistory is GET /api/v1/compaction/history.
type CompactionHistory struct {
	TotalJobs  int64           `json:"total_jobs"`
	RecentJobs []CompactionJob `json:"recent_jobs"`
	Raw        json.RawMessage `json:"-"`
}

// TriggerResult is the 200 body of POST /api/v1/compaction/trigger.
// CycleID is the server's prediction (current+1) made before the
// asynchronous cycle starts, not a confirmed job id.
type TriggerResult struct {
	Message  string          `json:"message"`
	Status   string          `json:"status"`
	Tiers    []string        `json:"tiers"`
	CycleID  int64           `json:"cycle_id"`
	Database string          `json:"database,omitempty"`
	Raw      json.RawMessage `json:"-"`
}

// isRouteMissing reports whether err is Fiber's "route not registered"
// 404, whose message is "Cannot <METHOD> <path>". A 404 with any other
// message (e.g. "Node not found") is a real not-found from a handler.
func isRouteMissing(err error) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.Status == http.StatusNotFound && strings.HasPrefix(he.Message, "Cannot ")
}

// classifyRouteMissing turns Fiber's "Cannot GET ..." 404 into a
// CompactionDisabledError, but only after the strict /health check
// proves the endpoint really is an Arc server. A wrong base path or a
// non-Arc service produces the same 404 shape, and reporting that as
// "compaction.enabled=false" would send the operator to the wrong
// place. Any other error is returned unchanged.
func (c *Client) classifyRouteMissing(ctx context.Context, err error) error {
	if !isRouteMissing(err) {
		return err
	}
	if _, herr := c.Health(ctx); herr != nil {
		return fmt.Errorf("%w (and %s does not answer like an Arc server: %v; check the endpoint path)", err, redactedEndpoint(c.cfg.Endpoint), herr)
	}
	return &CompactionDisabledError{}
}

func (c *Client) compactionGet(ctx context.Context, path string, limit int64) ([]byte, error) {
	body, err := c.getRaw(ctx, path, limit)
	if err != nil {
		return nil, c.classifyRouteMissing(ctx, err)
	}
	return body, nil
}

// CompactionStatus calls GET /api/v1/compaction/status.
func (c *Client) CompactionStatus(ctx context.Context) (*CompactionStatus, error) {
	body, err := c.compactionGet(ctx, "/api/v1/compaction/status", 1<<20)
	if err != nil {
		return nil, err
	}
	var out CompactionStatus
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode compaction status: %w", err)
	}
	if out.Schedulers == nil {
		out.Schedulers = map[string]SchedulerStatus{}
	}
	out.Raw = body
	return &out, nil
}

// CompactionStats calls GET /api/v1/compaction/stats.
func (c *Client) CompactionStats(ctx context.Context) (*CompactionStats, error) {
	body, err := c.compactionGet(ctx, "/api/v1/compaction/stats", 4<<20)
	if err != nil {
		return nil, err
	}
	var out CompactionStats
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode compaction stats: %w", err)
	}
	if out.RecentJobs == nil {
		out.RecentJobs = []CompactionJob{}
	}
	out.Raw = body
	return &out, nil
}

// CompactionCandidates calls GET /api/v1/compaction/candidates. The
// server performs a live storage scan with a 30s internal timeout, so
// callers should allow at least that much.
func (c *Client) CompactionCandidates(ctx context.Context) ([]CompactionCandidate, json.RawMessage, error) {
	body, err := c.compactionGet(ctx, "/api/v1/compaction/candidates", 16<<20)
	if err != nil {
		return nil, nil, err
	}
	var out struct {
		Candidates []CompactionCandidate `json:"candidates"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, nil, fmt.Errorf("decode compaction candidates: %w", err)
	}
	if out.Candidates == nil {
		out.Candidates = []CompactionCandidate{}
	}
	return out.Candidates, body, nil
}

// CompactionHistory calls GET /api/v1/compaction/history?limit=N. The
// server keeps only the 10 most recent jobs regardless of limit.
func (c *Client) CompactionHistory(ctx context.Context, limit int) (*CompactionHistory, error) {
	if limit < 1 {
		return nil, fmt.Errorf("limit must be >= 1")
	}
	body, err := c.compactionGet(ctx, "/api/v1/compaction/history?limit="+strconv.Itoa(limit), 4<<20)
	if err != nil {
		return nil, err
	}
	var out CompactionHistory
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode compaction history: %w", err)
	}
	if out.RecentJobs == nil {
		out.RecentJobs = []CompactionJob{}
	}
	out.Raw = body
	return &out, nil
}

// TriggerCompaction calls POST /api/v1/compaction/trigger (admin). Tiers
// and database go in the query string; the server ignores any body.
// Empty tiers means "all enabled tiers"; empty database means all.
func (c *Client) TriggerCompaction(ctx context.Context, tiers []string, database string) (*TriggerResult, error) {
	for _, t := range tiers {
		if !contains(CompactionTiers, t) {
			return nil, fmt.Errorf("invalid tier %q (valid: %s)", t, strings.Join(CompactionTiers, ", "))
		}
	}
	if database != "" && !databaseNameRe.MatchString(database) {
		return nil, fmt.Errorf("invalid database name %q (letter first, then letters, digits, _ or -, max 64)", database)
	}
	q := url.Values{}
	if len(tiers) > 0 {
		q.Set("tier", strings.Join(tiers, ","))
	}
	if database != "" {
		q.Set("database", database)
	}
	path := "/api/v1/compaction/trigger"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.Endpoint+path, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	c.setCrossDBHeaders(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("trigger compaction: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode == http.StatusConflict {
		var conflict struct {
			CycleID int64 `json:"cycle_id"`
		}
		_ = json.Unmarshal(body, &conflict)
		return nil, &CompactionRunningError{CycleID: conflict.CycleID}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, c.classifyRouteMissing(ctx, decodeWriteError(resp.StatusCode, body))
	}
	var out TriggerResult
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode trigger response: %w", err)
	}
	if out.Tiers == nil {
		out.Tiers = []string{}
	}
	out.Raw = body
	return &out, nil
}
