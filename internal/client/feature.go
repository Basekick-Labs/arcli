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

// FeatureDisabledError is the published error of the legacy feature
// groups (retention policies, continuous queries, backups) when their
// routes are not registered. Arc's router answers such paths with HTTP 404
// {"error":"Cannot GET ..."}; the error is only produced after the strict
// /health check has confirmed the host really is Arc, so a wrong base path
// or another service is never misreported as a server config problem.
//
// Its text is relied on by operator scripts: the legacy part is kept
// byte-identical and anything added goes after it (see Note).
// It is NOT used for newer feature groups: a 404 alone does not establish
// why a route is missing (configuration, license, a failed start, or the
// server version), so those keep the server's *HTTPError. See
// classifyFeatureRouteMissing.
type FeatureDisabledError struct {
	Feature   string // human name, e.g. "retention policies"
	ConfigKey string // e.g. "retention.enabled"
	// Note, when set, is appended after the legacy text ("; <Note>") so
	// scripts matching the legacy text as a substring keep matching.
	Note string
}

func (e *FeatureDisabledError) Error() string {
	msg := fmt.Sprintf("%s are disabled on this server (%s=false)", e.Feature, e.ConfigKey)
	if e.Note != "" {
		msg += "; " + e.Note
	}
	return msg
}

// redactedEndpoint hides any userinfo an endpoint URL might carry
// before it is echoed in an error.
func redactedEndpoint(endpoint string) string {
	if u, err := url.Parse(endpoint); err == nil {
		return u.Redacted()
	}
	return endpoint
}

// legacyFeatureDisabledKeys are the feature groups whose route-missing
// 404 has been published as FeatureDisabledError. Frozen: do not add keys.
// Newer groups report the wrapped *HTTPError instead, and that is their
// published behaviour; turning one into FeatureDisabledError later would
// change its message and its errors.As behaviour.
var legacyFeatureDisabledKeys = map[string]bool{
	retentionConfigKey: true,
	cqConfigKey:        true,
	backupConfigKey:    true,
}

// legacyFeatureDisabledNotes completes a legacy message that can be wrong.
// Arc also leaves the backup routes unregistered when backup.enabled is
// true but the backup manager fails to start (it logs the error and keeps
// serving), so "(backup.enabled=false)" alone may send the operator to the
// wrong place. Retention and continuous-query handler failures stop Arc,
// so their legacy text is accurate and gets no note.
var legacyFeatureDisabledNotes = map[string]string{
	backupConfigKey: "if backup.enabled is true, the backup manager failed to start (see the server log)",
}

// routeNotRegisteredHint is appended to a route-missing *HTTPError of a
// non-legacy feature group once /health proves the endpoint is Arc. It
// names what to check without asserting a cause. Its wording is part of
// the published error text: change it only with a release note.
const routeNotRegisteredHint = "Arc is reachable but did not register this route; check %sthe license, the server log, and that this Arc version supports this command"

// classifyFeatureRouteMissing handles Fiber's route-missing 404 once /health
// proves the endpoint is Arc; any other error is returned unchanged.
//
//   - Legacy groups (legacyFeatureDisabledKeys) get FeatureDisabledError
//     with the text of earlier releases; backups append a note after it.
//   - Every other group keeps the original *HTTPError (inspectable through
//     errors.As) wrapped with routeNotRegisteredHint. An empty configKey
//     omits the config check, for groups whose routes do not depend on one.
//
// If /health fails, the endpoint itself is suspect and that is reported
// instead, for every group, without the hint.
func (c *Client) classifyFeatureRouteMissing(ctx context.Context, err error, feature, configKey string) error {
	if !isRouteMissing(err) {
		return err
	}
	if _, herr := c.Health(ctx); herr != nil {
		return fmt.Errorf("%w (and %s does not answer like an Arc server: %v; check the endpoint path)", err, redactedEndpoint(c.cfg.Endpoint), herr)
	}
	if legacyFeatureDisabledKeys[configKey] {
		return &FeatureDisabledError{Feature: feature, ConfigKey: configKey, Note: legacyFeatureDisabledNotes[configKey]}
	}
	check := ""
	if configKey != "" {
		check = configKey + ", "
	}
	return fmt.Errorf("%w; "+routeNotRegisteredHint, err, check)
}

// featureJSON is the shared request helper for feature-gated JSON
// routes: optional JSON body, bounded read, non-2xx → *HTTPError (a
// missing route is classified by classifyFeatureRouteMissing), 2xx → body
// returned and,
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
