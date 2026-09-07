package client

import (
	"context"
	"encoding/json"
	"fmt"
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
