package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

const (
	tieringFeature   = "tiering endpoints"
	tieringConfigKey = "tiered_storage.enabled"
	tieringBase      = "/api/v1/tiering"
)

// TieringStatus mirrors GET /api/v1/tiering/status.
type TieringStatus struct {
	Enabled           bool                    `json:"enabled"`
	LicenseValid      bool                    `json:"license_valid"`
	Reason            string                  `json:"reason,omitempty"`
	Tiers             map[string]TieringStats `json:"tiers,omitempty"`
	QuarantinedFiles  int64                   `json:"quarantined_files"`
	Scheduler         *TieringSchedulerStatus `json:"scheduler,omitempty"`
	ReplicationEvents *TieringEventCounts     `json:"replication_events,omitempty"`
	LastScan          *TieringScanResult      `json:"last_scan,omitempty"`
	LastScanAt        *string                 `json:"last_scan_at,omitempty"`
	Raw               json.RawMessage         `json:"-"`
}

// TieringStats is one tier's entry in a status or statistics response.
type TieringStats struct {
	Tier        string `json:"tier"`
	Enabled     bool   `json:"enabled"`
	Backend     string `json:"backend"`
	FileCount   int64  `json:"file_count"`
	TotalSizeMB int64  `json:"total_size_mb"`
}

// TieringSchedulerStatus describes the tier migration scheduler.
type TieringSchedulerStatus struct {
	Running   bool    `json:"running"`
	Schedule  string  `json:"schedule"`
	NextRun   *string `json:"next_run,omitempty"`
	LastRun   *string `json:"last_run,omitempty"`
	RoleGated bool    `json:"role_gated"`
}

// TieringEventCounts reports metadata replication events.
type TieringEventCounts struct {
	Applied int64 `json:"applied"`
	Dropped int64 `json:"dropped"`
	Failed  int64 `json:"failed"`
}

// TieringScanResult is returned by POST /api/v1/tiering/scan and embedded
// in status responses.
type TieringScanResult struct {
	FilesScanned    int  `json:"files_scanned"`
	FilesRegistered int  `json:"files_registered"`
	FilesSkipped    int  `json:"files_skipped"`
	Errors          int  `json:"errors"`
	ColdSynced      int  `json:"cold_synced"`
	ColdSyncFailed  bool `json:"cold_sync_failed,omitempty"`
	HotRetired      int  `json:"hot_retired"`
	HotRowsWritten  int  `json:"hot_rows_written"`
	Truncated       bool `json:"truncated,omitempty"`
}

// TieringFile is one file returned by GET /api/v1/tiering/files.
type TieringFile struct {
	ID               int64   `json:"id"`
	Path             string  `json:"path"`
	Database         string  `json:"database"`
	Measurement      string  `json:"measurement"`
	PartitionTime    string  `json:"partition_time"`
	Tier             string  `json:"tier"`
	SizeBytes        int64   `json:"size_bytes"`
	CreatedAt        string  `json:"created_at"`
	MigratedAt       *string `json:"migrated_at,omitempty"`
	QuarantinedAt    *string `json:"quarantined_at,omitempty"`
	QuarantineReason string  `json:"quarantine_reason,omitempty"`
}

// TieringMigrationRecord is one recent migration in GET /api/v1/tiering/stats.
type TieringMigrationRecord struct {
	ID          int64   `json:"id"`
	FilePath    string  `json:"file_path"`
	Database    string  `json:"database"`
	FromTier    string  `json:"from_tier"`
	ToTier      string  `json:"to_tier"`
	SizeBytes   int64   `json:"size_bytes"`
	StartedAt   string  `json:"started_at"`
	CompletedAt *string `json:"completed_at,omitempty"`
	Error       string  `json:"error,omitempty"`
}

// TieringMigrationStats is the body of GET /api/v1/tiering/stats.
type TieringMigrationStats struct {
	TierStats        []TieringStats           `json:"tier_stats"`
	RecentMigrations []TieringMigrationRecord `json:"recent_migrations"`
	Raw              json.RawMessage          `json:"-"`
}

// TieringMigrationRequest is POST /api/v1/tiering/migrate. Arc currently
// supports only hot-to-cold migration; empty tiers use those server defaults.
type TieringMigrationRequest struct {
	FromTier    string `json:"from_tier,omitempty"`
	ToTier      string `json:"to_tier,omitempty"`
	Database    string `json:"database,omitempty"`
	Measurement string `json:"measurement,omitempty"`
	DryRun      bool   `json:"dry_run,omitempty"`
}

// TieringMigrationResult carries the server's dry-run or completed response.
type TieringMigrationResult struct {
	Status       string          `json:"status"`
	Message      string          `json:"message,omitempty"`
	WouldMigrate *int            `json:"would_migrate,omitempty"`
	Migrated     *int            `json:"migrated,omitempty"`
	Errors       *int            `json:"errors,omitempty"`
	TotalBytes   *int64          `json:"total_bytes,omitempty"`
	FromTier     string          `json:"from_tier,omitempty"`
	ToTier       string          `json:"to_tier,omitempty"`
	Raw          json.RawMessage `json:"-"`
}

