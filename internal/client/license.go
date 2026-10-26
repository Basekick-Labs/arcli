package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// HealthLicense is Arc's public /health license block, not the narrower
// /api/v1/cluster license shape. Optional pointers preserve omitted values
// rather than inventing an expiry, zero remaining days, or site-license flag.
type HealthLicense struct {
	Tier          string     `json:"tier"`
	Status        string     `json:"status"`
	Source        string     `json:"source"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	DaysRemaining *int       `json:"days_remaining,omitempty"`
	SiteLicense   *bool      `json:"site_license,omitempty"`
}

// LicenseStatusResponse distinguishes an absent license configuration from
// a configured server reporting status "unlicensed". Both are successful reads.
type LicenseStatusResponse struct {
	Configured bool           `json:"configured"`
	License    *HealthLicense `json:"license"`
}

// LicenseStatus reads public health only: no token or auth verification is
// needed, even when the resolved connection has a token. Keep the shared raw
// HealthResponse.License intact so ping can still preserve unknown fields.
func (c *Client) LicenseStatus(ctx context.Context) (*LicenseStatusResponse, error) {
	h, err := c.Health(ctx)
	if err != nil {
		return nil, err
	}
	if h.Status != "ok" {
		return nil, fmt.Errorf("GET /health: server did not report status ok")
	}
	raw := bytes.TrimSpace(h.License)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return &LicenseStatusResponse{}, nil
	}
	var lic HealthLicense
	if err := json.Unmarshal(raw, &lic); err != nil {
		// Do not echo malformed server values: a proxy may return sensitive
		// content, and timestamp decoder errors can include the original value.
		return nil, fmt.Errorf("GET /health: malformed license block")
	}
	if lic.Tier == "" || lic.Status == "" {
		return nil, fmt.Errorf("GET /health: license block missing tier or status")
	}
	if lic.ExpiresAt != nil {
		utc := lic.ExpiresAt.UTC()
		lic.ExpiresAt = &utc
	}
	// Status and days remaining are server-owned. A workstation clock must
	// not change the reported licensing state or imply local enforcement.
	return &LicenseStatusResponse{Configured: true, License: &lic}, nil
}
