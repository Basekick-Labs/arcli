package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"
)

const (
	backupFeature   = "backups"
	backupConfigKey = "backup.enabled"
	backupBase      = "/api/v1/backup/"
)

// backupIDRe mirrors arc/internal/api/backup_routes.go#validBackupID.
var backupIDRe = regexp.MustCompile(`^backup-\d{8}-\d{6}-[a-f0-9]{8}$`)

// ValidateBackupID rejects ids the server would answer with 400.
func ValidateBackupID(id string) error {
	if !backupIDRe.MatchString(id) {
		return fmt.Errorf("invalid backup id %q (expected backup-YYYYMMDD-HHMMSS-xxxxxxxx)", id)
	}
	return nil
}

// BackupBusyError is the server's 409: one backup/restore/delete slot.
type BackupBusyError struct {
	Operation string
}

func (e *BackupBusyError) Error() string {
	return fmt.Sprintf("a backup or restore operation is already in progress (%s); wait for it to finish", e.Operation)
}

// BackupSummary is one entry of GET /api/v1/backup/.
type BackupSummary struct {
	BackupID      string    `json:"backup_id"`
	CreatedAt     time.Time `json:"created_at"`
	BackupType    string    `json:"backup_type"`
	TotalFiles    int64     `json:"total_files"`
	TotalBytes    int64     `json:"total_size_bytes"`
	DatabaseCount int       `json:"database_count"`
}

// BackupMeasurement / BackupDatabase / BackupManifest mirror
// arc/internal/backup/manifest.go.
type BackupMeasurement struct {
	Name      string `json:"name"`
	FileCount int    `json:"file_count"`
	SizeBytes int64  `json:"size_bytes"`
}

type BackupDatabase struct {
	Name         string              `json:"name"`
	Measurements []BackupMeasurement `json:"measurements"`
	FileCount    int                 `json:"file_count"`
	SizeBytes    int64               `json:"size_bytes"`
}

type BackupManifest struct {
	Version           string           `json:"version"`
	BackupID          string           `json:"backup_id"`
	CreatedAt         time.Time        `json:"created_at"`
	BackupType        string           `json:"backup_type"`
	Databases         []BackupDatabase `json:"databases"`
	TotalFiles        int64            `json:"total_files"`
	TotalSizeBytes    int64            `json:"total_size_bytes"`
	SkippedFiles      int64            `json:"skipped_files,omitempty"`
	HasMetadata       bool             `json:"has_metadata"`
	HasIcebergCatalog bool             `json:"has_iceberg_catalog,omitempty"`
	HasConfig         bool             `json:"has_config"`
	Raw               json.RawMessage  `json:"-"`
}

