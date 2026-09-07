// Package client is the HTTP wire-format adapter for the Arc server.
//
// Arc speaks JSON-over-HTTP for queries (row-major response, see
// QueryResult), Arrow IPC streaming for large query results, and
// line-protocol POST for writes. This package wraps those endpoints
// so the command layer doesn't have to know wire-format details.
//
// Conventions:
//   - The `x-arc-database` header selects the target database for
//     every request (query + write). If empty the server defaults to
//     "default".
//   - Authorization is always `Bearer <token>`.
//   - Errors come back as JSON `{"success": false, "error": "..."}`
//     on the query endpoints and `{"error": "..."}` on writes. The
//     Do* helpers in this package normalise both into a Go error.
//
// The client deliberately uses a configured `*http.Client` rather than
// `http.DefaultClient` so timeouts and TLS verification are explicit.
package client

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// HeaderDatabase is the request header Arc uses to select the target
// database for both query and write endpoints.
const HeaderDatabase = "x-arc-database"

// ArrowExecutionTimeTrailer is the HTTP response trailer Arc emits at
// the end of an Arrow IPC stream carrying server-side execution time
// in milliseconds. Clients must read the response body to EOF before
// reading this trailer (HTTP/1.1 trailer semantics).
const ArrowExecutionTimeTrailer = "Arc-Execution-Time-Ms"

// Config holds the per-client tuning knobs. Endpoint and Token are
// required; everything else has a sensible default.
type Config struct {
	// Endpoint is the Arc HTTP base URL, e.g. "http://localhost:8000"
	// (no trailing slash; we add the API paths ourselves).
	Endpoint string

	// Token is the Bearer token from Arc's first-run banner.
	Token string

	// Database is the default database name to send via x-arc-database
	// when the per-call override is empty. May itself be empty, in
	// which case Arc defaults to "default" server-side.
	Database string

	// InsecureTLS skips certificate verification. Off by default.
	// When true, the caller is responsible for warning the user.
	InsecureTLS bool

	// Timeout is the per-request HTTP timeout. Default 60s.
	// Writes and small queries finish well under this; for large
	// `-o arrow` streams we override on the request.
	Timeout time.Duration
}

// Client is a stateful adapter around *http.Client + auth headers.
// One Client per Arc cluster; safe for concurrent use.
type Client struct {
	cfg  Config
	http *http.Client
}

// New builds a Client. Returns an error only on missing required
// config; transport construction never fails.
func New(cfg Config) (*Client, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("client: endpoint required")
	}
	if cfg.Token == "" {
		return nil, fmt.Errorf("client: token required")
	}
	cfg.Endpoint = strings.TrimRight(cfg.Endpoint, "/")
	if cfg.Timeout == 0 {
		cfg.Timeout = 60 * time.Second
	}

	// Clone DefaultTransport so we don't mutate the package global.
	// We need to set TLSClientConfig per-Client (InsecureTLS varies).
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: cfg.InsecureTLS} //nolint:gosec // opt-in via --insecure / insecure_tls
	transport.DialContext = (&net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext

	return &Client{
		cfg: cfg,
		http: &http.Client{
			Transport: transport,
			Timeout:   cfg.Timeout,
		},
	}, nil
}

// Endpoint returns the base URL the client is configured against.
// Useful for `-v` verbose output.
func (c *Client) Endpoint() string { return c.cfg.Endpoint }

// DefaultDatabase returns the database the client falls back to when
// a per-call override is empty. May itself be empty (in which case
// Arc applies its server-side default, "default"). Exposed so the
// command layer can implement "use the connection's default DB"
// semantics for endpoints whose URL is database-scoped (e.g.
// /api/v1/databases/:name/measurements) where the server can't
// apply a fallback for us.
func (c *Client) DefaultDatabase() string { return c.cfg.Database }

// resolveDatabase picks the per-call override if set, else the
// client's default. Both can be empty (Arc falls back to "default").
func (c *Client) resolveDatabase(override string) string {
	if override != "" {
		return override
	}
	return c.cfg.Database
}

// setCommonHeaders writes Authorization, x-arc-database (per the
// resolveDatabase rules), and User-Agent onto a *http.Request. Use
// for per-database endpoints (query, write).
func (c *Client) setCommonHeaders(req *http.Request, database string) {
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	if db := c.resolveDatabase(database); db != "" {
		req.Header.Set(HeaderDatabase, db)
	}
	req.Header.Set("User-Agent", "arcli")
}

// setCrossDBHeaders writes Authorization + User-Agent but NEVER sends
// x-arc-database. Use for endpoints whose URL is itself the database
// selector (e.g. /api/v1/databases, /api/v1/databases/:name/...) or
// that are inherently cross-database (the list endpoints).
//
// Distinct from setCommonHeaders so the no-DB case is intentional, not
// accidental — calling setCommonHeaders(req, "") on a client with a
// non-empty default Database would silently include the wrong header.
func (c *Client) setCrossDBHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	req.Header.Set("User-Agent", "arcli")
}
