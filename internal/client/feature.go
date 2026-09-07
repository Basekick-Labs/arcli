package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// FeatureDisabledError is returned when an optional server feature's
// routes are not registered (its `<feature>.enabled` config is false).
// Arc's router answers such paths with HTTP 404 {"error":"Cannot GET
// ..."}; the error is only produced after the strict /health check has
// confirmed the host really is Arc, so a wrong base path or another
// service is never misreported as a server config problem.
type FeatureDisabledError struct {
	Feature   string // human name, e.g. "retention policies"
	ConfigKey string // e.g. "retention.enabled"
}

func (e *FeatureDisabledError) Error() string {
	return fmt.Sprintf("%s are disabled on this server (%s=false)", e.Feature, e.ConfigKey)
}

// redactedEndpoint hides any userinfo an endpoint URL might carry
// before it is echoed in an error.
func redactedEndpoint(endpoint string) string {
	if u, err := url.Parse(endpoint); err == nil {
		return u.Redacted()
	}
	return endpoint
}

// classifyFeatureRouteMissing maps Fiber's route-missing 404 to a
// FeatureDisabledError once /health proves the endpoint is Arc; any
// other error is returned unchanged.
func (c *Client) classifyFeatureRouteMissing(ctx context.Context, err error, feature, configKey string) error {
	if !isRouteMissing(err) {
		return err
	}
	if _, herr := c.Health(ctx); herr != nil {
		return fmt.Errorf("%w (and %s does not answer like an Arc server: %v; check the endpoint path)", err, redactedEndpoint(c.cfg.Endpoint), herr)
	}
	return &FeatureDisabledError{Feature: feature, ConfigKey: configKey}
}

// featureJSON is the shared request helper for feature-gated JSON
// routes: optional JSON body, bounded read, non-2xx → *HTTPError (or
// FeatureDisabledError for a missing route), 2xx → body returned and,
// when out is non-nil, decoded into it. A 2xx body of "null" is
// returned as-is so callers can normalise it.
func (c *Client) featureJSON(ctx context.Context, feature, configKey, method, path string, reqBody any, limit int64, out any) ([]byte, error) {
	var body io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.Endpoint+path, body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.setCrossDBHeaders(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, c.classifyFeatureRouteMissing(ctx, decodeWriteError(resp.StatusCode, respBody), feature, configKey)
	}
	if out != nil {
		if err := json.Unmarshal(respBody, out); err != nil {
			return nil, fmt.Errorf("decode response: %w", err)
		}
	}
	return respBody, nil
}
