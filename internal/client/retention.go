package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

const (
	retentionFeature   = "retention policies"
	retentionConfigKey = "retention.enabled"
	retentionBase      = "/api/v1/retention/"
)

// RetentionPolicy mirrors arc/internal/api/retention.go#RetentionPolicy.
// Age is expressed as whole days: files whose newest row is older than
// now − (retention_days + buffer_days) are deleted. Timestamp fields are
// the server's strings, RFC3339Nano UTC (database/sql renders the
// driver's time.Time into *string that way).
type RetentionPolicy struct {
	ID                  int64   `json:"id"`
	Name                string  `json:"name"`
	Database            string  `json:"database"`
	Measurement         *string `json:"measurement"`
	RetentionDays       int     `json:"retention_days"`
	BufferDays          int     `json:"buffer_days"`
	IsActive            bool    `json:"is_active"`
	LastExecutionTime   *string `json:"last_execution_time"`
	LastExecutionStatus *string `json:"last_execution_status"`
	LastDeletedCount    *int64  `json:"last_deleted_count"`
	CreatedAt           string  `json:"created_at"`
	UpdatedAt           string  `json:"updated_at"`
}

// RetentionPolicyRequest is the POST/PUT body. The server's PUT is a
// full replace, so callers must always send every field.
type RetentionPolicyRequest struct {
	Name          string  `json:"name"`
	Database      string  `json:"database"`
	Measurement   *string `json:"measurement"`
	RetentionDays int     `json:"retention_days"`
	BufferDays    int     `json:"buffer_days"`
	IsActive      bool    `json:"is_active"`
}

// RequestFromPolicy builds the full PUT body for an existing policy.
func RequestFromPolicy(p RetentionPolicy) RetentionPolicyRequest {
	return RetentionPolicyRequest{
		Name: p.Name, Database: p.Database, Measurement: p.Measurement,
		RetentionDays: p.RetentionDays, BufferDays: p.BufferDays, IsActive: p.IsActive,
	}
}

// ValidateRetentionPolicyRequest mirrors the server's create checks so
// they fire before HTTP, and applies them to PUT bodies too (the server
// skips most of them on PUT).
func ValidateRetentionPolicyRequest(r RetentionPolicyRequest) error {
	if strings.TrimSpace(r.Name) == "" {
		return fmt.Errorf("name is required")
	}
	if strings.TrimSpace(r.Database) == "" {
		return fmt.Errorf("database is required")
	}
	if err := ValidateDatabaseName(r.Database); err != nil {
		return err
	}
	if r.Measurement != nil && *r.Measurement != "" {
		if err := ValidateMeasurementName(*r.Measurement); err != nil {
			return err
		}
	}
	if r.RetentionDays <= 0 {
		return fmt.Errorf("retention-days must be greater than 0")
	}
	if r.BufferDays < 0 {
		return fmt.Errorf("buffer-days must not be negative")
	}
	if r.RetentionDays <= r.BufferDays {
		return fmt.Errorf("retention-days (%d) must be greater than buffer-days (%d)", r.RetentionDays, r.BufferDays)
	}
	return nil
}

// RetentionExecuteResult is the 200 body of POST /:id/execute.
type RetentionExecuteResult struct {
	PolicyID             int64           `json:"policy_id"`
	PolicyName           string          `json:"policy_name"`
	DeletedCount         int64           `json:"deleted_count"`
	FilesDeleted         int             `json:"files_deleted"`
	ExecutionTimeMs      float64         `json:"execution_time_ms"`
	DryRun               bool            `json:"dry_run"`
	CutoffDate           string          `json:"cutoff_date"`
	AffectedMeasurements []string        `json:"affected_measurements"`
	Raw                  json.RawMessage `json:"-"`
}

// RetentionExecution is one history row.
type RetentionExecution struct {
	ID                  int64   `json:"id"`
	PolicyID            int64   `json:"policy_id"`
	ExecutionTime       string  `json:"execution_time"`
	Status              string  `json:"status"`
	DeletedCount        int64   `json:"deleted_count"`
	CutoffDate          *string `json:"cutoff_date"`
	ExecutionDurationMs float64 `json:"execution_duration_ms"`
	ErrorMessage        *string `json:"error_message"`
}

func retentionPath(id int64) string {
	return retentionBase + strconv.FormatInt(id, 10)
}

