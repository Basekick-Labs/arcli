package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// QueryResult is the decoded JSON response from /api/v1/query.
//
// The JSON endpoint returns ROW-MAJOR data: Data[i] is the i-th row,
// a slice of length len(Columns) holding that row's cells.
// (The msgpack endpoint is columnar; the arrow endpoint is binary IPC.
// This shape is JSON-only — see arc/internal/api/query_msgpack_response.go).
type QueryResult struct {
	// Columns is the list of column names in display order.
	Columns []string `json:"columns"`

	// Data is row-major: Data[rowIdx] is one row's cells, in the same
	// order as Columns. Each cell is whatever JSON decoded into —
	// typically string, float64, bool, nil, or json.Number depending
	// on the column's source type.
	Data [][]any `json:"data"`

	// RowCount is the number of rows. The server sets this as
	// len(Data); we surface it explicitly to handle the empty case
	// (Data may be `null` over the wire when no rows match).
	RowCount int `json:"row_count"`

	// ExecutionTimeMs is the server-side execution time.
	ExecutionTimeMs float64 `json:"execution_time_ms"`
}

// queryRequest is the on-the-wire body shape for /api/v1/query.
type queryRequest struct {
	SQL string `json:"sql"`
}

// errorResponse is the JSON body Arc returns on a failed query.
// `Error` is always populated; `Success` is `false` (but we don't
// rely on that — we key off the HTTP status code).
type errorResponse struct {
	Success bool   `json:"success"`
	Error   string `json:"error"`
}

// QueryJSON runs a SQL query against the Arc server and returns the
// column-major result. The database arg overrides the client's
// default (empty == use client default == eventually Arc's "default").
//
// Errors map as follows:
//   - Network / timeout failures: returned as-is, wrapped.
//   - 2xx but malformed body: returned as a parse error.
//   - Non-2xx with a JSON error body: returned as `arc: <message>`.
//   - Non-2xx with a non-JSON body (e.g. plain-text 502 from a proxy):
//     returned as `arc: HTTP <status>: <truncated body>`.
func (c *Client) QueryJSON(ctx context.Context, sql, database string) (*QueryResult, error) {
	body, err := json.Marshal(queryRequest{SQL: sql})
	if err != nil {
		return nil, fmt.Errorf("encode query: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.Endpoint+"/api/v1/query", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	c.setCommonHeaders(req, database)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer resp.Body.Close()

	// Bound the body read so a misbehaving proxy can't OOM the CLI.
	// 64 MiB is generous for a JSON query response (Arrow IPC is the
	// path for "big" results); Arc enforces its own server-side cap.
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, decodeServerError(resp.StatusCode, respBody)
	}

	var qr QueryResult
	if err := json.Unmarshal(respBody, &qr); err != nil {
		return nil, fmt.Errorf("decode query response: %w", err)
	}
	return &qr, nil
}

// decodeServerError translates a non-2xx response into a Go error.
// Tries JSON first (Arc's normal failure shape), falls back to a
// truncated raw body for proxies / load balancers that don't speak
// Arc's protocol.
func decodeServerError(status int, body []byte) error {
	var er errorResponse
	if err := json.Unmarshal(body, &er); err == nil && er.Error != "" {
		return &HTTPError{Status: status, Message: scrubControls(er.Error), Body: body}
	}
	// Non-JSON or JSON-but-no-error-field. Truncate so a multi-MB
	// HTML error page from nginx doesn't fill the terminal.
	const maxRawLen = 512
	raw := string(body)
	if len(raw) > maxRawLen {
		raw = raw[:maxRawLen] + "...[truncated]"
	}
	return &HTTPError{Status: status, Raw: scrubControls(raw), Body: body}
}

// RowAt returns the i-th row of a QueryResult. Returns nil if i is
// out of range. Since Data is already row-major this is a direct
// index — the helper exists so renderers don't have to repeat the
// bounds check.
func (qr *QueryResult) RowAt(i int) []any {
	if i < 0 || i >= len(qr.Data) {
		return nil
	}
	return qr.Data[i]
}
