package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestRBACEndpoints(t *testing.T) {
	type request struct {
		method   string
		path     string
		body     string
		response string
		status   int
	}
	requests := []request{
		{http.MethodGet, "/api/v1/rbac/organizations", "", `{"organizations":[]}`, http.StatusOK},
		{http.MethodGet, "/api/v1/rbac/organizations/2", "", `{"organization":{"id":2,"name":"ops","enabled":true}}`, http.StatusOK},
		{http.MethodPost, "/api/v1/rbac/organizations", `{"name":"ops","description":"operators"}`, `{"organization":{"id":2,"name":"ops","enabled":true}}`, http.StatusCreated},
		{http.MethodPatch, "/api/v1/rbac/organizations/2", `{"name":"new ops","description":"","enabled":false}`, `{}`, http.StatusOK},
		{http.MethodDelete, "/api/v1/rbac/organizations/2", "", `{}`, http.StatusOK},
		{http.MethodGet, "/api/v1/rbac/organizations/2/teams", "", `{"teams":[]}`, http.StatusOK},
		{http.MethodGet, "/api/v1/rbac/teams/3", "", `{"team":{"id":3,"organization_id":2,"name":"operators","enabled":true}}`, http.StatusOK},
		{http.MethodPost, "/api/v1/rbac/organizations/2/teams", `{"name":"operators","description":"on call"}`, `{"team":{"id":3,"organization_id":2,"name":"operators","enabled":true}}`, http.StatusCreated},
		{http.MethodPatch, "/api/v1/rbac/teams/3", `{"enabled":false}`, `{}`, http.StatusOK},
		{http.MethodDelete, "/api/v1/rbac/teams/3", "", `{}`, http.StatusOK},
		{http.MethodGet, "/api/v1/rbac/teams/3/roles", "", `{"roles":[{"id":4,"team_id":3,"database_pattern":"prod","permissions":null}]}`, http.StatusOK},
		{http.MethodGet, "/api/v1/rbac/roles/4", "", `{"role":{"id":4,"team_id":3,"database_pattern":"prod","permissions":["read"]}}`, http.StatusOK},
		{http.MethodPost, "/api/v1/rbac/teams/3/roles", `{"database_pattern":"prod_*","permissions":["read","write"]}`, `{"role":{"id":4,"team_id":3,"database_pattern":"prod_*","permissions":["read","write"]}}`, http.StatusCreated},
		{http.MethodPatch, "/api/v1/rbac/roles/4", `{"permissions":["read"]}`, `{}`, http.StatusOK},
		{http.MethodDelete, "/api/v1/rbac/roles/4", "", `{}`, http.StatusOK},
		{http.MethodGet, "/api/v1/rbac/roles/4/measurements", "", `{"measurement_permissions":[]}`, http.StatusOK},
		{http.MethodPost, "/api/v1/rbac/roles/4/measurements", `{"measurement_pattern":"cpu_*","permissions":["read"]}`, `{"measurement_permission":{"id":5,"role_id":4,"measurement_pattern":"cpu_*","permissions":["read"]}}`, http.StatusCreated},
		{http.MethodDelete, "/api/v1/rbac/measurement-permissions/5", "", `{}`, http.StatusOK},
		{http.MethodGet, "/api/v1/auth/tokens/8/teams", "", `{"teams":[{"id":3,"organization_id":2,"name":"operators"}]}`, http.StatusOK},
		{http.MethodPost, "/api/v1/auth/tokens/8/teams", `{"team_id":3}`, `{"membership":{"id":6,"token_id":8,"team_id":3}}`, http.StatusCreated},
		{http.MethodDelete, "/api/v1/auth/tokens/8/teams/3", "", `{}`, http.StatusOK},
	}
	i := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertAuthHeaders(t, r)
		if i >= len(requests) {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		want := requests[i]
		i++
		if r.Method != want.method || r.URL.Path != want.path {
			t.Errorf("request = %s %s, want %s %s", r.Method, r.URL.Path, want.method, want.path)
		}
		var gotBody any
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil && want.body != "" {
			t.Errorf("decode request body: %v", err)
		}
		if want.body != "" {
			var expected any
			if err := json.Unmarshal([]byte(want.body), &expected); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(gotBody, expected) {
				t.Errorf("body = %#v, want %#v", gotBody, expected)
			}
			if got := r.Header.Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q", got)
			}
		} else if gotBody != nil {
			t.Errorf("unexpected request body: %#v", gotBody)
		}
		w.WriteHeader(want.status)
		_, _ = w.Write([]byte(want.response))
	}))
	defer srv.Close()
	cli, err := New(Config{Endpoint: srv.URL, Token: "test-token", Database: "ignored_default"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	name := "new ops"
	description := ""
	disabled := false
	permissions := []string{"read"}

	callers := []struct {
		name string
		call func() error
	}{
		{"list organizations", func() error {
			got, err := cli.ListOrganizations(ctx)
			if err == nil && got == nil {
				t.Error("nil organizations")
			}
			return err
		}},
		{"get organization", func() error {
			got, err := cli.GetOrganization(ctx, 2)
			if err == nil && got.ID != 2 {
				t.Errorf("organization = %+v", got)
			}
			return err
		}},
		{"create organization", func() error {
			_, err := cli.CreateOrganization(ctx, CreateOrganizationOptions{Name: "ops", Description: "operators"})
			return err
		}},
		{"update organization", func() error {
			return cli.UpdateOrganization(ctx, 2, UpdateOrganizationOptions{Name: &name, Description: &description, Enabled: &disabled})
		}},
		{"delete organization", func() error { return cli.DeleteOrganization(ctx, 2) }},
		{"list teams", func() error { _, err := cli.ListTeams(ctx, 2); return err }},
		{"get team", func() error {
			got, err := cli.GetTeam(ctx, 3)
			if err == nil && got.OrganizationID != 2 {
				t.Errorf("team = %+v", got)
			}
			return err
		}},
		{"create team", func() error {
			_, err := cli.CreateTeam(ctx, 2, CreateTeamOptions{Name: "operators", Description: "on call"})
			return err
		}},
		{"update team", func() error { return cli.UpdateTeam(ctx, 3, UpdateTeamOptions{Enabled: &disabled}) }},
		{"delete team", func() error { return cli.DeleteTeam(ctx, 3) }},
		{"list roles", func() error {
			got, err := cli.ListRoles(ctx, 3)
			if err == nil && (len(got) != 1 || got[0].Permissions == nil) {
				t.Errorf("roles = %+v", got)
			}
			return err
		}},
		{"get role", func() error {
			got, err := cli.GetRole(ctx, 4)
			if err == nil && got.Permissions[0] != "read" {
				t.Errorf("role = %+v", got)
			}
			return err
		}},
		{"create role", func() error {
			_, err := cli.CreateRole(ctx, 3, CreateRoleOptions{DatabasePattern: "prod_*", Permissions: []string{"read", "write"}})
			return err
		}},
		{"update role", func() error { return cli.UpdateRole(ctx, 4, UpdateRoleOptions{Permissions: &permissions}) }},
		{"delete role", func() error { return cli.DeleteRole(ctx, 4) }},
		{"list measurement permissions", func() error { _, err := cli.ListMeasurementPermissions(ctx, 4); return err }},
		{"create measurement permission", func() error {
			_, err := cli.CreateMeasurementPermission(ctx, 4, CreateMeasurementPermissionOptions{MeasurementPattern: "cpu_*", Permissions: []string{"read"}})
			return err
		}},
		{"delete measurement permission", func() error { return cli.DeleteMeasurementPermission(ctx, 5) }},
		{"list token teams", func() error {
			got, err := cli.ListTokenTeams(ctx, 8)
			if err == nil && (len(got) != 1 || got[0].ID != 3) {
				t.Errorf("teams = %+v", got)
			}
			return err
		}},
		{"add token to team", func() error {
			got, err := cli.AddTokenToTeam(ctx, 8, 3)
			if err == nil && got.TeamID != 3 {
				t.Errorf("membership = %+v", got)
			}
			return err
		}},
		{"remove token from team", func() error { return cli.RemoveTokenFromTeam(ctx, 8, 3) }},
	}
	for _, tc := range callers {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); err != nil {
				t.Fatal(err)
			}
		})
	}
	if i != len(requests) {
		t.Fatalf("handled %d requests, want %d", i, len(requests))
	}
}

