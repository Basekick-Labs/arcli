package commands

import (
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func assertRBACRequest(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != "Bearer admin-secret" {
		t.Errorf("Authorization = %q", got)
	}
	if got := r.Header.Get("x-arc-database"); got != "" {
		t.Errorf("x-arc-database = %q, want no database header", got)
	}
}

func TestRBACOrganizationListJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertRBACRequest(t, r)
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/rbac/organizations" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"organizations":[
			{"id":9,"name":"zeta","enabled":true},
			{"id":2,"name":"alpha","enabled":false}
		],"count":2}`))
	}))
	defer srv.Close()
	writeTestConfig(t, srv.URL, "admin-secret")

	root := NewRoot(BuildInfo{Version: "test"})
	out, stderr, err := execCmd(t, root, "rbac", "organizations", "list", "--output", "json")
	if err != nil {
		t.Fatal(err)
	}
	if stderr != "" {
		t.Errorf("stderr = %q", stderr)
	}
	var got struct {
		Organizations []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"organizations"`
		Count int `json:"count"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode output: %v\n%s", err, out)
	}
	if got.Count != 2 || len(got.Organizations) != 2 || got.Organizations[0].ID != 2 || got.Organizations[1].ID != 9 {
		t.Errorf("output = %+v", got)
	}
}

func TestRBACOrganizationUpdateJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertRBACRequest(t, r)
		if r.Method != http.MethodPatch || r.URL.Path != "/api/v1/rbac/organizations/2" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["enabled"] != false {
			t.Errorf("request body = %#v, want enabled=false", body)
		}
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer srv.Close()
	writeTestConfig(t, srv.URL, "admin-secret")

	root := NewRoot(BuildInfo{Version: "test"})
	out, stderr, err := execCmd(t, root, "rbac", "organizations", "update", "2", "--enabled=false", "--output", "json")
	if err != nil {
		t.Fatal(err)
	}
	if stderr != "" {
		t.Errorf("stderr = %q", stderr)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("decode output: %v\n%s", err, out)
	}
	if result["id"] != float64(2) || result["updated"] != true {
		t.Errorf("output = %#v", result)
	}
}

func TestRBACTokenTeamsResolvesExactTokenName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertRBACRequest(t, r)
		switch r.URL.Path {
		case "/api/v1/auth/tokens":
			if r.Method != http.MethodGet {
				t.Errorf("token list method = %s", r.Method)
			}
			_, _ = w.Write([]byte(`{"tokens":[
				{"id":8,"name":"build","permissions":["read"],"enabled":true,"expires_at":null},
				{"id":9,"name":"release","permissions":["read"],"enabled":true,"expires_at":null}
			]}`))
		case "/api/v1/auth/tokens/9/teams":
			if r.Method != http.MethodGet {
				t.Errorf("teams method = %s", r.Method)
			}
			_, _ = w.Write([]byte(`{"teams":[{"id":3,"organization_id":2,"name":"release team","enabled":true}]}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	writeTestConfig(t, srv.URL, "admin-secret")

	root := NewRoot(BuildInfo{Version: "test"})
	out, stderr, err := execCmd(t, root, "rbac", "tokens", "teams", "list", "release", "--output", "csv")
	if err != nil {
		t.Fatal(err)
	}
	if stderr != "" {
		t.Errorf("stderr = %q", stderr)
	}
	rows, err := csv.NewReader(strings.NewReader(out)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[1][0] != "3" || rows[1][2] != "release team" {
		t.Errorf("CSV = %#v", rows)
	}
}

