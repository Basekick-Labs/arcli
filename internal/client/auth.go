package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// TokenInfo is Arc's public view of an API token. Mirrors
// arc/internal/auth/auth.go#TokenInfo. It NEVER carries the secret or
// a prefix of it — the secret exists on the wire exactly twice: in the
// CreateToken and RotateToken responses.
type TokenInfo struct {
	ID          int64      `json:"id"`
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	Permissions []string   `json:"permissions"`
	CreatedAt   time.Time  `json:"created_at"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	Enabled     bool       `json:"enabled"`
	ExpiresAt   *time.Time `json:"expires_at"`
}

// IsExpired reports whether the token has a past expiry. A nil
// ExpiresAt never expires.
func (t TokenInfo) IsExpired(now time.Time) bool {
	return t.ExpiresAt != nil && !t.ExpiresAt.IsZero() && t.ExpiresAt.Before(now)
}

// ValidPermissions is the server's permission enum
// (arc/internal/auth/rbac_manager.go#IsValidPermission). Mirrored so
// a typo fails client-side before any HTTP.
var ValidPermissions = []string{"read", "write", "delete", "admin"}

// IsValidPermission reports whether p is one of ValidPermissions.
func IsValidPermission(p string) bool {
	for _, v := range ValidPermissions {
		if p == v {
			return true
		}
	}
	return false
}

// EffectivePermission is one row of GET /api/v1/auth/tokens/:id/permissions.
// On an OSS server there is exactly one row: database "*", source "token".
type EffectivePermission struct {
	Database    string   `json:"database"`
	Measurement string   `json:"measurement,omitempty"`
	Permissions []string `json:"permissions"`
	Source      string   `json:"source"`
}

// TokenPermissionsResponse is the response body of
// GET /api/v1/auth/tokens/:id/permissions.
type TokenPermissionsResponse struct {
	Permissions []EffectivePermission `json:"permissions"`
	RBACEnabled bool                  `json:"rbac_enabled"`
}

// VerifyResponse is the 200 body of GET /api/v1/auth/verify.
type VerifyResponse struct {
	Valid     bool      `json:"valid"`
	TokenInfo TokenInfo `json:"token_info"`
}

// CreateTokenOptions is the request body of POST /api/v1/auth/tokens.
//
// Permissions semantics follow the server exactly: nil means "server
// default" (read,write); a non-nil empty slice means "no OSS
// permissions" (RBAC-only token).
type CreateTokenOptions struct {
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Permissions *[]string `json:"permissions,omitempty"`
	ExpiresIn   string    `json:"expires_in,omitempty"`
}

// UpdateTokenOptions is the request body of PATCH /api/v1/auth/tokens/:id.
// Every field is a pointer: nil = leave unchanged. Permissions follows
// the same nil / empty-slice convention as CreateTokenOptions. The
// server has no way to CLEAR an expiry once set.
type UpdateTokenOptions struct {
	Name        *string   `json:"name,omitempty"`
	Description *string   `json:"description,omitempty"`
	Permissions *[]string `json:"permissions,omitempty"`
	ExpiresIn   *string   `json:"expires_in,omitempty"`
}

// IsEmpty reports whether the update would change nothing.
func (o UpdateTokenOptions) IsEmpty() bool {
	return o.Name == nil && o.Description == nil && o.Permissions == nil && o.ExpiresIn == nil
}

// ParseExpiresIn validates the `expires_in` grammar the server accepts
// (arc/internal/api/auth_routes.go#createToken): a Go duration such as
// "24h" / "90m", or an integer day count with a "d" suffix such as
// "7d". The result must be positive — the server accepts "0" and
// "-1h" and would happily mint an already-expired token.
func ParseExpiresIn(s string) (time.Duration, error) {
	if s == "" {
		return 0, fmt.Errorf("expires-in must not be empty")
	}
	if d, err := time.ParseDuration(s); err == nil {
		if d <= 0 {
			return 0, fmt.Errorf("expires-in must be positive (got %s)", s)
		}
		return d, nil
	}
	if n := len(s); n > 1 && s[n-1] == 'd' {
		days, err := strconv.Atoi(s[:n-1])
		if err == nil {
			if days <= 0 {
				return 0, fmt.Errorf("expires-in must be positive (got %s)", s)
			}
			return time.Duration(days) * 24 * time.Hour, nil
		}
	}
	return 0, fmt.Errorf("invalid expires-in %q (use a duration like 24h, 90m, or a day count like 7d)", s)
}

// authJSON performs one JSON request against an /api/v1/auth path and
// decodes the response body into out on 2xx (out may be nil). A non-2xx
// status becomes an *HTTPError carrying the server's message verbatim.
func (c *Client) authJSON(ctx context.Context, method, path string, reqBody any, limit int64, out any) error {
	var body io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.Endpoint+path, body)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Token admin is inherently cross-database; never send x-arc-database.
	c.setCrossDBHeaders(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return decodeWriteError(resp.StatusCode, respBody)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func tokenPath(id int64) string {
	return "/api/v1/auth/tokens/" + strconv.FormatInt(id, 10)
}

// Verify calls GET /api/v1/auth/verify with the client's own token and
// returns the server's view of it. A revoked, expired, or unknown token
// yields an HTTP 401 error ("Invalid or expired token"). On a server
// with authentication disabled the route does not exist and the error
// is an HTTP 404.
func (c *Client) Verify(ctx context.Context) (*TokenInfo, error) {
	var out VerifyResponse
	if err := c.authJSON(ctx, http.MethodGet, "/api/v1/auth/verify", nil, 1<<20, &out); err != nil {
		return nil, err
	}
	if !out.Valid {
		return nil, fmt.Errorf("arc: token reported invalid")
	}
	if out.TokenInfo.Permissions == nil {
		out.TokenInfo.Permissions = []string{}
	}
	return &out.TokenInfo, nil
}

// ListTokens returns every token on the server (enabled and revoked
// alike; the server does no filtering or paging). The result is never
// nil: the server encodes an empty table as `null`, which we normalise
// to an empty slice so callers and JSON output see `[]`.
func (c *Client) ListTokens(ctx context.Context) ([]TokenInfo, error) {
	var out struct {
		Tokens []TokenInfo `json:"tokens"`
	}
	if err := c.authJSON(ctx, http.MethodGet, "/api/v1/auth/tokens", nil, 16<<20, &out); err != nil {
		return nil, err
	}
	if out.Tokens == nil {
		out.Tokens = []TokenInfo{}
	}
	for i := range out.Tokens {
		if out.Tokens[i].Permissions == nil {
			out.Tokens[i].Permissions = []string{}
		}
	}
	return out.Tokens, nil
}

// GetToken returns one token by numeric id. HTTP 404 → error
// "Token not found" verbatim from the server.
func (c *Client) GetToken(ctx context.Context, id int64) (*TokenInfo, error) {
	var out struct {
		Token TokenInfo `json:"token"`
	}
	if err := c.authJSON(ctx, http.MethodGet, tokenPath(id), nil, 1<<20, &out); err != nil {
		return nil, err
	}
	if out.Token.Permissions == nil {
		out.Token.Permissions = []string{}
	}
	return &out.Token, nil
}

// GetTokenPermissions returns the effective (resolved) permissions for
// a token. The server returns `null` for a token with no OSS
// permissions and RBAC off; normalised to an empty slice.
func (c *Client) GetTokenPermissions(ctx context.Context, id int64) (*TokenPermissionsResponse, error) {
	var out TokenPermissionsResponse
	if err := c.authJSON(ctx, http.MethodGet, tokenPath(id)+"/permissions", nil, 1<<20, &out); err != nil {
		return nil, err
	}
	if out.Permissions == nil {
		out.Permissions = []EffectivePermission{}
	}
	for i := range out.Permissions {
		if out.Permissions[i].Permissions == nil {
			out.Permissions[i].Permissions = []string{}
		}
	}
	return &out, nil
}

// CreateToken mints a new token and returns its plaintext secret. The
// secret is shown by the server exactly once; the response does NOT
// include the new token's id (callers that need it list-by-name).
func (c *Client) CreateToken(ctx context.Context, opts CreateTokenOptions) (secret string, err error) {
	if opts.Name == "" {
		return "", fmt.Errorf("token name is required")
	}
	if opts.Permissions != nil {
		for _, p := range *opts.Permissions {
			if !IsValidPermission(p) {
				return "", fmt.Errorf("invalid permission %q (valid: read, write, delete, admin)", p)
			}
		}
	}
	if opts.ExpiresIn != "" {
		if _, err := ParseExpiresIn(opts.ExpiresIn); err != nil {
			return "", err
		}
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := c.authJSON(ctx, http.MethodPost, "/api/v1/auth/tokens", opts, 1<<20, &out); err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", fmt.Errorf("arc: create token response carried no token")
	}
	return out.Token, nil
}

// UpdateToken patches the mutable fields of a token. Refuses an empty
// update client-side so a no-op never round-trips.
func (c *Client) UpdateToken(ctx context.Context, id int64, opts UpdateTokenOptions) error {
	if opts.IsEmpty() {
		return fmt.Errorf("nothing to update")
	}
	if opts.Permissions != nil {
		for _, p := range *opts.Permissions {
			if !IsValidPermission(p) {
				return fmt.Errorf("invalid permission %q (valid: read, write, delete, admin)", p)
			}
		}
	}
	if opts.ExpiresIn != nil {
		if _, err := ParseExpiresIn(*opts.ExpiresIn); err != nil {
			return err
		}
	}
	return c.authJSON(ctx, http.MethodPatch, tokenPath(id), opts, 1<<20, nil)
}

// RotateToken replaces the token's secret and returns the new
// plaintext. The previous secret stops working immediately. Id, name,
// permissions, expiry and enabled state are all preserved — rotating a
// revoked or expired token yields a secret that can never authenticate.
func (c *Client) RotateToken(ctx context.Context, id int64) (secret string, err error) {
	var out struct {
		NewToken string `json:"new_token"`
	}
	if err := c.authJSON(ctx, http.MethodPost, tokenPath(id)+"/rotate", nil, 1<<20, &out); err != nil {
		return "", err
	}
	if out.NewToken == "" {
		return "", fmt.Errorf("arc: rotate response carried no new_token")
	}
	return out.NewToken, nil
}

// RevokeToken disables a token (enabled=false). The row survives and
// still lists; it can never be re-enabled through the API.
func (c *Client) RevokeToken(ctx context.Context, id int64) error {
	return c.authJSON(ctx, http.MethodPost, tokenPath(id)+"/revoke", nil, 64<<10, nil)
}

// DeleteToken removes a token row entirely. The server does not refuse
// deleting the caller's own token or the last admin token; the command
// layer warns.
func (c *Client) DeleteToken(ctx context.Context, id int64) error {
	return c.authJSON(ctx, http.MethodDelete, tokenPath(id), nil, 64<<10, nil)
}