// TieringPolicy is a per-database tiering policy.
type TieringPolicy struct {
	ID            int64  `json:"id"`
	Database      string `json:"database"`
	HotOnly       bool   `json:"hot_only"`
	HotMaxAgeDays *int   `json:"hot_max_age_days"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

// TieringPolicyRequest is the body of PUT /api/v1/tiering/policies/:database.
type TieringPolicyRequest struct {
	HotOnly       bool `json:"hot_only,omitempty"`
	HotMaxAgeDays *int `json:"hot_max_age_days,omitempty"`
}

// TieringPolicyList is the body of GET /api/v1/tiering/policies.
type TieringPolicyList struct {
	Policies []TieringPolicy `json:"policies"`
	Count    int             `json:"count"`
	Raw      json.RawMessage `json:"-"`
}

// TieringPolicyMutation is returned by policy set and delete endpoints.
type TieringPolicyMutation struct {
	Message   string          `json:"message"`
	Database  string          `json:"database"`
	Policy    *TieringPolicy  `json:"policy,omitempty"`
	ColdFiles json.RawMessage `json:"cold_files,omitempty"`
	Raw       json.RawMessage `json:"-"`
}

// EffectiveTieringPolicy is the resolved policy for a database.
type EffectiveTieringPolicy struct {
	Database      string `json:"database"`
	HotOnly       bool   `json:"hot_only"`
	HotMaxAgeDays int    `json:"hot_max_age_days"`
	Source        string `json:"source"`
}

func tieringPolicyPath(database string) string {
	return tieringBase + "/policies/" + url.PathEscape(database)
}

// TieringStatus calls GET /api/v1/tiering/status.
func (c *Client) TieringStatus(ctx context.Context) (*TieringStatus, error) {
	var out TieringStatus
	body, err := c.featureJSON(ctx, tieringFeature, tieringConfigKey, http.MethodGet, tieringBase+"/status", nil, 4<<20, &out)
	if err != nil {
		return nil, err
	}
	out.Raw = body
	return &out, nil
}

// ListTieringFiles calls GET /api/v1/tiering/files. limit is always sent so
// callers can keep output bounded; zero retains Arc's documented empty-page
// behaviour. Arc currently exposes no offset or cursor parameter.
func (c *Client) ListTieringFiles(ctx context.Context, tier, database string, limit int) ([]TieringFile, json.RawMessage, error) {
	if tier != "" && tier != "hot" && tier != "cold" {
		return nil, nil, fmt.Errorf("invalid tier %q (valid: hot, cold)", tier)
	}
	if tier != "" && database != "" {
		return nil, nil, fmt.Errorf("--tier and --database cannot be combined: Arc applies the database filter first and ignores tier")
	}
	if database != "" {
		if err := ValidateDatabaseName(database); err != nil {
			return nil, nil, err
		}
	}
	if limit < 0 {
		return nil, nil, fmt.Errorf("limit must not be negative")
	}
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	if tier != "" {
		q.Set("tier", tier)
	}
	if database != "" {
		q.Set("database", database)
	}
	var out struct {
		Files []TieringFile `json:"files"`
		Count int           `json:"count"`
	}
	body, err := c.featureJSON(ctx, tieringFeature, tieringConfigKey, http.MethodGet, tieringBase+"/files?"+q.Encode(), nil, 16<<20, &out)
	if err != nil {
		return nil, nil, err
	}
	if out.Files == nil {
		out.Files = []TieringFile{}
		body = normalizeTieringArrayFields(body, "files")
	}
	return out.Files, body, nil
}

// TieringStats calls GET /api/v1/tiering/stats.
func (c *Client) TieringStats(ctx context.Context) (*TieringMigrationStats, error) {
	var out TieringMigrationStats
	body, err := c.featureJSON(ctx, tieringFeature, tieringConfigKey, http.MethodGet, tieringBase+"/stats", nil, 16<<20, &out)
	if err != nil {
		return nil, err
	}
	if out.TierStats == nil {
		out.TierStats = []TieringStats{}
		body = normalizeTieringArrayFields(body, "tier_stats")
	}
	if out.RecentMigrations == nil {
		out.RecentMigrations = []TieringMigrationRecord{}
		body = normalizeTieringArrayFields(body, "recent_migrations")
	}
	out.Raw = body
	return &out, nil
}

// MigrateTiering calls POST /api/v1/tiering/migrate.
func (c *Client) MigrateTiering(ctx context.Context, req TieringMigrationRequest) (*TieringMigrationResult, error) {
	if req.Database != "" {
		if err := ValidateDatabaseName(req.Database); err != nil {
			return nil, err
		}
	}
	if req.Measurement != "" {
		if err := ValidateMeasurementName(req.Measurement); err != nil {
			return nil, err
		}
	}
	if req.FromTier != "" && req.FromTier != "hot" {
		return nil, fmt.Errorf("invalid from-tier %q (Arc supports hot to cold migration)", req.FromTier)
	}
	if req.ToTier != "" && req.ToTier != "cold" {
		return nil, fmt.Errorf("invalid to-tier %q (Arc supports hot to cold migration)", req.ToTier)
	}
	var out TieringMigrationResult
	body, err := c.featureJSON(ctx, tieringFeature, tieringConfigKey, http.MethodPost, tieringBase+"/migrate", req, 4<<20, &out)
	if err != nil {
		return nil, err
	}
	out.Raw = body
	return &out, nil
}

// ScanTieringFiles calls POST /api/v1/tiering/scan.
func (c *Client) ScanTieringFiles(ctx context.Context) (*TieringScanResult, json.RawMessage, error) {
	var out TieringScanResult
	body, err := c.featureJSON(ctx, tieringFeature, tieringConfigKey, http.MethodPost, tieringBase+"/scan", nil, 4<<20, &out)
	if err != nil {
		return nil, nil, err
	}
	return &out, body, nil
}

// ListTieringPolicies calls GET /api/v1/tiering/policies/.
func (c *Client) ListTieringPolicies(ctx context.Context) (*TieringPolicyList, error) {
	var out TieringPolicyList
	body, err := c.featureJSON(ctx, tieringFeature, tieringConfigKey, http.MethodGet, tieringBase+"/policies/", nil, 4<<20, &out)
	if err != nil {
		return nil, err
	}
	if out.Policies == nil {
		out.Policies = []TieringPolicy{}
		body = normalizeTieringArrayFields(body, "policies")
	}
	out.Raw = body
	return &out, nil
}

// GetTieringPolicy calls GET /api/v1/tiering/policies/:database.
func (c *Client) GetTieringPolicy(ctx context.Context, database string) (*TieringPolicy, json.RawMessage, error) {
	if err := ValidateDatabaseName(database); err != nil {
		return nil, nil, err
	}
	var out TieringPolicy
	body, err := c.featureJSON(ctx, tieringFeature, tieringConfigKey, http.MethodGet, tieringPolicyPath(database), nil, 1<<20, &out)
	if err != nil {
		return nil, nil, err
	}
	return &out, body, nil
}

// SetTieringPolicy calls PUT /api/v1/tiering/policies/:database.
func (c *Client) SetTieringPolicy(ctx context.Context, database string, req TieringPolicyRequest) (*TieringPolicyMutation, error) {
	if err := ValidateDatabaseName(database); err != nil {
		return nil, err
	}
	if req.HotMaxAgeDays != nil && *req.HotMaxAgeDays < 0 {
		return nil, fmt.Errorf("hot-max-age-days must not be negative")
	}
	var out TieringPolicyMutation
	body, err := c.featureJSON(ctx, tieringFeature, tieringConfigKey, http.MethodPut, tieringPolicyPath(database), req, 1<<20, &out)
	if err != nil {
		return nil, err
	}
	out.Raw = body
	return &out, nil
}

// DeleteTieringPolicy calls DELETE /api/v1/tiering/policies/:database.
func (c *Client) DeleteTieringPolicy(ctx context.Context, database string) (*TieringPolicyMutation, error) {
	if err := ValidateDatabaseName(database); err != nil {
		return nil, err
	}
	var out TieringPolicyMutation
	body, err := c.featureJSON(ctx, tieringFeature, tieringConfigKey, http.MethodDelete, tieringPolicyPath(database), nil, 1<<20, &out)
	if err != nil {
		return nil, err
	}
	out.Raw = body
	return &out, nil
}

// EffectiveTieringPolicy calls GET /api/v1/tiering/policies/:database/effective.
func (c *Client) EffectiveTieringPolicy(ctx context.Context, database string) (*EffectiveTieringPolicy, json.RawMessage, error) {
	if err := ValidateDatabaseName(database); err != nil {
		return nil, nil, err
	}
	var out EffectiveTieringPolicy
	body, err := c.featureJSON(ctx, tieringFeature, tieringConfigKey, http.MethodGet, tieringPolicyPath(database)+"/effective", nil, 1<<20, &out)
	if err != nil {
		return nil, nil, err
	}
	return &out, body, nil
}

// normalizeTieringArrayFields keeps JSON list output useful when Arc encodes
// an empty collection as null, while retaining any fields added by newer Arc
// versions.
func normalizeTieringArrayFields(body []byte, fields ...string) []byte {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil || object == nil {
		return body
	}
	changed := false
	for _, field := range fields {
		raw, ok := object[field]
		if ok && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			object[field] = json.RawMessage("[]")
			changed = true
		}
	}
	if !changed {
		return body
	}
	normalized, err := json.Marshal(object)
	if err != nil {
		return body
	}
	return normalized
}
