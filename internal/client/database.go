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

// DatabaseInfo is one database in the response from /api/v1/databases.
// Mirrors the server struct in arc/internal/api/databases.go.
type DatabaseInfo struct {
	Name             string `json:"name"`
	MeasurementCount int    `json:"measurement_count"`
	CreatedAt        string `json:"created_at,omitempty"`
}

// DatabaseListResponse is the response body of GET /api/v1/databases.
type DatabaseListResponse struct {
	Databases []DatabaseInfo `json:"databases"`
	Count     int            `json:"count"`
}

// DatabaseMeasurement is one measurement inside a database.
type DatabaseMeasurement struct {
	Name      string `json:"name"`
	FileCount int    `json:"file_count,omitempty"`
}

// MeasurementListResponse is the response body of
// GET /api/v1/databases/:name/measurements.
type MeasurementListResponse struct {
	Database     string                `json:"database"`
	Measurements []DatabaseMeasurement `json:"measurements"`
	Count        int                   `json:"count"`
}

// createDatabaseRequest is the on-the-wire body shape for
// POST /api/v1/databases.
type createDatabaseRequest struct {
	Name string `json:"name"`
}

// ListDatabases returns every database the caller can see, with
// measurement counts pre-computed by the server.
func (c *Client) ListDatabases(ctx context.Context) (*DatabaseListResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.Endpoint+"/api/v1/databases", nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	// Cross-database operation; the x-arc-database header is never
	// applicable here, so we use setCrossDBHeaders to make the
	// absence explicit.
	c.setCrossDBHeaders(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list databases: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, decodeWriteError(resp.StatusCode, body)
	}
	var out DatabaseListResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &out, nil
}

// GetDatabase returns metadata for one database. Returns a
// recognisable "not found" error for HTTP 404 so the command layer can
// distinguish "missing" from "broken."
func (c *Client) GetDatabase(ctx context.Context, name string) (*DatabaseInfo, error) {
	if name == "" {
		return nil, fmt.Errorf("database name is required")
	}
	u := c.cfg.Endpoint + "/api/v1/databases/" + url.PathEscape(name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	c.setCrossDBHeaders(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get database: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, decodeWriteError(resp.StatusCode, body)
	}
	var out DatabaseInfo
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &out, nil
}

// CreateDatabase creates a new empty database. Returns the server's
// freshly-created metadata on HTTP 201. The server enforces name
// validation (alphanumeric + `_-`, ≤ 64 chars, not a reserved name like
// "system" / "internal"); arcli forwards whatever the user typed and
// lets the server produce the canonical error.
func (c *Client) CreateDatabase(ctx context.Context, name string) (*DatabaseInfo, error) {
	if name == "" {
		return nil, fmt.Errorf("database name is required")
	}
	body, err := json.Marshal(createDatabaseRequest{Name: name})
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.Endpoint+"/api/v1/databases", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	c.setCrossDBHeaders(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("create database: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, decodeWriteError(resp.StatusCode, respBody)
	}
	var out DatabaseInfo
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &out, nil
}

// DeleteDatabase removes a database and ALL its data from the server.
// Always passes `?confirm=true` because the server requires it; the
// CLI's safety story is layered on top (the command refuses unless the
// user explicitly invokes `db drop`).
//
// The server may refuse with HTTP 403 if `delete.enabled` is false in
// arc.toml; the error message is surfaced verbatim so the operator
// knows to enable the flag server-side.
func (c *Client) DeleteDatabase(ctx context.Context, name string) error {
	if name == "" {
		return fmt.Errorf("database name is required")
	}
	u := c.cfg.Endpoint + "/api/v1/databases/" + url.PathEscape(name) + "?confirm=true"
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	c.setCrossDBHeaders(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("delete database: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// Drain any small body the server may include.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return decodeWriteError(resp.StatusCode, body)
}

// ListMeasurements returns measurements inside a single database via
// GET /api/v1/databases/:name/measurements.
func (c *Client) ListMeasurements(ctx context.Context, database string) (*MeasurementListResponse, error) {
	if database == "" {
		return nil, fmt.Errorf("database name is required")
	}
	u := c.cfg.Endpoint + "/api/v1/databases/" + url.PathEscape(database) + "/measurements"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	c.setCrossDBHeaders(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list measurements: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, decodeWriteError(resp.StatusCode, body)
	}
	var out MeasurementListResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &out, nil
}
