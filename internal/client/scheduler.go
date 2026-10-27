package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// SchedulerBlock is one of the two entries of GET /api/v1/schedulers/.
// The server uses two different shapes: a running scheduler reports
// `running` plus its details; a scheduler that could not start reports
// `{"enabled": false, "reason": "..."}` (the OSS / unlicensed default).
// Enabled is a pointer so the two can be told apart.
type SchedulerBlock struct {
	Enabled      *bool  `json:"enabled,omitempty"`
	Reason       string `json:"reason,omitempty"`
	Running      bool   `json:"running"`
	LicenseValid bool   `json:"license_valid"`
	// CQ scheduler only.
	JobCount int     `json:"job_count"`
	Jobs     []CQJob `json:"jobs"`
	// Retention scheduler only.
	Schedule string `json:"schedule,omitempty"`
	NextRun  string `json:"next_run,omitempty"`
	CanRun   *bool  `json:"can_run,omitempty"`
	GateRole string `json:"gate_role,omitempty"`
}

// NotRunning reports whether the block is the "could not start" shape.
func (b SchedulerBlock) NotRunning() bool {
	return b.Enabled != nil && !*b.Enabled
}

// CQJob is one scheduled continuous query in the CQ scheduler block.
type CQJob struct {
	CQID     int64  `json:"cq_id"`
	CQName   string `json:"cq_name"`
	Interval string `json:"interval"`
}

// SchedulersStatus is GET /api/v1/schedulers/ (always registered; any token).
type SchedulersStatus struct {
	CQ        SchedulerBlock  `json:"cq_scheduler"`
	Retention SchedulerBlock  `json:"retention_scheduler"`
	Raw       json.RawMessage `json:"-"`
}

// SchedulerStatus calls GET /api/v1/schedulers/.
func (c *Client) SchedulerStatus(ctx context.Context) (*SchedulersStatus, error) {
	body, err := c.getRaw(ctx, "/api/v1/schedulers/", 1<<20)
	if err != nil {
		return nil, err
	}
	var out SchedulersStatus
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode scheduler status: %w", err)
	}
	if out.CQ.Jobs == nil {
		out.CQ.Jobs = []CQJob{}
	}
	out.Raw = body
	return &out, nil
}

// SchedulerActionResult is the response from a scheduler administration
// endpoint. JobCount is present only for CQ reloads.
type SchedulerActionResult struct {
	Message  string          `json:"message"`
	JobCount *int            `json:"job_count,omitempty"`
	Raw      json.RawMessage `json:"-"`
}

// ReloadCQScheduler reloads every active continuous query into the scheduler.
func (c *Client) ReloadCQScheduler(ctx context.Context) (*SchedulerActionResult, error) {
	return c.schedulerAction(ctx, "/api/v1/schedulers/cq/reload", "reload CQ scheduler")
}

// TriggerRetentionScheduler runs every active retention policy immediately.
func (c *Client) TriggerRetentionScheduler(ctx context.Context) (*SchedulerActionResult, error) {
	return c.schedulerAction(ctx, "/api/v1/schedulers/retention/trigger", "trigger retention scheduler")
}

func (c *Client) schedulerAction(ctx context.Context, path, action string) (*SchedulerActionResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.Endpoint+path, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	c.setCrossDBHeaders(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, fmt.Errorf("read %s response: %w", action, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, decodeWriteError(resp.StatusCode, body)
	}
	var out SchedulerActionResult
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode %s response: %w", action, err)
	}
	out.Raw = body
	return &out, nil
}