func TestRBACValidationDoesNotSendRequests(t *testing.T) {
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	})
	ctx := context.Background()
	empty := []string{}
	badPermission := []string{"root"}
	blank := "  "
	cases := []struct {
		name string
		call func() error
		want string
	}{
		{"organization id", func() error { _, err := cli.GetOrganization(ctx, 0); return err }, "positive integer"},
		{"blank organization name", func() error { _, err := cli.CreateOrganization(ctx, CreateOrganizationOptions{Name: " "}); return err }, "name is required"},
		{"empty organization update", func() error { return cli.UpdateOrganization(ctx, 1, UpdateOrganizationOptions{}) }, "nothing to update"},
		{"team id", func() error { _, err := cli.ListTeams(ctx, -1); return err }, "positive integer"},
		{"blank team name", func() error { _, err := cli.CreateTeam(ctx, 1, CreateTeamOptions{Name: ""}); return err }, "name is required"},
		{"empty team update", func() error { return cli.UpdateTeam(ctx, 1, UpdateTeamOptions{}) }, "nothing to update"},
		{"role id", func() error { _, err := cli.GetRole(ctx, 0); return err }, "positive integer"},
		{"blank database pattern", func() error {
			_, err := cli.CreateRole(ctx, 1, CreateRoleOptions{DatabasePattern: blank, Permissions: []string{"read"}})
			return err
		}, "database pattern is required"},
		{"empty role permissions", func() error { _, err := cli.CreateRole(ctx, 1, CreateRoleOptions{DatabasePattern: "prod"}); return err }, "at least one permission"},
		{"invalid role permission", func() error {
			_, err := cli.CreateRole(ctx, 1, CreateRoleOptions{DatabasePattern: "prod", Permissions: badPermission})
			return err
		}, "invalid permission"},
		{"empty role update", func() error { return cli.UpdateRole(ctx, 1, UpdateRoleOptions{}) }, "nothing to update"},
		{"empty role permission update", func() error { return cli.UpdateRole(ctx, 1, UpdateRoleOptions{Permissions: &empty}) }, "at least one permission"},
		{"measurement role id", func() error { _, err := cli.ListMeasurementPermissions(ctx, 0); return err }, "positive integer"},
		{"blank measurement pattern", func() error {
			_, err := cli.CreateMeasurementPermission(ctx, 1, CreateMeasurementPermissionOptions{MeasurementPattern: " ", Permissions: []string{"read"}})
			return err
		}, "measurement pattern is required"},
		{"invalid membership token id", func() error { _, err := cli.ListTokenTeams(ctx, 0); return err }, "positive integer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestRBACHTTPErrorPreservesStatusAndMessage(t *testing.T) {
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"success":false,"error":"Organization name already exists"}`))
	})
	_, err := cli.CreateOrganization(context.Background(), CreateOrganizationOptions{Name: "taken"})
	if err == nil {
		t.Fatal("expected conflict")
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != http.StatusConflict || httpErr.Message != "Organization name already exists" {
		t.Fatalf("error = %#v", err)
	}
}
