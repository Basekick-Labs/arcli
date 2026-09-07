package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// LogLevels is the server's level ladder (arc/internal/logger/buffer.go
// matchesLevel). A level filter is a threshold: "warn" returns WARN,
// ERROR and FATAL entries.
var LogLevels = []string{"debug", "info", "warn", "error", "fatal"}

const (
	// LogsMaxLimit / LogsMaxSince mirror the server's bounds; values
	// outside them are silently replaced by the server defaults (100 and
	// 60 minutes), so the client validates instead.
	LogsMaxLimit = 1000
	LogsMaxSince = 24 * time.Hour
)

// LogEntry mirrors arc/internal/logger/buffer.go#LogEntry.
type LogEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Level     string    `json:"level"`
	Component string    `json:"component,omitempty"`
	Message   string    `json:"message"`
	Caller    string    `json:"caller,omitempty"`
}

// LogsResult is GET /api/v1/logs. Entries are newest first. The server
// keeps a ring buffer of the last 10 000 entries and returns at most
// 1000; `logs` is null when nothing matched and is normalised to [].
type LogsResult struct {
	Timestamp    string          `json:"timestamp"`
	Count        int             `json:"count"`
	Limit        int             `json:"limit"`
	LevelFilter  string          `json:"level_filter"`
	SinceMinutes int             `json:"since_minutes"`
	Logs         []LogEntry      `json:"logs"`
	Raw          json.RawMessage `json:"-"`
}

// LogsOptions are the query parameters. Zero values mean "server default".
type LogsOptions struct {
	Limit int           // 1..1000
	Level string        // one of LogLevels (threshold)
	Since time.Duration // 1m..24h, rounded up to whole minutes
}

// Logs calls GET /api/v1/logs (admin when auth is on).
func (c *Client) Logs(ctx context.Context, opts LogsOptions) (*LogsResult, error) {
	q := url.Values{}
	if opts.Limit != 0 {
		if opts.Limit < 1 || opts.Limit > LogsMaxLimit {
			return nil, fmt.Errorf("limit must be between 1 and %d (got %d)", LogsMaxLimit, opts.Limit)
		}
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Level != "" {
		lvl := strings.ToLower(opts.Level)
		if !contains(LogLevels, lvl) {
			return nil, fmt.Errorf("invalid level %q (valid: %s)", opts.Level, strings.Join(LogLevels, ", "))
		}
		q.Set("level", lvl)
	}
	if opts.Since != 0 {
		if opts.Since < time.Minute || opts.Since > LogsMaxSince {
			return nil, fmt.Errorf("since must be between 1m and 24h (got %s)", opts.Since)
		}
		minutes := int((opts.Since + time.Minute - 1) / time.Minute)
		q.Set("since_minutes", strconv.Itoa(minutes))
	}
	path := "/api/v1/logs"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	body, err := c.getRaw(ctx, path, 16<<20)
	if err != nil {
		return nil, err
	}
	var out LogsResult
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode logs: %w", err)
	}
	if out.Logs == nil {
		out.Logs = []LogEntry{}
		// Mirror the normalisation in the raw document so -o json emits
		// "logs": [] and `jq '.logs[]'` iterates an empty window cleanly.
		// Other keys are kept verbatim.
		var fields map[string]json.RawMessage
		if json.Unmarshal(body, &fields) == nil && fields != nil {
			fields["logs"] = json.RawMessage("[]")
			if nb, err := json.Marshal(fields); err == nil {
				body = nb
			}
		}
	}
	out.Raw = body
	return &out, nil
}

// ImportStats is GET /api/v1/import/stats: process-wide counters across
// every import format since the server started.
type ImportStats struct {
	TotalRequests int64 `json:"total_requests"`
	TotalRecords  int64 `json:"total_records"`
	TotalErrors   int64 `json:"total_errors"`
}

// GetImportStats calls GET /api/v1/import/stats (any token).
func (c *Client) GetImportStats(ctx context.Context) (*ImportStats, json.RawMessage, error) {
	body, err := c.getRaw(ctx, "/api/v1/import/stats", 64<<10)
	if err != nil {
		return nil, nil, err
	}
	var out struct {
		Stats ImportStats `json:"stats"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, nil, fmt.Errorf("decode import stats: %w", err)
	}
	return &out.Stats, body, nil
}
