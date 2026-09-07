package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// EstimateResult is the body of POST /api/v1/query/estimate. The server
// runs `SELECT COUNT(*) FROM (<sql>)` for real and classifies the count:
// none (≤10k), low (≤100k), medium (≤1M), high (>1M); "error" on a
// timeout. A failed estimate comes back as HTTP 200 with Success=false,
// EstimatedRows=nil and Error set, so callers branch on Success.
type EstimateResult struct {
	Success         bool            `json:"success"`
	EstimatedRows   *int64          `json:"estimated_rows"`
	WarningLevel    string          `json:"warning_level"`
	WarningMessage  string          `json:"warning_message,omitempty"`
	ExecutionTimeMs float64         `json:"execution_time_ms"`
	Error           string          `json:"error,omitempty"`
	Raw             json.RawMessage `json:"-"`
}

// EstimateQuery calls POST /api/v1/query/estimate (read tier). The
// database goes in the x-arc-database header exactly as for QueryJSON.
// A server-reported failure (Success=false, any status) is returned as
// an error carrying the server's message; the decoded result is still
// returned alongside it so callers can print the raw body.
func (c *Client) EstimateQuery(ctx context.Context, sql, database string) (*EstimateResult, error) {
	if sql == "" {
		return nil, fmt.Errorf("sql is required")
	}
	body, err := json.Marshal(queryRequest{SQL: sql})
	if err != nil {
		return nil, fmt.Errorf("encode query: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.Endpoint+"/api/v1/query/estimate", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	c.setCommonHeaders(req, database)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("estimate query: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	var out EstimateResult
	decoded := json.Unmarshal(respBody, &out) == nil
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if decoded && out.Error != "" {
			out.Raw = respBody
			return &out, &HTTPError{Status: resp.StatusCode, Message: scrubControls(out.Error), Body: respBody}
		}
		return nil, decodeServerError(resp.StatusCode, respBody)
	}
	if !decoded {
		return nil, fmt.Errorf("decode estimate response: %w", json.Unmarshal(respBody, &out))
	}
	out.Raw = respBody
	if !out.Success {
		msg := scrubControls(out.Error)
		if msg == "" {
			msg = "estimate failed"
		}
		return &out, fmt.Errorf("arc: %s", msg)
	}
	return &out, nil
}