// ListRetentionPolicies calls GET /api/v1/retention/. The server sends a
// bare array, `null` when empty; normalised to a non-nil slice. The raw
// body is returned for JSON output (normalised to `[]` when null).
func (c *Client) ListRetentionPolicies(ctx context.Context) ([]RetentionPolicy, json.RawMessage, error) {
	var out []RetentionPolicy
	body, err := c.featureJSON(ctx, retentionFeature, retentionConfigKey, http.MethodGet, retentionBase, nil, 16<<20, &out)
	if err != nil {
		return nil, nil, err
	}
	if out == nil {
		out = []RetentionPolicy{}
		body = []byte("[]")
	}
	return out, body, nil
}

// GetRetentionPolicy calls GET /api/v1/retention/:id.
func (c *Client) GetRetentionPolicy(ctx context.Context, id int64) (*RetentionPolicy, json.RawMessage, error) {
	var out RetentionPolicy
	body, err := c.featureJSON(ctx, retentionFeature, retentionConfigKey, http.MethodGet, retentionPath(id), nil, 1<<20, &out)
	if err != nil {
		return nil, nil, err
	}
	return &out, body, nil
}

// CreateRetentionPolicy calls POST /api/v1/retention/ (admin) and
// returns the created policy (HTTP 201). A duplicate name is a 400
// from the server, surfaced verbatim.
func (c *Client) CreateRetentionPolicy(ctx context.Context, req RetentionPolicyRequest) (*RetentionPolicy, json.RawMessage, error) {
	if err := ValidateRetentionPolicyRequest(req); err != nil {
		return nil, nil, err
	}
	var out RetentionPolicy
	body, err := c.featureJSON(ctx, retentionFeature, retentionConfigKey, http.MethodPost, retentionBase, req, 1<<20, &out)
	if err != nil {
		return nil, nil, err
	}
	return &out, body, nil
}

// UpdateRetentionPolicy calls PUT /api/v1/retention/:id (admin) with a
// FULL policy body; the server replaces every column unconditionally.
func (c *Client) UpdateRetentionPolicy(ctx context.Context, id int64, req RetentionPolicyRequest) (*RetentionPolicy, json.RawMessage, error) {
	if err := ValidateRetentionPolicyRequest(req); err != nil {
		return nil, nil, err
	}
	var out RetentionPolicy
	body, err := c.featureJSON(ctx, retentionFeature, retentionConfigKey, http.MethodPut, retentionPath(id), req, 1<<20, &out)
	if err != nil {
		return nil, nil, err
	}
	return &out, body, nil
}

// DeleteRetentionPolicy calls DELETE /api/v1/retention/:id (admin). The
// server also drops the policy's execution history.
func (c *Client) DeleteRetentionPolicy(ctx context.Context, id int64) error {
	_, err := c.featureJSON(ctx, retentionFeature, retentionConfigKey, http.MethodDelete, retentionPath(id), nil, 64<<10, nil)
	return err
}

// ExecuteRetentionPolicy calls POST /api/v1/retention/:id/execute
// (admin). The call is synchronous: the server scans and deletes before
// answering. dryRun=false sends the server-required {"confirm":true};
// the command layer owns the human confirmation.
func (c *Client) ExecuteRetentionPolicy(ctx context.Context, id int64, dryRun bool) (*RetentionExecuteResult, error) {
	req := struct {
		DryRun  bool `json:"dry_run"`
		Confirm bool `json:"confirm"`
	}{DryRun: dryRun, Confirm: !dryRun}
	var out RetentionExecuteResult
	body, err := c.featureJSON(ctx, retentionFeature, retentionConfigKey, http.MethodPost, retentionPath(id)+"/execute", req, 4<<20, &out)
	if err != nil {
		return nil, err
	}
	if out.AffectedMeasurements == nil {
		out.AffectedMeasurements = []string{}
	}
	out.Raw = body
	return &out, nil
}

// ListRetentionExecutions calls GET /api/v1/retention/:id/executions
// (newest first). `executions: null` is normalised to an empty slice.
func (c *Client) ListRetentionExecutions(ctx context.Context, id int64, limit int) ([]RetentionExecution, json.RawMessage, error) {
	if limit < 1 {
		return nil, nil, fmt.Errorf("limit must be >= 1")
	}
	var out struct {
		Executions []RetentionExecution `json:"executions"`
	}
	body, err := c.featureJSON(ctx, retentionFeature, retentionConfigKey, http.MethodGet, retentionPath(id)+"/executions?limit="+strconv.Itoa(limit), nil, 4<<20, &out)
	if err != nil {
		return nil, nil, err
	}
	if out.Executions == nil {
		out.Executions = []RetentionExecution{}
	}
	return out.Executions, body, nil
}
