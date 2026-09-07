package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// DeleteRequest is the body of POST /api/v1/delete/. Where is a SQL
// WHERE fragment the server interpolates verbatim (after its own
// denylist); time bounds go inside it. Confirm is required by the
// server for a real delete; callers also set it on dry runs so a
// preview above the server's confirmation threshold is not refused.
type DeleteRequest struct {
	Database    string `json:"database"`
	Measurement string `json:"measurement"`
	Where       string `json:"where"`
	DryRun      bool   `json:"dry_run"`
	Confirm     bool   `json:"confirm"`
}

// DeleteResult is the body of POST /api/v1/delete/ on 200 and on 207
// (partial failure). Error paths use the same shape with Success=false.
type DeleteResult struct {
	Success         bool            `json:"success"`
	DeletedCount    int64           `json:"deleted_count"`
	AffectedFiles   int             `json:"affected_files"`
	RewrittenFiles  int             `json:"rewritten_files"`
	ExecutionTimeMs float64         `json:"execution_time_ms"`
	DryRun          bool            `json:"dry_run"`
	FilesProcessed  []string        `json:"files_processed"`
	FailedFiles     []string        `json:"failed_files,omitempty"`
	Error           string          `json:"error,omitempty"`
	Raw             json.RawMessage `json:"-"`
}

// PartialDeleteError is returned when the server reports that some rows
// or files were already deleted before a failure: HTTP 207 (some files
// failed), or a non-2xx whose body still carries counts (a mid-run
// abort). Result holds what the server managed to do.
type PartialDeleteError struct {
	Status int
	Result *DeleteResult
}

func (e *PartialDeleteError) Error() string {
	msg := scrubControls(e.Result.Error)
	if msg == "" {
		msg = fmt.Sprintf("HTTP %d", e.Status)
	}
	return fmt.Sprintf("delete partially applied: %d rows deleted in %d file(s), %d file(s) failed (%s)", e.Result.DeletedCount, len(e.Result.FilesProcessed), len(e.Result.FailedFiles), msg)
}

// DeleteConfig is GET /api/v1/delete/config.
type DeleteConfig struct {
	Enabled               bool              `json:"enabled"`
	ConfirmationThreshold int               `json:"confirmation_threshold"`
	MaxRowsPerDelete      int               `json:"max_rows_per_delete"`
	Implementation        string            `json:"implementation"`
	PerformanceImpact     map[string]string `json:"performance_impact"`
}

// StripWhereKeyword removes a leading "WHERE" (any case, followed by
// whitespace) and surrounding whitespace. The server strips it only from
// its validation copy and interpolates the raw text, so "WHERE x" would
// otherwise reach DuckDB as "WHERE WHERE x".
func StripWhereKeyword(where string) string {
	w := strings.TrimSpace(where)
	if len(w) > 6 && strings.EqualFold(w[:5], "WHERE") && (w[5] == ' ' || w[5] == '\t' || w[5] == '\n') {
		return strings.TrimSpace(w[6:])
	}
	return w
}

// IsFullTableWhere mirrors the server's full-table detection on the
// user's input: after trimming, upper-casing, and dropping a leading
// "WHERE ", the clause is exactly 1=1, TRUE, or 1.
func IsFullTableWhere(where string) bool {
	w := strings.ToUpper(StripWhereKeyword(where))
	return w == "1=1" || w == "TRUE" || w == "1"
}

// NormaliseWhere is what arcli actually sends: the stripped clause
// wrapped as "(<clause>) IS TRUE". Arc's rewrite keeps rows matching
// "NOT (<clause>)", which under SQL three-valued logic also drops rows
// whose predicate is NULL, while its preview counts only TRUE rows.
// Wrapping makes the preview and the rewrite agree and keeps NULL rows,
// the standard DELETE semantics.
func NormaliseWhere(where string) string {
	return "(" + StripWhereKeyword(where) + ") IS TRUE"
}

