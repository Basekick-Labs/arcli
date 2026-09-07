package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// HealthResponse is the decoded GET /health payload. Arc's handler
// (arc/internal/api/server.go#healthHandler) always reports
// status "ok" while the process is up; the interesting signal is the
// per-tier storage state and, when licensed, the license block. Both
// are passed through as raw JSON so `-o json` reproduces the server's
// exact shape without arcli having to track its schema.
type HealthResponse struct {
	Status    string          `json:"status"`
	Time      string          `json:"time,omitempty"`
	Uptime    string          `json:"uptime,omitempty"`
	UptimeSec float64         `json:"uptime_sec,omitempty"`
	Storage   json.RawMessage `json:"storage,omitempty"`
	License   json.RawMessage `json:"license,omitempty"`

	// Latency is the client-measured round trip, not a server field.
	Latency time.Duration `json:"-"`
}

// Health calls GET /health. The route is public, so no Authorization
// header is sent — there is no reason to hand the token to an
// unauthenticated path. A non-2xx status or a body that is not Arc's
// health JSON (a proxy error page, for instance) is an error.
func (c *Client) Health(ctx context.Context) (*HealthResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.Endpoint+"/health", nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	c.setIdentityHeaders(req)

	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET /health: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	latency := time.Since(start)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, decodeWriteError(resp.StatusCode, body)
	}
	var out HealthResponse
	// Arc's handler always emits status, time, and uptime. Requiring
	// more than a bare {"status":"ok"} keeps a load balancer's own
	// health page from passing as Arc (which would also make ping's
	// "verify 404 = auth disabled" inference fire on a non-Arc host).
	if err := json.Unmarshal(body, &out); err != nil || out.Status == "" || out.Time == "" || out.Uptime == "" {
		return nil, fmt.Errorf("GET /health: not an Arc health response (HTTP %d, %d bytes)", resp.StatusCode, len(body))
	}
	out.Latency = latency
	return &out, nil
}
