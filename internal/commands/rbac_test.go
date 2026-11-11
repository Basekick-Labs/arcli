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
	out, stderr, err := execCmd(t, root, "rbac", "organization", "list", "--output", "json")
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
	out, stderr, err := execCmd(t, root, "rbac", "organization", "update", "2", "--enabled=false", "--output", "json")
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
	out, stderr, err := execCmd(t, root, "rbac", "token", "team", "list", "release", "--output", "csv")
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
		name        string
		args        []string
		wantLookups int32
	}{
		{"organization", []string{"rbac", "organization", "delete", "2"}, 1},
		{"team", []string{"rbac", "team", "delete", "3"}, 1},
		{"role", []string{"rbac", "role", "delete", "4"}, 1},
		{"measurement permission", []string{"rbac", "measurement", "delete", "5"}, 0},
		{"token membership", []string{"rbac", "token", "team", "remove", "8", "3"}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				assertRBACRequest(t, r)
				if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/rbac/") {
					_, _ = w.Write([]byte(`{"organization":{"id":2,"name":"tenant"},"team":{"id":3,"name":"readers"},"role":{"id":4,"database_pattern":"metrics*"}}`))
					return
				}
				if tc.wantLookups > 0 && r.Method == http.MethodGet && r.URL.Path == "/api/v1/auth/tokens/8" {
					_, _ = w.Write([]byte(`{"token":{"id":8,"name":"ci","permissions":["read"],"enabled":true,"expires_at":null}}`))
					return
				}
				if tc.wantLookups > 0 && r.Method == http.MethodGet && r.URL.Path == "/api/v1/auth/tokens/8/teams" {
					_, _ = w.Write([]byte(`{"success":true,"teams":[{"id":3,"organization_id":2,"name":"readers","enabled":true}],"count":1}`))
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
			if got := requests.Load(); got != tc.wantLookups {
				t.Errorf("requests = %d, want %d", got, tc.wantLookups)
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
		warn   bool
	}{
		{"organization", []string{"rbac", "organization", "delete", "2"}, http.MethodDelete, "/api/v1/rbac/organizations/2", true},
		{"team", []string{"rbac", "team", "delete", "3"}, http.MethodDelete, "/api/v1/rbac/teams/3", true},
		// Deleting a role removes its grants with it; nothing widens.
		{"role", []string{"rbac", "role", "delete", "4"}, http.MethodDelete, "/api/v1/rbac/roles/4", false},
		{"measurement permission", []string{"rbac", "measurement", "delete", "5"}, http.MethodDelete, "/api/v1/rbac/measurement-permissions/5", true},
		{"token membership", []string{"rbac", "token", "team", "remove", "8", "3"}, http.MethodDelete, "/api/v1/auth/tokens/8/teams/3", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mutations atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assertRBACRequest(t, r)
				if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/rbac/") {
					_, _ = w.Write([]byte(`{"organization":{"id":2,"name":"tenant"},"team":{"id":3,"name":"readers"},"role":{"id":4,"database_pattern":"metrics*"}}`))
					return
				}
				if tc.name == "token membership" && r.Method == http.MethodGet && r.URL.Path == "/api/v1/auth/tokens/8" {
					_, _ = w.Write([]byte(`{"token":{"id":8,"name":"ci","permissions":["read"],"enabled":true,"expires_at":null}}`))
					return
				}
				if tc.name == "token membership" && r.Method == http.MethodGet && r.URL.Path == "/api/v1/auth/tokens/8/teams" {
					_, _ = w.Write([]byte(`{"success":true,"teams":[{"id":3,"organization_id":2,"name":"readers","enabled":true}],"count":1}`))
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
			if got := strings.Contains(stderr, "Warning:"); got != tc.warn {
				t.Errorf("warning printed = %v, want %v: %q", got, tc.warn, stderr)
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
	_, _, err := execCmd(t, root, "rbac", "team", "list", "0")
	if err == nil || !strings.Contains(err.Error(), "organization id must be a positive integer") {
		t.Fatalf("error = %v", err)
	}
}

func TestRBACSingularResources(t *testing.T) {
	for _, resource := range []string{"organization", "team", "role", "measurement", "token team"} {
		args := append([]string{"rbac"}, strings.Fields(resource)...)
		args = append(args, "list")
		cmd, _, err := NewRoot(BuildInfo{}).Find(args)
		if err != nil || cmd.CommandPath() != "arcli "+strings.Join(args, " ") {
			t.Fatalf("%v: %v %v", args, cmd, err)
		}
	}
	for _, old := range []string{"organizations", "teams", "roles", "measurements", "tokens"} {
		for _, cmd := range newRBACCmd().Commands() {
			if cmd.Name() == old {
				t.Errorf("legacy command %s registered", old)
			}
		}
	}
}

func TestRBACTimestampPrecision(t *testing.T) {
	const stamp = "2026-10-27T03:04:05.123456789+02:30"
	const utc = "2026-10-27T00:34:05Z"
	const utcNano = "2026-10-27T00:34:05.123456789Z"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"organizations":[{"id":2,"name":"test","enabled":true,"created_at":"` + stamp + `","updated_at":"` + stamp + `"},{"id":3,"name":"unset","enabled":true,"created_at":"0001-01-01T00:00:00Z"}]}`))
	}))
	defer srv.Close()
	writeTestConfig(t, srv.URL, "admin-secret")
	for _, nano := range []bool{false, true} {
		for _, format := range []string{"table", "csv", "json"} {
			args := []string{"rbac", "organization", "list", "-o", format}
			want := utc
			if nano {
				args = append(args, "--time-output", "rfc3339nano")
				want = utcNano
			}
			if format == "json" {
				want = stamp
			}
			out, _, err := execCmd(t, NewRoot(BuildInfo{}), args...)
			if err != nil || !strings.Contains(out, want) {
				t.Errorf("%s/nano=%t: out=%q err=%v", format, nano, out, err)
			}
			if !nano && format != "json" && strings.Contains(out, ".123456789") {
				t.Errorf("%s: default output shows fractional seconds: %q", format, out)
			}
			switch format {
			case "table":
				if strings.Count(out, "never") != 2 {
					t.Errorf("table: unset timestamps should read never: %q", out)
				}
			case "csv":
				if !strings.Contains(out, "\n3,unset,,true,,\n") || strings.Contains(out, "never") {
					t.Errorf("csv: unset timestamps should be empty fields: %q", out)
				}
			}
		}
	}
}