// ValidateWhere applies the cheap structural rules the server enforces
// so they fail before HTTP: non-empty, no statement separators or
// comments, balanced quotes and parentheses. Keyword and function
// denylists are left to the server, whose message is authoritative.
func ValidateWhere(where string) error {
	if strings.TrimSpace(where) == "" {
		return fmt.Errorf("--where is required (use --where \"1=1\" to delete every row of the measurement)")
	}
	for _, bad := range []string{";", "--", "/*"} {
		if strings.Contains(where, bad) {
			return fmt.Errorf("--where must not contain %q", bad)
		}
	}
	if strings.Count(where, "'")%2 != 0 {
		return fmt.Errorf("--where has unmatched quotes")
	}
	if strings.Count(where, "(") != strings.Count(where, ")") {
		return fmt.Errorf("--where has unmatched parentheses")
	}
	return nil
}

// validateDeleteName mirrors the server's path-safety check on the
// database and measurement names.
func validateDeleteName(kind, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("%s is required", kind)
	}
	if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return fmt.Errorf("%s name %q contains invalid characters", kind, name)
	}
	return nil
}

// DeleteRows calls POST /api/v1/delete/ (admin). The call is synchronous:
// the server rewrites every affected file before answering. Whenever
// the server reports work already done alongside a failure — HTTP 207,
// a mid-run 5xx carrying counts, or a 2xx with success=false or
// failed_files — the decoded result is returned with a
// *PartialDeleteError. A disabled delete feature is the server's own 403.
func (c *Client) DeleteRows(ctx context.Context, req DeleteRequest) (*DeleteResult, error) {
	if err := validateDeleteName("database", req.Database); err != nil {
		return nil, err
	}
	if err := validateDeleteName("measurement", req.Measurement); err != nil {
		return nil, err
	}
	if err := ValidateWhere(req.Where); err != nil {
		return nil, err
	}
	b, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.Endpoint+"/api/v1/delete/", bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	c.setCrossDBHeaders(httpReq)
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("POST /api/v1/delete/: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	// Every status carries a DeleteResponse; decode it first so partial
	// work is never lost behind a status code.
	var out DeleteResult
	decoded := json.Unmarshal(body, &out) == nil
	if decoded {
		normaliseDelete(&out, body)
	}
	partial := decoded && (out.DeletedCount > 0 || len(out.FilesProcessed) > 0 || len(out.FailedFiles) > 0)
	switch {
	case resp.StatusCode == http.StatusMultiStatus:
		if !decoded {
			return nil, fmt.Errorf("decode partial-failure response: %w", json.Unmarshal(body, &out))
		}
		return &out, &PartialDeleteError{Status: resp.StatusCode, Result: &out}
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		if partial && !out.DryRun {
			return &out, &PartialDeleteError{Status: resp.StatusCode, Result: &out}
		}
		return nil, decodeWriteError(resp.StatusCode, body)
	case !decoded:
		return nil, fmt.Errorf("decode response: %w", json.Unmarshal(body, &out))
	case !out.Success || len(out.FailedFiles) > 0:
		if !partial {
			return nil, fmt.Errorf("arc: unexpected delete response (HTTP %d, success=false, no counts)", resp.StatusCode)
		}
		return &out, &PartialDeleteError{Status: resp.StatusCode, Result: &out}
	}
	return &out, nil
}

func normaliseDelete(r *DeleteResult, body []byte) {
	if r.FilesProcessed == nil {
		r.FilesProcessed = []string{}
	}
	if r.FailedFiles == nil {
		r.FailedFiles = []string{}
	}
	r.Raw = body
}

// GetDeleteConfig calls GET /api/v1/delete/config (admin). Always 200
// on Arc; Enabled tells whether POST would be refused.
func (c *Client) GetDeleteConfig(ctx context.Context) (*DeleteConfig, error) {
	body, err := c.getRaw(ctx, "/api/v1/delete/config", 64<<10)
	if err != nil {
		return nil, err
	}
	var out DeleteConfig
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode delete config: %w", err)
	}
	return &out, nil
}
