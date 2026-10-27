package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const rbacPath = "/api/v1/rbac"

// Organization is an Arc Enterprise RBAC organization.
type Organization struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	Enabled     bool      `json:"enabled"`
	Teams       []Team    `json:"teams,omitempty"`
}

// Team is an RBAC team within an organization.
type Team struct {
	ID             int64     `json:"id"`
	OrganizationID int64     `json:"organization_id"`
	Name           string    `json:"name"`
	Description    string    `json:"description,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	Enabled        bool      `json:"enabled"`
	Roles          []Role    `json:"roles,omitempty"`
}

// Role grants permissions over a database pattern to a team.
type Role struct {
	ID                     int64                   `json:"id"`
	TeamID                 int64                   `json:"team_id"`
	DatabasePattern        string                  `json:"database_pattern"`
	Permissions            []string                `json:"permissions"`
	CreatedAt              time.Time               `json:"created_at"`
	MeasurementPermissions []MeasurementPermission `json:"measurement_permissions,omitempty"`
}

// MeasurementPermission narrows a role's grants to measurement patterns.
type MeasurementPermission struct {
	ID                 int64     `json:"id"`
	RoleID             int64     `json:"role_id"`
	MeasurementPattern string    `json:"measurement_pattern"`
	Permissions        []string  `json:"permissions"`
	CreatedAt          time.Time `json:"created_at"`
}

// TokenMembership links an API token to an RBAC team.
type TokenMembership struct {
	ID        int64     `json:"id"`
	TokenID   int64     `json:"token_id"`
	TeamID    int64     `json:"team_id"`
	CreatedAt time.Time `json:"created_at"`
}

type CreateOrganizationOptions struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type UpdateOrganizationOptions struct {
	Name        *string `json:"name,omitempty"`
	Description *string `json:"description,omitempty"`
	Enabled     *bool   `json:"enabled,omitempty"`
}

func (o UpdateOrganizationOptions) IsEmpty() bool {
	return o.Name == nil && o.Description == nil && o.Enabled == nil
}

type CreateTeamOptions struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type UpdateTeamOptions struct {
	Name        *string `json:"name,omitempty"`
	Description *string `json:"description,omitempty"`
	Enabled     *bool   `json:"enabled,omitempty"`
}

func (o UpdateTeamOptions) IsEmpty() bool {
	return o.Name == nil && o.Description == nil && o.Enabled == nil
}

type CreateRoleOptions struct {
	DatabasePattern string   `json:"database_pattern"`
	Permissions     []string `json:"permissions"`
}

type UpdateRoleOptions struct {
	DatabasePattern *string   `json:"database_pattern,omitempty"`
	Permissions     *[]string `json:"permissions,omitempty"`
}

func (o UpdateRoleOptions) IsEmpty() bool {
	return o.DatabasePattern == nil && o.Permissions == nil
}

type CreateMeasurementPermissionOptions struct {
	MeasurementPattern string   `json:"measurement_pattern"`
	Permissions        []string `json:"permissions"`
}

// rbacJSON performs one authenticated JSON request against an admin API
// endpoint. These routes are cross-database and must not inherit the
// connection's default database header.
func (c *Client) rbacJSON(ctx context.Context, method, path string, reqBody any, out any) error {
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
	c.setCrossDBHeaders(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return decodeWriteError(resp.StatusCode, respBody)
	}
	if out == nil || len(respBody) == 0 {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func rbacID(kind string, id int64) error {
	if id <= 0 {
		return fmt.Errorf("%s id must be a positive integer (got %d)", kind, id)
	}
	return nil
}

func rbacIDPath(path string, id int64) string {
	return path + "/" + strconv.FormatInt(id, 10)
}

func validateRolePermissions(perms []string) error {
	if len(perms) == 0 {
		return fmt.Errorf("at least one permission is required")
	}
	for _, p := range perms {
		if !IsValidPermission(p) {
			return fmt.Errorf("invalid permission %q (valid: read, write, delete, admin)", p)
		}
	}
	return nil
}

func (c *Client) ListOrganizations(ctx context.Context) ([]Organization, error) {
	var out struct {
		Organizations []Organization `json:"organizations"`
	}
	if err := c.rbacJSON(ctx, http.MethodGet, rbacPath+"/organizations", nil, &out); err != nil {
		return nil, err
	}
	if out.Organizations == nil {
		out.Organizations = []Organization{}
	}
	return out.Organizations, nil
}

func (c *Client) GetOrganization(ctx context.Context, id int64) (*Organization, error) {
	if err := rbacID("organization", id); err != nil {
		return nil, err
	}
	var out struct {
		Organization Organization `json:"organization"`
	}
	if err := c.rbacJSON(ctx, http.MethodGet, rbacIDPath(rbacPath+"/organizations", id), nil, &out); err != nil {
		return nil, err
	}
	return &out.Organization, nil
}

func (c *Client) CreateOrganization(ctx context.Context, opts CreateOrganizationOptions) (*Organization, error) {
	if strings.TrimSpace(opts.Name) == "" {
		return nil, fmt.Errorf("organization name is required")
	}
	var out struct {
		Organization Organization `json:"organization"`
	}
	if err := c.rbacJSON(ctx, http.MethodPost, rbacPath+"/organizations", opts, &out); err != nil {
		return nil, err
	}
	return &out.Organization, nil
}

func (c *Client) UpdateOrganization(ctx context.Context, id int64, opts UpdateOrganizationOptions) error {
	if err := rbacID("organization", id); err != nil {
		return err
	}
	if opts.IsEmpty() {
		return fmt.Errorf("nothing to update")
	}
	return c.rbacJSON(ctx, http.MethodPatch, rbacIDPath(rbacPath+"/organizations", id), opts, nil)
}

func (c *Client) DeleteOrganization(ctx context.Context, id int64) error {
	if err := rbacID("organization", id); err != nil {
		return err
	}
	return c.rbacJSON(ctx, http.MethodDelete, rbacIDPath(rbacPath+"/organizations", id), nil, nil)
}

func (c *Client) ListTeams(ctx context.Context, organizationID int64) ([]Team, error) {
	if err := rbacID("organization", organizationID); err != nil {
		return nil, err
	}
	var out struct {
		Teams []Team `json:"teams"`
	}
	path := rbacIDPath(rbacPath+"/organizations", organizationID) + "/teams"
	if err := c.rbacJSON(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	if out.Teams == nil {
		out.Teams = []Team{}
	}
	return out.Teams, nil
}

func (c *Client) GetTeam(ctx context.Context, id int64) (*Team, error) {
	if err := rbacID("team", id); err != nil {
		return nil, err
	}
	var out struct {
		Team Team `json:"team"`
	}
	if err := c.rbacJSON(ctx, http.MethodGet, rbacIDPath(rbacPath+"/teams", id), nil, &out); err != nil {
		return nil, err
	}
	return &out.Team, nil
}

func (c *Client) CreateTeam(ctx context.Context, organizationID int64, opts CreateTeamOptions) (*Team, error) {
	if err := rbacID("organization", organizationID); err != nil {
		return nil, err
	}
	if strings.TrimSpace(opts.Name) == "" {
		return nil, fmt.Errorf("team name is required")
	}
	var out struct {
		Team Team `json:"team"`
	}
	path := rbacIDPath(rbacPath+"/organizations", organizationID) + "/teams"
	if err := c.rbacJSON(ctx, http.MethodPost, path, opts, &out); err != nil {
		return nil, err
	}
	return &out.Team, nil
}

func (c *Client) UpdateTeam(ctx context.Context, id int64, opts UpdateTeamOptions) error {
	if err := rbacID("team", id); err != nil {
		return err
	}
	if opts.IsEmpty() {
		return fmt.Errorf("nothing to update")
	}
	return c.rbacJSON(ctx, http.MethodPatch, rbacIDPath(rbacPath+"/teams", id), opts, nil)
}

func (c *Client) DeleteTeam(ctx context.Context, id int64) error {
	if err := rbacID("team", id); err != nil {
		return err
	}
	return c.rbacJSON(ctx, http.MethodDelete, rbacIDPath(rbacPath+"/teams", id), nil, nil)
}

func (c *Client) ListRoles(ctx context.Context, teamID int64) ([]Role, error) {
	if err := rbacID("team", teamID); err != nil {
		return nil, err
	}
	var out struct {
		Roles []Role `json:"roles"`
	}
	path := rbacIDPath(rbacPath+"/teams", teamID) + "/roles"
	if err := c.rbacJSON(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	if out.Roles == nil {
		out.Roles = []Role{}
	}
	for i := range out.Roles {
		if out.Roles[i].Permissions == nil {
			out.Roles[i].Permissions = []string{}
		}
	}
	return out.Roles, nil
}

func (c *Client) GetRole(ctx context.Context, id int64) (*Role, error) {
	if err := rbacID("role", id); err != nil {
		return nil, err
	}
	var out struct {
		Role Role `json:"role"`
	}
	if err := c.rbacJSON(ctx, http.MethodGet, rbacIDPath(rbacPath+"/roles", id), nil, &out); err != nil {
		return nil, err
	}
	if out.Role.Permissions == nil {
		out.Role.Permissions = []string{}
	}
	return &out.Role, nil
}

func (c *Client) CreateRole(ctx context.Context, teamID int64, opts CreateRoleOptions) (*Role, error) {
	if err := rbacID("team", teamID); err != nil {
		return nil, err
	}
	if strings.TrimSpace(opts.DatabasePattern) == "" {
		return nil, fmt.Errorf("database pattern is required")
	}
	if err := validateRolePermissions(opts.Permissions); err != nil {
		return nil, err
	}
	var out struct {
		Role Role `json:"role"`
	}
	path := rbacIDPath(rbacPath+"/teams", teamID) + "/roles"
	if err := c.rbacJSON(ctx, http.MethodPost, path, opts, &out); err != nil {
		return nil, err
	}
	if out.Role.Permissions == nil {
		out.Role.Permissions = []string{}
	}
	return &out.Role, nil
}

func (c *Client) UpdateRole(ctx context.Context, id int64, opts UpdateRoleOptions) error {
	if err := rbacID("role", id); err != nil {
		return err
	}
	if opts.IsEmpty() {
		return fmt.Errorf("nothing to update")
	}
	if opts.DatabasePattern != nil && strings.TrimSpace(*opts.DatabasePattern) == "" {
		return fmt.Errorf("database pattern is required")
	}
	if opts.Permissions != nil {
		if err := validateRolePermissions(*opts.Permissions); err != nil {
			return err
		}
	}
	return c.rbacJSON(ctx, http.MethodPatch, rbacIDPath(rbacPath+"/roles", id), opts, nil)
}

func (c *Client) DeleteRole(ctx context.Context, id int64) error {
	if err := rbacID("role", id); err != nil {
		return err
	}
	return c.rbacJSON(ctx, http.MethodDelete, rbacIDPath(rbacPath+"/roles", id), nil, nil)
}

func (c *Client) ListMeasurementPermissions(ctx context.Context, roleID int64) ([]MeasurementPermission, error) {
	if err := rbacID("role", roleID); err != nil {
		return nil, err
	}
	var out struct {
		Permissions []MeasurementPermission `json:"measurement_permissions"`
	}
	path := rbacIDPath(rbacPath+"/roles", roleID) + "/measurements"
	if err := c.rbacJSON(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	if out.Permissions == nil {
		out.Permissions = []MeasurementPermission{}
	}
	for i := range out.Permissions {
		if out.Permissions[i].Permissions == nil {
			out.Permissions[i].Permissions = []string{}
		}
	}
	return out.Permissions, nil
}

func (c *Client) CreateMeasurementPermission(ctx context.Context, roleID int64, opts CreateMeasurementPermissionOptions) (*MeasurementPermission, error) {
	if err := rbacID("role", roleID); err != nil {
		return nil, err
	}
	if strings.TrimSpace(opts.MeasurementPattern) == "" {
		return nil, fmt.Errorf("measurement pattern is required")
	}
	if err := validateRolePermissions(opts.Permissions); err != nil {
		return nil, err
	}
	var out struct {
		Permission MeasurementPermission `json:"measurement_permission"`
	}
	path := rbacIDPath(rbacPath+"/roles", roleID) + "/measurements"
	if err := c.rbacJSON(ctx, http.MethodPost, path, opts, &out); err != nil {
		return nil, err
	}
	if out.Permission.Permissions == nil {
		out.Permission.Permissions = []string{}
	}
	return &out.Permission, nil
}

func (c *Client) DeleteMeasurementPermission(ctx context.Context, id int64) error {
	if err := rbacID("measurement permission", id); err != nil {
		return err
	}
	return c.rbacJSON(ctx, http.MethodDelete, rbacIDPath(rbacPath+"/measurement-permissions", id), nil, nil)
}

func (c *Client) ListTokenTeams(ctx context.Context, tokenID int64) ([]Team, error) {
	if err := rbacID("token", tokenID); err != nil {
		return nil, err
	}
	var out struct {
		Teams []Team `json:"teams"`
	}
	path := rbacIDPath("/api/v1/auth/tokens", tokenID) + "/teams"
	if err := c.rbacJSON(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	if out.Teams == nil {
		out.Teams = []Team{}
	}
	return out.Teams, nil
}

func (c *Client) AddTokenToTeam(ctx context.Context, tokenID, teamID int64) (*TokenMembership, error) {
	if err := rbacID("token", tokenID); err != nil {
		return nil, err
	}
	if err := rbacID("team", teamID); err != nil {
		return nil, err
	}
	var out struct {
		Membership TokenMembership `json:"membership"`
	}
	path := rbacIDPath("/api/v1/auth/tokens", tokenID) + "/teams"
	if err := c.rbacJSON(ctx, http.MethodPost, path, struct {
		TeamID int64 `json:"team_id"`
	}{teamID}, &out); err != nil {
		return nil, err
	}
	return &out.Membership, nil
}

func (c *Client) RemoveTokenFromTeam(ctx context.Context, tokenID, teamID int64) error {
	if err := rbacID("token", tokenID); err != nil {
		return err
	}
	if err := rbacID("team", teamID); err != nil {
		return err
	}
	path := rbacIDPath("/api/v1/auth/tokens", tokenID) + "/teams/" + strconv.FormatInt(teamID, 10)
	return c.rbacJSON(ctx, http.MethodDelete, path, nil, nil)
}