func TestRBACDestructiveCommandsNeedConfirmation(t *testing.T) {
	cases := []struct {
		name             string
		args             []string
		wantLookupBefore bool
	}{
		{"organization", []string{"rbac", "organizations", "delete", "2"}, false},
		{"team", []string{"rbac", "teams", "delete", "3"}, false},
		{"role", []string{"rbac", "roles", "delete", "4"}, false},
		{"measurement permission", []string{"rbac", "measurements", "delete", "5"}, false},
		{"token membership", []string{"rbac", "tokens", "teams", "remove", "8", "3"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				assertRBACRequest(t, r)
				if tc.wantLookupBefore && r.Method == http.MethodGet && r.URL.Path == "/api/v1/auth/tokens/8" {
					_, _ = w.Write([]byte(`{"token":{"id":8,"name":"ci","permissions":["read"],"enabled":true,"expires_at":null}}`))
					return
				}
				t.Errorf("destructive request without confirmation: %s %s", r.Method, r.URL.Path)
			}))
			defer srv.Close()
			writeTestConfig(t, srv.URL, "admin-secret")
			root := NewRoot(BuildInfo{Version: "test"})
			root.SetIn(confirmationInput(t, "pipe"))
			_, _, err := execCmd(t, root, tc.args...)
			const want = "confirmation required but stdin is not a terminal; pass --yes"
			if err == nil || err.Error() != want {
				t.Fatalf("error = %v, want %q", err, want)
			}
			wantRequests := int32(0)
			if tc.wantLookupBefore {
				wantRequests = 1
			}
			if got := requests.Load(); got != wantRequests {
				t.Errorf("requests = %d, want %d", got, wantRequests)
			}
		})
	}
}

func TestRBACDestructiveCommandsAcceptYes(t *testing.T) {
	cases := []struct {
		name   string
		args   []string
		method string
		path   string
	}{
		{"organization", []string{"rbac", "organizations", "delete", "2"}, http.MethodDelete, "/api/v1/rbac/organizations/2"},
		{"team", []string{"rbac", "teams", "delete", "3"}, http.MethodDelete, "/api/v1/rbac/teams/3"},
		{"role", []string{"rbac", "roles", "delete", "4"}, http.MethodDelete, "/api/v1/rbac/roles/4"},
		{"measurement permission", []string{"rbac", "measurements", "delete", "5"}, http.MethodDelete, "/api/v1/rbac/measurement-permissions/5"},
		{"token membership", []string{"rbac", "tokens", "teams", "remove", "8", "3"}, http.MethodDelete, "/api/v1/auth/tokens/8/teams/3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mutations atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assertRBACRequest(t, r)
				if tc.name == "token membership" && r.Method == http.MethodGet && r.URL.Path == "/api/v1/auth/tokens/8" {
					_, _ = w.Write([]byte(`{"token":{"id":8,"name":"ci","permissions":["read"],"enabled":true,"expires_at":null}}`))
					return
				}
				if r.Method != tc.method || r.URL.Path != tc.path {
					t.Errorf("request = %s %s, want %s %s", r.Method, r.URL.Path, tc.method, tc.path)
				}
				mutations.Add(1)
				_, _ = w.Write([]byte(`{"success":true}`))
			}))
			defer srv.Close()
			writeTestConfig(t, srv.URL, "admin-secret")
			root := NewRoot(BuildInfo{Version: "test"})
			root.SetIn(confirmationInput(t, "pipe"))
			args := append(append([]string{}, tc.args...), "--yes")
			out, stderr, err := execCmd(t, root, args...)
			if err != nil {
				t.Fatal(err)
			}
			if stderr != "" {
				t.Errorf("stderr = %q", stderr)
			}
			if out == "" {
				t.Error("expected confirmation of the completed action")
			}
			if got := mutations.Load(); got != 1 {
				t.Errorf("mutation requests = %d, want 1", got)
			}
		})
	}
}

func TestRBACInvalidIDDoesNotReachServer(t *testing.T) {
	writeTestConfig(t, unreachable, "admin")
	root := NewRoot(BuildInfo{Version: "test"})
	_, _, err := execCmd(t, root, "rbac", "teams", "list", "0")
	if err == nil || !strings.Contains(err.Error(), "organization id must be a positive integer") {
		t.Fatalf("error = %v", err)
	}
}
