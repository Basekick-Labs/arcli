package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	cqFeature   = "continuous queries"
	cqConfigKey = "continuous_query.enabled"
	cqBase      = "/api/v1/continuous_queries/"

	// CQMinInterval is the scheduler's floor; anything shorter is clamped
	// to it server-side (arc/internal/scheduler/cq_scheduler.go).
	CQMinInterval = 10 * time.Second
	// CQMaxQueryLen mirrors ValidateSQLRequest's ceiling.
	CQMaxQueryLen = 10000
)

// measurementNameRe mirrors arc/internal/api/lineprotocol.go#isValidMeasurementName.
var measurementNameRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]*$`)

// tagColumnRe mirrors arc/internal/api/continuous_query.go#validateTagColumns.
var tagColumnRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// ValidateMeasurementName applies the server's measurement grammar
// (letter first, then letters/digits/_/-, 1..128).
func ValidateMeasurementName(name string) error {
	if name == "" || len(name) > 128 || !measurementNameRe.MatchString(name) {
		return fmt.Errorf("invalid measurement name %q (letter first, then letters, digits, _ or -, max 128)", name)
	}
	return nil
}

// HasSourceReference reports whether the query contains the
// `FROM <database>.<source>` form the server rewrites to the underlying
// Parquet files (arc/internal/api/continuous_query.go). Any other way
// of naming the source reaches DuckDB unchanged and fails at execution.
func HasSourceReference(query, database, source string) bool {
	// Compile, not MustCompile: QuoteMeta does not repair invalid UTF-8,
	// and a warning helper must never panic. On any compile error the
	// warning is suppressed (return true) rather than risk a crash.
	// The terminator is explicit because \b does not fire after "-",
	// which is legal in measurement names.
	re, err := regexp.Compile(`(?i)\bFROM\s+` + regexp.QuoteMeta(database) + `\.` + regexp.QuoteMeta(source) + `(?:[^A-Za-z0-9_-]|$)`)
	if err != nil {
		return true
	}
	return re.MatchString(query)
}

// ContinuousQuery mirrors arc/internal/api/continuous_query.go#ContinuousQuery.
type ContinuousQuery struct {
	ID                     int64    `json:"id"`
	Name                   string   `json:"name"`
	Description            *string  `json:"description"`
	Database               string   `json:"database"`
	SourceMeasurement      string   `json:"source_measurement"`
	DestinationMeasurement string   `json:"destination_measurement"`
	Query                  string   `json:"query"`
	Interval               string   `json:"interval"`
	TagColumns             []string `json:"tag_columns"`
	RetentionDays          *int     `json:"retention_days"`
	DeleteSourceAfterDays  *int     `json:"delete_source_after_days"`
	IsActive               bool     `json:"is_active"`
	LastExecutionTime      *string  `json:"last_execution_time"`
	LastExecutionStatus    *string  `json:"last_execution_status"`
	LastProcessedTime      *string  `json:"last_processed_time"`
	LastRecordsWritten     *int64   `json:"last_records_written"`
	CreatedAt              string   `json:"created_at"`
	UpdatedAt              string   `json:"updated_at"`
}

// ContinuousQueryRequest is the POST/PUT body (PUT is a full replace).
type ContinuousQueryRequest struct {
	Name                   string   `json:"name"`
	Description            *string  `json:"description"`
	Database               string   `json:"database"`
	SourceMeasurement      string   `json:"source_measurement"`
	DestinationMeasurement string   `json:"destination_measurement"`
	Query                  string   `json:"query"`
	Interval               string   `json:"interval"`
	TagColumns             []string `json:"tag_columns"`
	RetentionDays          *int     `json:"retention_days"`
	DeleteSourceAfterDays  *int     `json:"delete_source_after_days"`
	IsActive               bool     `json:"is_active"`
}

// RequestFromCQ builds the full PUT body for an existing CQ.
func RequestFromCQ(q ContinuousQuery) ContinuousQueryRequest {
	return ContinuousQueryRequest{
		Name: q.Name, Description: q.Description, Database: q.Database,
		SourceMeasurement: q.SourceMeasurement, DestinationMeasurement: q.DestinationMeasurement,
		Query: q.Query, Interval: q.Interval, TagColumns: q.TagColumns,
		RetentionDays: q.RetentionDays, DeleteSourceAfterDays: q.DeleteSourceAfterDays, IsActive: q.IsActive,
	}
}

// ParseCQInterval validates the scheduler's interval grammar (a Go
// duration) and floor. The server does NOT validate the interval at
// create time — an unparseable value is stored and only fails inside
// the scheduler with a log line — so this is the only guard.
func ParseCQInterval(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid interval %q (use a Go duration such as 30s, 5m, 1h)", s)
	}
	if d <= 0 {
		return 0, fmt.Errorf("interval must be positive (got %s)", s)
	}
	if d < CQMinInterval {
		return 0, fmt.Errorf("interval %s is below %s; the server's scheduler clamps anything shorter to %s", s, CQMinInterval, CQMinInterval)
	}
	return d, nil
}

// ValidateCQRequest applies the server's create rules (and the ones the
// server omits) before any HTTP. SQL semantics are left to the server.
func ValidateCQRequest(r ContinuousQueryRequest) error {
	if strings.TrimSpace(r.Name) == "" {
		return fmt.Errorf("name is required")
	}
	if strings.TrimSpace(r.Database) == "" {
		return fmt.Errorf("database is required")
	}
	if err := ValidateDatabaseName(r.Database); err != nil {
		return err
	}
	if strings.TrimSpace(r.SourceMeasurement) == "" {
		return fmt.Errorf("source measurement is required")
	}
	if err := ValidateMeasurementName(r.SourceMeasurement); err != nil {
		return fmt.Errorf("source: %w", err)
	}
	if r.DestinationMeasurement == "" {
		return fmt.Errorf("destination measurement is required")
	}
	if err := ValidateMeasurementName(r.DestinationMeasurement); err != nil {
		return fmt.Errorf("destination: %w", err)
	}
	if strings.TrimSpace(r.Query) == "" {
		return fmt.Errorf("query is required")
	}
	if len(r.Query) > CQMaxQueryLen {
		return fmt.Errorf("query is %d characters; the server accepts at most %d", len(r.Query), CQMaxQueryLen)
	}
	if !strings.Contains(r.Query, "{start_time}") || !strings.Contains(r.Query, "{end_time}") {
		return fmt.Errorf("query must contain both {start_time} and {end_time} placeholders")
	}
	if r.Interval == "" {
		return fmt.Errorf("interval is required")
	}
	if _, err := ParseCQInterval(r.Interval); err != nil {
		return err
	}
	for _, t := range r.TagColumns {
		if t == "" {
			return fmt.Errorf("tag column names must not be empty")
		}
		if strings.EqualFold(t, "time") {
			return fmt.Errorf("\"time\" is reserved and cannot be a tag column")
		}
		if !tagColumnRe.MatchString(t) {
			return fmt.Errorf("invalid tag column %q: only letters, digits, _ and - are allowed", t)
		}
	}
	if r.RetentionDays != nil && *r.RetentionDays < 0 {
		return fmt.Errorf("retention-days must not be negative")
	}
	if r.DeleteSourceAfterDays != nil && *r.DeleteSourceAfterDays < 0 {
		return fmt.Errorf("delete-source-after-days must not be negative")
	}
	return nil
}

// CQExecuteOptions is the POST /:id/execute body. Nil times use the
// server defaults (start = last_processed_time or now−1h, end = now).
type CQExecuteOptions struct {
	Start  *time.Time
	End    *time.Time
	DryRun bool
}

// CQExecuteResult is the 200 body of POST /:id/execute. ExecutedQuery is
// only populated for dry runs. RecordsRead is always null on the wire.
type CQExecuteResult struct {
	QueryID                int64           `json:"query_id"`
	QueryName              string          `json:"query_name"`
	ExecutionID            string          `json:"execution_id"`
	Status                 string          `json:"status"`
	StartTime              string          `json:"start_time"`
	EndTime                string          `json:"end_time"`
	RecordsWritten         int64           `json:"records_written"`
	ExecutionTimeSeconds   float64         `json:"execution_time_seconds"`
	DestinationMeasurement string          `json:"destination_measurement"`
	DryRun                 bool            `json:"dry_run"`
	ExecutedAt             string          `json:"executed_at"`
	ExecutedQuery          string          `json:"executed_query,omitempty"`
	Raw                    json.RawMessage `json:"-"`
}

// CQExecution is one history row.
type CQExecution struct {
	ID                       int64   `json:"id"`
	QueryID                  int64   `json:"query_id"`
	ExecutionID              string  `json:"execution_id"`
	ExecutionTime            string  `json:"execution_time"`
	Status                   string  `json:"status"`
	StartTime                string  `json:"start_time"`
	EndTime                  string  `json:"end_time"`
	RecordsWritten           int64   `json:"records_written"`
	ExecutionDurationSeconds float64 `json:"execution_duration_seconds"`
	ErrorMessage             *string `json:"error_message"`
}

func cqPath(id int64) string {
	return cqBase + strconv.FormatInt(id, 10)
}

func normaliseCQ(q *ContinuousQuery) {
	if q.TagColumns == nil {
		q.TagColumns = []string{}
	}
}

// ListContinuousQueries calls GET /api/v1/continuous_queries/ (admin).
// active: nil = no filter; the server's filter only recognises the
// literal "true", so both values are sent explicitly.
func (c *Client) ListContinuousQueries(ctx context.Context, database string, active *bool) ([]ContinuousQuery, json.RawMessage, error) {
	q := url.Values{}
	if database != "" {
		q.Set("database", database)
	}
	if active != nil {
		q.Set("is_active", strconv.FormatBool(*active))
	}
	path := cqBase
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out []ContinuousQuery
	body, err := c.featureJSON(ctx, cqFeature, cqConfigKey, http.MethodGet, path, nil, 16<<20, &out)
	if err != nil {
		return nil, nil, err
	}
	if out == nil {
		out = []ContinuousQuery{}
		body = []byte("[]")
	}
	for i := range out {
		normaliseCQ(&out[i])
	}
	return out, body, nil
}

// GetContinuousQuery calls GET /api/v1/continuous_queries/:id (admin).
func (c *Client) GetContinuousQuery(ctx context.Context, id int64) (*ContinuousQuery, json.RawMessage, error) {
	var out ContinuousQuery
	body, err := c.featureJSON(ctx, cqFeature, cqConfigKey, http.MethodGet, cqPath(id), nil, 1<<20, &out)
	if err != nil {
		return nil, nil, err
	}
	normaliseCQ(&out)
	return &out, body, nil
}

// CreateContinuousQuery calls POST /api/v1/continuous_queries/ (admin).
func (c *Client) CreateContinuousQuery(ctx context.Context, req ContinuousQueryRequest) (*ContinuousQuery, json.RawMessage, error) {
	if err := ValidateCQRequest(req); err != nil {
		return nil, nil, err
	}
	if req.TagColumns == nil {
		req.TagColumns = []string{}
	}
	var out ContinuousQuery
	body, err := c.featureJSON(ctx, cqFeature, cqConfigKey, http.MethodPost, cqBase, req, 1<<20, &out)
	if err != nil {
		return nil, nil, err
	}
	normaliseCQ(&out)
	return &out, body, nil
}

// UpdateContinuousQuery calls PUT /api/v1/continuous_queries/:id (admin)
// with a FULL body; the server replaces every column and re-checks only
// a subset of the create rules, so the full set is applied here.
func (c *Client) UpdateContinuousQuery(ctx context.Context, id int64, req ContinuousQueryRequest) (*ContinuousQuery, json.RawMessage, error) {
	if err := ValidateCQRequest(req); err != nil {
		return nil, nil, err
	}
	if req.TagColumns == nil {
		req.TagColumns = []string{}
	}
	var out ContinuousQuery
	body, err := c.featureJSON(ctx, cqFeature, cqConfigKey, http.MethodPut, cqPath(id), req, 1<<20, &out)
	if err != nil {
		return nil, nil, err
	}
	normaliseCQ(&out)
	return &out, body, nil
}

// DeleteContinuousQuery calls DELETE /api/v1/continuous_queries/:id (admin).
func (c *Client) DeleteContinuousQuery(ctx context.Context, id int64) error {
	_, err := c.featureJSON(ctx, cqFeature, cqConfigKey, http.MethodDelete, cqPath(id), nil, 64<<10, nil)
	return err
}

// ExecuteContinuousQuery calls POST /api/v1/continuous_queries/:id/execute
// (admin). Synchronous; the server caps a run at 10 minutes.
func (c *Client) ExecuteContinuousQuery(ctx context.Context, id int64, opts CQExecuteOptions) (*CQExecuteResult, error) {
	if opts.Start != nil && opts.End != nil && !opts.Start.Before(*opts.End) {
		return nil, fmt.Errorf("start must be before end")
	}
	req := struct {
		StartTime *string `json:"start_time,omitempty"`
		EndTime   *string `json:"end_time,omitempty"`
		DryRun    bool    `json:"dry_run"`
	}{DryRun: opts.DryRun}
	if opts.Start != nil {
		s := opts.Start.UTC().Format(time.RFC3339)
		req.StartTime = &s
	}
	if opts.End != nil {
		e := opts.End.UTC().Format(time.RFC3339)
		req.EndTime = &e
	}
	var out CQExecuteResult
	body, err := c.featureJSON(ctx, cqFeature, cqConfigKey, http.MethodPost, cqPath(id)+"/execute", req, 4<<20, &out)
	if err != nil {
		return nil, err
	}
	out.Raw = body
	return &out, nil
}

// ListCQExecutions calls GET /api/v1/continuous_queries/:id/executions
// (admin, newest first). `executions: null` → empty slice.
func (c *Client) ListCQExecutions(ctx context.Context, id int64, limit int) ([]CQExecution, json.RawMessage, error) {
	if limit < 1 {
		return nil, nil, fmt.Errorf("limit must be >= 1")
	}
	var out struct {
		Executions []CQExecution `json:"executions"`
	}
	body, err := c.featureJSON(ctx, cqFeature, cqConfigKey, http.MethodGet, cqPath(id)+"/executions?limit="+strconv.Itoa(limit), nil, 4<<20, &out)
	if err != nil {
		return nil, nil, err
	}
	if out.Executions == nil {
		out.Executions = []CQExecution{}
	}
	return out.Executions, body, nil
}
