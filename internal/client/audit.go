package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	auditFeature   = "audit logging"
	auditConfigKey = "audit_log.enabled"
	auditBase      = "/api/v1/audit/"
	AuditMaxLimit  = 10000
)

// AuditEntry mirrors Arc's stored audit entry.
type AuditEntry struct {
	ID          int64     `json:"id"`
	Timestamp   time.Time `json:"timestamp"`
	EventType   string    `json:"event_type"`
	Actor       string    `json:"actor"`
	Method      string    `json:"method"`
	Path        string    `json:"path"`
	Database    string    `json:"database,omitempty"`
	Measurement string    `json:"measurement,omitempty"`
	StatusCode  int       `json:"status_code"`
	IPAddress   string    `json:"ip_address"`
	UserAgent   string    `json:"user_agent"`
	DurationMs  int64     `json:"duration_ms"`
	Detail      string    `json:"detail,omitempty"`
}

// AuditLogsOptions contains the server-side filters and pagination.
type AuditLogsOptions struct {
	EventType string
	Actor     string
	Database  string
	Since     string
	Until     string
	Limit     int
	Offset    int
}

// AuditLogsResult is GET /api/v1/audit/logs. Data is normalised to an
// empty slice when the server returns null; Raw retains other response fields.
type AuditLogsResult struct {
	Success bool            `json:"success"`
	Data    []AuditEntry    `json:"data"`
	Count   int             `json:"count"`
	Raw     json.RawMessage `json:"-"`
}

// AuditStatsResult is GET /api/v1/audit/stats.
type AuditStatsResult struct {
	Success bool             `json:"success"`
	Data    map[string]int64 `json:"data"`
	Since   string           `json:"since"`
	Raw     json.RawMessage  `json:"-"`
}

func validateAuditTime(name, value string) error {
	if value == "" {
		return nil
	}
	if _, err := time.Parse(time.RFC3339, value); err != nil {
		return fmt.Errorf("%s must be an RFC3339 timestamp (got %q)", name, value)
	}
	return nil
}

// AuditLogs calls GET /api/v1/audit/logs. The database filter is a query
// parameter; this endpoint is cross-database and never uses the connection's
// default x-arc-database header.
func (c *Client) AuditLogs(ctx context.Context, opts AuditLogsOptions) (*AuditLogsResult, error) {
	if err := validateAuditTime("since", opts.Since); err != nil {
		return nil, err
	}
	if err := validateAuditTime("until", opts.Until); err != nil {
		return nil, err
	}
	if opts.Limit < 0 || opts.Limit > AuditMaxLimit {
		return nil, fmt.Errorf("limit must be between 1 and %d when provided (got %d)", AuditMaxLimit, opts.Limit)
	}
	if opts.Offset < 0 {
		return nil, fmt.Errorf("offset must be >= 0 (got %d)", opts.Offset)
	}

	q := url.Values{}
	for key, value := range map[string]string{
		"event_type": opts.EventType,
		"actor":      opts.Actor,
		"database":   opts.Database,
		"since":      opts.Since,
		"until":      opts.Until,
	} {
		if value != "" {
			q.Set(key, value)
		}
	}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Offset > 0 {
		q.Set("offset", strconv.Itoa(opts.Offset))
	}
	path := auditBase + "logs"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}

	var out AuditLogsResult
	body, err := c.featureJSON(ctx, auditFeature, auditConfigKey, http.MethodGet, path, nil, 16<<20, &out)
	if err != nil {
		return nil, err
	}
	if out.Data == nil {
		out.Data = []AuditEntry{}
		var fields map[string]json.RawMessage
		if json.Unmarshal(body, &fields) == nil && fields != nil {
			fields["data"] = json.RawMessage("[]")
			if normalized, err := json.Marshal(fields); err == nil {
				body = normalized
			}
		}
	}
	out.Raw = body
	return &out, nil
}

// AuditStats calls GET /api/v1/audit/stats. An empty since uses Arc's
// default window of the last 24 hours.
func (c *Client) AuditStats(ctx context.Context, since string) (*AuditStatsResult, error) {
	if err := validateAuditTime("since", since); err != nil {
		return nil, err
	}
	path := auditBase + "stats"
	if since != "" {
		q := url.Values{}
		q.Set("since", since)
		path += "?" + q.Encode()
	}
	var out AuditStatsResult
	body, err := c.featureJSON(ctx, auditFeature, auditConfigKey, http.MethodGet, path, nil, 1<<20, &out)
	if err != nil {
		return nil, err
	}
	if out.Data == nil {
		out.Data = map[string]int64{}
		var fields map[string]json.RawMessage
		if json.Unmarshal(body, &fields) == nil && fields != nil {
			fields["data"] = json.RawMessage("{}")
			if normalized, err := json.Marshal(fields); err == nil {
				body = normalized
			}
		}
	}
	out.Raw = body
	return &out, nil
}