// BackupProgress is GET /api/v1/backup/status when an operation has run
// since the server started. It is sticky: a completed or failed
// operation stays visible until the next one starts.
type BackupProgress struct {
	Operation      string     `json:"operation"`
	BackupID       string     `json:"backup_id"`
	Status         string     `json:"status"`
	TotalFiles     int64      `json:"total_files"`
	ProcessedFiles int64      `json:"processed_files"`
	SkippedFiles   int64      `json:"skipped_files"`
	TotalBytes     int64      `json:"total_bytes"`
	ProcessedBytes int64      `json:"processed_bytes"`
	StartedAt      time.Time  `json:"started_at"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
	Error          string     `json:"error,omitempty"`
}

// BackupStatus is the decoded status endpoint: Idle when the server has
// not run an operation since boot, otherwise Progress.
type BackupStatus struct {
	Idle     bool
	Progress *BackupProgress
	Raw      json.RawMessage
}

// BackupCreateOptions is the body of POST /api/v1/backup/. Nil pointers
// take the server defaults (both true).
type BackupCreateOptions struct {
	IncludeMetadata *bool `json:"include_metadata,omitempty"`
	IncludeConfig   *bool `json:"include_config,omitempty"`
}

// RestoreOptions is the body of POST /api/v1/backup/restore. Confirm is
// always sent true by the client; the command layer owns the prompt.
type RestoreOptions struct {
	RestoreData     *bool
	RestoreMetadata *bool
	RestoreConfig   *bool
}

// RestoreStarted is the 202 body of POST /api/v1/backup/restore.
// RestartRequired / Staged are absent (false) when not applicable.
type RestoreStarted struct {
	Message         string          `json:"message"`
	BackupID        string          `json:"backup_id"`
	Status          string          `json:"status"`
	RestartRequired bool            `json:"restart_required"`
	Staged          bool            `json:"staged"`
	Raw             json.RawMessage `json:"-"`
}

// backupJSON wraps featureJSON with the 409 mapping the backup group uses.
func (c *Client) backupJSON(ctx context.Context, method, path string, reqBody any, limit int64, out any) ([]byte, error) {
	body, err := c.featureJSON(ctx, backupFeature, backupConfigKey, method, path, reqBody, limit, out)
	var he *HTTPError
	if errors.As(err, &he) && he.Status == http.StatusConflict {
		return nil, &BackupBusyError{Operation: conflictOperation(he)}
	}
	return body, err
}

// conflictOperation reads the 409 body's "operation" field
// (backup | restore | delete | unknown).
func conflictOperation(he *HTTPError) string {
	var b struct {
		Operation string `json:"operation"`
	}
	if err := json.Unmarshal(he.Body, &b); err == nil && b.Operation != "" {
		return scrubControls(b.Operation)
	}
	return "unknown operation"
}

// CreateBackup calls POST /api/v1/backup/ (admin). The server answers
// 202 without an id; call BackupStatus to learn it.
func (c *Client) CreateBackup(ctx context.Context, opts BackupCreateOptions) error {
	_, err := c.backupJSON(ctx, http.MethodPost, backupBase, opts, 64<<10, nil)
	return err
}

// ListBackups calls GET /api/v1/backup/ (admin). Never nil.
func (c *Client) ListBackups(ctx context.Context) ([]BackupSummary, json.RawMessage, error) {
	var out struct {
		Backups []BackupSummary `json:"backups"`
	}
	body, err := c.backupJSON(ctx, http.MethodGet, backupBase, nil, 16<<20, &out)
	if err != nil {
		return nil, nil, err
	}
	if out.Backups == nil {
		out.Backups = []BackupSummary{}
	}
	return out.Backups, body, nil
}

// GetBackup calls GET /api/v1/backup/:id (admin). 404 is the server's
// "Backup not found".
func (c *Client) GetBackup(ctx context.Context, id string) (*BackupManifest, error) {
	if err := ValidateBackupID(id); err != nil {
		return nil, err
	}
	var out BackupManifest
	body, err := c.backupJSON(ctx, http.MethodGet, backupBase+id, nil, 16<<20, &out)
	if err != nil {
		return nil, err
	}
	if out.Databases == nil {
		out.Databases = []BackupDatabase{}
	}
	for i := range out.Databases {
		if out.Databases[i].Measurements == nil {
			out.Databases[i].Measurements = []BackupMeasurement{}
		}
	}
	out.Raw = body
	return &out, nil
}

// BackupStatus calls GET /api/v1/backup/status (admin).
func (c *Client) BackupStatus(ctx context.Context) (*BackupStatus, error) {
	body, err := c.backupJSON(ctx, http.MethodGet, backupBase+"status", nil, 1<<20, nil)
	if err != nil {
		return nil, err
	}
	var probe struct {
		Status    string `json:"status"`
		Operation string `json:"operation"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, fmt.Errorf("decode backup status: %w", err)
	}
	st := &BackupStatus{Raw: body}
	if probe.Operation == "" && probe.Status == "idle" {
		st.Idle = true
		return st, nil
	}
	var p BackupProgress
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("decode backup progress: %w", err)
	}
	st.Progress = &p
	return st, nil
}

// DeleteBackup calls DELETE /api/v1/backup/:id (admin). The server
// answers 500 for an unknown id; callers should GetBackup first.
func (c *Client) DeleteBackup(ctx context.Context, id string) error {
	if err := ValidateBackupID(id); err != nil {
		return err
	}
	_, err := c.backupJSON(ctx, http.MethodDelete, backupBase+id, nil, 64<<10, nil)
	return err
}

// RestoreBackup calls POST /api/v1/backup/restore (admin) with
// confirm:true. The server does not verify the id exists before
// answering 202; callers should GetBackup first.
func (c *Client) RestoreBackup(ctx context.Context, id string, opts RestoreOptions) (*RestoreStarted, error) {
	if err := ValidateBackupID(id); err != nil {
		return nil, err
	}
	req := struct {
		BackupID        string `json:"backup_id"`
		RestoreData     *bool  `json:"restore_data,omitempty"`
		RestoreMetadata *bool  `json:"restore_metadata,omitempty"`
		RestoreConfig   *bool  `json:"restore_config,omitempty"`
		Confirm         bool   `json:"confirm"`
	}{id, opts.RestoreData, opts.RestoreMetadata, opts.RestoreConfig, true}
	var out RestoreStarted
	body, err := c.backupJSON(ctx, http.MethodPost, backupBase+"restore", req, 64<<10, &out)
	if err != nil {
		return nil, err
	}
	out.Raw = body
	return &out, nil
}
