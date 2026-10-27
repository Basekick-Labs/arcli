package client

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
)

const (
	governanceFeature   = "query governance"
	governanceConfigKey = "governance.enabled"
	governanceBase      = "/api/v1/governance"
)

// GovernancePolicy mirrors Arc's governance policy response.
type GovernancePolicy struct {
	ID                 int64  `json:"id"`
	TokenID            int64  `json:"token_id"`
	TokenName          string `json:"token_name,omitempty"`
	RateLimitPerMinute int    `json:"rate_limit_per_minute"`
	RateLimitPerHour   int    `json:"rate_limit_per_hour"`
	MaxQueriesPerHour  int    `json:"max_queries_per_hour"`
	MaxQueriesPerDay   int    `json:"max_queries_per_day"`
	MaxRowsPerQuery    int    `json:"max_rows_per_query"`
	MaxScanDurationSec int    `json:"max_scan_duration_sec"`
	CreatedAt          string `json:"created_at"`
	UpdatedAt          string `json:"updated_at"`
}

// GovernancePolicyRequest is the full POST/PUT policy body. PUT replaces
// every limit, so callers updating a policy must send the merged values.
type GovernancePolicyRequest struct {
	TokenID            int64 `json:"token_id"`
	RateLimitPerMinute int   `json:"rate_limit_per_minute"`
	RateLimitPerHour   int   `json:"rate_limit_per_hour"`
	MaxQueriesPerHour  int   `json:"max_queries_per_hour"`
	MaxQueriesPerDay   int   `json:"max_queries_per_day"`
	MaxRowsPerQuery    int   `json:"max_rows_per_query"`
	MaxScanDurationSec int   `json:"max_scan_duration_sec"`
}

// GovernanceUsage mirrors Arc's per-token query usage response.
type GovernanceUsage struct {
	TokenID                   int64  `json:"token_id"`
	QueriesThisHour           int    `json:"queries_this_hour"`
	QueriesThisDay            int    `json:"queries_this_day"`
	HourResetAt               string `json:"hour_reset_at"`
	DayResetAt                string `json:"day_reset_at"`
	RateLimitRemainingPerMin  int    `json:"rate_limit_remaining_per_minute"`
	RateLimitRemainingPerHour int    `json:"rate_limit_remaining_per_hour"`
}

// ValidateGovernancePolicyRequest mirrors the server's token and limit
// validation. A zero limit means unlimited.
func ValidateGovernancePolicyRequest(req GovernancePolicyRequest) error {
	if req.TokenID <= 0 {
		return fmt.Errorf("token id must be a positive integer")
	}
	limits := []struct {
		name  string
		value int
	}{
		{"rate-limit-per-minute", req.RateLimitPerMinute},
		{"rate-limit-per-hour", req.RateLimitPerHour},
		{"max-queries-per-hour", req.MaxQueriesPerHour},
		{"max-queries-per-day", req.MaxQueriesPerDay},
		{"max-rows-per-query", req.MaxRowsPerQuery},
		{"max-scan-duration-sec", req.MaxScanDurationSec},
	}
	for _, limit := range limits {
		if limit.value < 0 {
			return fmt.Errorf("%s must not be negative", limit.name)
		}
	}
	return nil
}

// ListGovernancePolicies returns every policy. A null policies field is
// normalised to an empty slice.
func (c *Client) ListGovernancePolicies(ctx context.Context) ([]GovernancePolicy, error) {
	var out struct {
		Policies []GovernancePolicy `json:"policies"`
	}
	if _, err := c.featureJSON(ctx, governanceFeature, governanceConfigKey, http.MethodGet, governanceBase+"/policies", nil, 16<<20, &out); err != nil {
		return nil, err
	}
	if out.Policies == nil {
		out.Policies = []GovernancePolicy{}
	}
	return out.Policies, nil
}

// GetGovernancePolicy calls GET /api/v1/governance/policies/:token_id.
func (c *Client) GetGovernancePolicy(ctx context.Context, tokenID int64) (*GovernancePolicy, error) {
	if tokenID <= 0 {
		return nil, fmt.Errorf("token id must be a positive integer")
	}
	var out struct {
		Policy *GovernancePolicy `json:"policy"`
	}
	if _, err := c.featureJSON(ctx, governanceFeature, governanceConfigKey, http.MethodGet, governancePolicyPath(tokenID), nil, 1<<20, &out); err != nil {
		return nil, err
	}
	if out.Policy == nil {
		return nil, fmt.Errorf("decode response: policy is missing")
	}
	return out.Policy, nil
}

// CreateGovernancePolicy creates a policy for one API token.
func (c *Client) CreateGovernancePolicy(ctx context.Context, req GovernancePolicyRequest) (*GovernancePolicy, error) {
	if err := ValidateGovernancePolicyRequest(req); err != nil {
		return nil, err
	}
	var out struct {
		Policy *GovernancePolicy `json:"policy"`
	}
	if _, err := c.featureJSON(ctx, governanceFeature, governanceConfigKey, http.MethodPost, governanceBase+"/policies", req, 1<<20, &out); err != nil {
		return nil, err
	}
	if out.Policy == nil {
		return nil, fmt.Errorf("decode response: policy is missing")
	}
	return out.Policy, nil
}

// UpdateGovernancePolicy sends every field because Arc's PUT replaces the
// complete policy. The command layer fetches and merges the current values.
func (c *Client) UpdateGovernancePolicy(ctx context.Context, tokenID int64, req GovernancePolicyRequest) (*GovernancePolicy, error) {
	if tokenID <= 0 {
		return nil, fmt.Errorf("token id must be a positive integer")
	}
	req.TokenID = tokenID
	if err := ValidateGovernancePolicyRequest(req); err != nil {
		return nil, err
	}
	var out struct {
		Policy *GovernancePolicy `json:"policy"`
	}
	if _, err := c.featureJSON(ctx, governanceFeature, governanceConfigKey, http.MethodPut, governancePolicyPath(tokenID), req, 1<<20, &out); err != nil {
		return nil, err
	}
	if out.Policy == nil {
		return nil, fmt.Errorf("decode response: policy is missing")
	}
	return out.Policy, nil
}

// DeleteGovernancePolicy removes the policy for one token.
func (c *Client) DeleteGovernancePolicy(ctx context.Context, tokenID int64) error {
	if tokenID <= 0 {
		return fmt.Errorf("token id must be a positive integer")
	}
	_, err := c.featureJSON(ctx, governanceFeature, governanceConfigKey, http.MethodDelete, governancePolicyPath(tokenID), nil, 64<<10, nil)
	return err
}

// GetGovernanceUsage returns usage counters and the effective policy, if any.
func (c *Client) GetGovernanceUsage(ctx context.Context, tokenID int64) (*GovernanceUsage, *GovernancePolicy, error) {
	if tokenID <= 0 {
		return nil, nil, fmt.Errorf("token id must be a positive integer")
	}
	var out struct {
		Usage  *GovernanceUsage  `json:"usage"`
		Policy *GovernancePolicy `json:"policy"`
	}
	path := governanceBase + "/usage/" + strconv.FormatInt(tokenID, 10)
	if _, err := c.featureJSON(ctx, governanceFeature, governanceConfigKey, http.MethodGet, path, nil, 1<<20, &out); err != nil {
		return nil, nil, err
	}
	if out.Usage == nil {
		return nil, nil, fmt.Errorf("decode response: usage is missing")
	}
	return out.Usage, out.Policy, nil
}

func governancePolicyPath(tokenID int64) string {
	return governanceBase + "/policies/" + strconv.FormatInt(tokenID, 10)
}
