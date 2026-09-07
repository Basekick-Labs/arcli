package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Precision is the timestamp precision sent on a line-protocol write
// via the `?precision=` query param. Empty maps to nanoseconds (Arc's
// default).
type Precision string

const (
	PrecisionNS Precision = "ns"
	PrecisionUS Precision = "us"
	PrecisionMS Precision = "ms"
	PrecisionS  Precision = "s"
)

// ValidPrecision reports whether s is one of the four precisions Arc
// accepts. Empty string is treated as valid (server applies its default).
func ValidPrecision(s string) bool {
	switch Precision(s) {
	case "", PrecisionNS, PrecisionUS, PrecisionMS, PrecisionS:
		return true
	}
	return false
}

// WriteLineProtocol POSTs raw line-protocol bytes to /api/v1/write/line-protocol.
//
// The body io.Reader is streamed — we never buffer it fully. Callers
// passing an os.File or a stdin pipe get true streaming behaviour.
//
// `precision` may be empty (Arc applies nanosecond default).
// `database` overrides the client's default database.
//
// Returns nil on success (HTTP 204 No Content) and a decoded server
// error on any non-2xx response.
func (c *Client) WriteLineProtocol(ctx context.Context, body io.Reader, database string, precision Precision) error {
	if !ValidPrecision(string(precision)) {
		return fmt.Errorf("invalid precision %q (must be one of ns, us, ms, s)", precision)
	}

	u, err := url.Parse(c.cfg.Endpoint + "/api/v1/write/line-protocol")
	if err != nil {
		return fmt.Errorf("build write URL: %w", err)
	}
	if precision != "" {
		q := u.Query()
		q.Set("precision", string(precision))
		u.RawQuery = q.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), body)
	if err != nil {
		return fmt.Errorf("build write request: %w", err)
	}
	req.Header.Set("Content-Type", "text/plain")
	c.setCommonHeaders(req, database)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("write: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// Drain any small body Arc emits (currently 204 with empty
		// body, but defensive against future changes).
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil
	}

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return decodeWriteError(resp.StatusCode, respBody)
}

// decodeWriteError turns a non-2xx response into an *HTTPError. It is
// the shared decoder for every JSON endpoint (write, databases, import,
// auth, health): Arc's error bodies are `{"error": "..."}`, optionally
// with a `success:false`, and a non-JSON body is kept as a truncated
// raw string.
func decodeWriteError(status int, body []byte) error {
	var er struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &er); err == nil && er.Error != "" {
		return &HTTPError{Status: status, Message: scrubControls(er.Error)}
	}
	const maxRawLen = 512
	raw := string(body)
	if len(raw) > maxRawLen {
		raw = raw[:maxRawLen] + "...[truncated]"
	}
	return &HTTPError{Status: status, Raw: scrubControls(raw)}
}

// scrubControls strips C0/C1 control characters and Unicode bidi
// overrides from server-supplied text before it can reach a terminal
// through an error message. Server error strings can echo request
// content (a CQ's SQL, a name chosen by another admin).
func scrubControls(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r < 0x20 && r != '\n' && r != '\t', r == 0x7f, r >= 0x80 && r <= 0x9f:
			return -1
		case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
			return -1
		}
		return r
	}, s)
}

// HTTPError is a non-2xx response from Arc. Message is the server's
// JSON `error` field when the body was JSON; otherwise Raw holds a
// truncated copy of the body. Callers that need to branch on status
// (e.g. treating 404 on an optional route as "feature disabled") use
// errors.As; everyone else just prints Error().
type HTTPError struct {
	Status  int
	Message string
	Raw     string
}

func (e *HTTPError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("arc: %s (HTTP %d)", e.Message, e.Status)
	}
	return fmt.Sprintf("arc: HTTP %d: %s", e.Status, e.Raw)
}
