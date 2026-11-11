package commands

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRBACReviewPermissionSlices(t *testing.T) {
	for _, args := range [][]string{
		{"role", "create", "3", "--database-pattern", "metrics*"},
		{"role", "update", "4"},
		{"measurement", "create", "4", "--measurement-pattern", "cpu"},
	} {
		t.Run(strings.Join(args[:2], "-"), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Permissions []string `json:"permissions"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if strings.Join(body.Permissions, ",") != "read,write,delete" {
					t.Errorf("permissions = %v", body.Permissions)
				}
				w.Write([]byte(`{"role":{"id":4},"measurement_permission":{"id":5}}`))
			}))
			defer srv.Close()
			writeTestConfig(t, srv.URL, "admin-secret")
			argv := append([]string{"rbac"}, args...)
			argv = append(argv, "--permission", "read,write", "--permission", "delete,read")
			if _, _, err := execCmd(t, NewRoot(BuildInfo{}), argv...); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRBACReviewDeleteWarningsAndNames(t *testing.T) {
	cases := []struct {
		args  []string
		wants []string
	}{
		{[]string{"organization", "delete", "2"}, []string{`organization "tenant"`, "measurement permissions", "only team", "coarse token permissions", "HTTP 409", "cluster.rbac.max_cascade_descendants"}},
		{[]string{"team", "delete", "3"}, []string{`team "readers"`, "measurement permissions", "only team", "HTTP 409"}},
		{[]string{"role", "delete", "4"}, []string{"role id 4", `database pattern "metrics*"`, "measurement permissions"}},
		{[]string{"measurement", "delete", "5"}, []string{"last measurement permission", "widens", "all measurements"}},
		{[]string{"token", "team", "remove", "8", "3"}, []string{`token "ci"`, `team "readers" (id 3)`, "last team", "coarse token permissions (read,write)", "widen access"}},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, "-"), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("mutation after decline: %s", r.Method)
				}
				w.Write([]byte(`{"organization":{"id":2,"name":"tenant"},"team":{"id":3,"name":"readers"},"role":{"id":4,"team_id":3,"database_pattern":"metrics*"},"token":{"id":8,"name":"ci","permissions":["read","write"]},"teams":[{"id":3,"organization_id":2,"name":"readers","enabled":true}]}`))
			}))
			defer srv.Close()
			writeTestConfig(t, srv.URL, "admin-secret")
			root := NewRoot(BuildInfo{})
			root.SetIn(strings.NewReader("\n"))
			_, stderr, err := execCmd(t, root, append([]string{"rbac"}, tc.args...)...)
			if err != errAborted {
				t.Fatalf("error = %v", err)
			}
			for _, want := range append(tc.wants, "[y/N]") {
				if !strings.Contains(stderr, want) {
					t.Errorf("missing %q in %q", want, stderr)
				}
			}
			// The warning is printed once, not repeated inside the prompt.
			if n := strings.Count(stderr, "Warning:"); n > 1 || strings.Count(stderr, "widen") > 1 {
				t.Errorf("warning repeated in %q", stderr)
			}
			if tc.args[0] == "role" && strings.Contains(stderr, "HTTP 409") {
				t.Errorf("role delete has no cascade cap in Arc: %q", stderr)
			}
		})
	}
}

func TestRBACReviewAdminTokenNameAndSingleClient(t *testing.T) {
	for _, format := range []string{"table", "json"} {
		t.Run(format, func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method + " " + r.URL.Path {
				case "GET /api/v1/auth/tokens/42":
					w.Write([]byte(`{"token":{"id":42,"name":"resolved-admin","permissions":["admin"]}}`))
				case "POST /api/v1/auth/tokens/42/teams":
					w.Write([]byte(`{"membership":{"id":6,"token_id":42,"team_id":3}}`))
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
			}))
			defer srv.Close()
			writeTestConfig(t, srv.URL, "admin-secret")
			out, stderr, err := execCmd(t, NewRoot(BuildInfo{}), "rbac", "token", "team", "add", "42", "3", "--insecure", "-o", format)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out, "resolved-admin") {
				t.Errorf("missing resolved name: %s", out)
			}
			if !strings.Contains(stderr, "does not restrict its access") {
				t.Errorf("missing admin warning: %s", stderr)
			}
			if strings.Count(stderr, "TLS certificate verification") != 1 {
				t.Errorf("expected one TLS warning: %q", stderr)
			}
		})
	}
}

func TestRBACReviewHelp(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"organization", "update"}, "currently does not affect access"},
		{[]string{"measurement", "create"}, "Restrict a role"},
		{[]string{"organization", "show"}, "team count"},
		{[]string{"team", "show"}, "role count"},
		{[]string{"role", "show"}, "measurement-permission count"},
		{[]string{"role", "create"}, "not cluster administration"},
	} {
		argv := append([]string{"rbac"}, tc.args...)
		argv = append(argv, "--help")
		out, _, err := execCmd(t, NewRoot(BuildInfo{}), argv...)
		if err != nil || !strings.Contains(out, tc.want) {
			t.Errorf("%v: %v %s", argv, err, out)
		}
	}
}

// Human confirmation must not consume the subsequent mutation's request budget.
type delayedRBACConfirmation struct{ done bool }

func (r *delayedRBACConfirmation) Read(p []byte) (int, error) {
	if !r.done {
		time.Sleep(200 * time.Millisecond)
		r.done = true
	}
	return strings.NewReader("yes\n").Read(p)
}
func TestRBACDeleteUsesFreshDeadlineAfterConfirmation(t *testing.T) {
	for _, resource := range []string{"organization", "team", "role"} {
		t.Run(resource, func(t *testing.T) {
			deletes := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					deletes++
					_, _ = w.Write([]byte(`{"success":true}`))
					return
				}
				_, _ = w.Write([]byte(`{"organization":{"id":1,"name":"org"},"team":{"id":1,"name":"team"},"role":{"id":1,"database_pattern":"metrics"}}`))
			}))
			defer srv.Close()
			writeTestConfig(t, srv.URL, "admin")
			root := NewRoot(BuildInfo{})
			root.SetIn(&delayedRBACConfirmation{})
			_, _, err := execCmd(t, root, "rbac", resource, "delete", "1", "--timeout", "100ms")
			if err != nil || deletes != 1 {
				t.Fatalf("error=%v deletes=%d", err, deletes)
			}
		})
	}
}

// Removing a membership widens access only when it is the token's last team and
// the token's coarse permissions grant something (Arc checkPermissionUncached
// falls back to them; coarse admin bypasses RBAC already). The warning must
// fire in exactly that case.
func TestRBACTokenTeamRemoveWarnsOnlyWhenAccessWidens(t *testing.T) {
	const (
		readers = `{"id":3,"organization_id":2,"name":"readers","enabled":true,"created_at":"2026-11-11T22:42:29Z"}`
		writers = `{"id":5,"organization_id":2,"name":"writers","enabled":false,"created_at":"2026-11-11T22:42:29Z"}`
	)
	cases := []struct {
		name        string
		permissions string
		teams       string // empty: the membership read fails
		want        string // empty: no warning
	}{
		{"last team", `["read","write"]`, `[` + readers + `]`, `Warning: this is the token's last team; it falls back to its coarse token permissions (read,write)`},
		{"last team, read only", `["read"]`, `[` + readers + `]`, `coarse token permissions (read)`},
		{"another team remains", `["read","write"]`, `[` + readers + `,` + writers + `]`, ""},
		{"not a member", `["read","write"]`, `[` + writers + `]`, ""},
		{"no coarse permissions", `[]`, `[` + readers + `]`, ""},
		{"coarse admin", `["admin"]`, `[` + readers + `]`, ""},
		{"membership read fails", `["read"]`, "", "Warning: could not read the token's teams"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deletes := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method + " " + r.URL.Path {
				case "GET /api/v1/auth/tokens/8":
					w.Write([]byte(`{"success":true,"token":{"id":8,"name":"ci","permissions":` + tc.permissions + `,"enabled":true,"expires_at":null}}`))
				case "GET /api/v1/auth/tokens/8/teams":
					if tc.teams == "" {
						w.WriteHeader(http.StatusInternalServerError)
						w.Write([]byte(`{"success":false,"error":"Failed to get token teams: failed to get token teams: database is locked"}`))
						return
					}
					w.Write([]byte(`{"success":true,"teams":` + tc.teams + `,"count":1}`))
				case "DELETE /api/v1/auth/tokens/8/teams/3":
					deletes++
					w.Write([]byte(`{"success":true,"message":"Token removed from team successfully"}`))
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
			}))
			defer srv.Close()
			writeTestConfig(t, srv.URL, "admin-secret")
			_, stderr, err := execCmd(t, NewRoot(BuildInfo{}), "rbac", "token", "team", "remove", "8", "3", "--yes")
			if err != nil {
				t.Fatal(err)
			}
			if deletes != 1 {
				t.Fatalf("deletes = %d, want 1", deletes)
			}
			if tc.want == "" {
				if stderr != "" {
					t.Errorf("unexpected warning: %q", stderr)
				}
			} else if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr = %q, want %q", stderr, tc.want)
			}
		})
	}
}

func TestRBACTokenTeamRemoveUsesFreshDeadlineAfterConfirmation(t *testing.T) {
	deletes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/auth/tokens/8":
			_, _ = w.Write([]byte(`{"success":true,"token":{"id":8,"name":"ci","permissions":["read"],"enabled":true}}`))
		case "GET /api/v1/auth/tokens/8/teams":
			_, _ = w.Write([]byte(`{"success":true,"teams":[{"id":3,"organization_id":2,"name":"readers","enabled":true}],"count":1}`))
		case "DELETE /api/v1/auth/tokens/8/teams/3":
			deletes++
			_, _ = w.Write([]byte(`{"success":true}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	writeTestConfig(t, srv.URL, "admin")
	root := NewRoot(BuildInfo{})
	root.SetIn(&delayedRBACConfirmation{})
	_, _, err := execCmd(t, root, "rbac", "token", "team", "remove", "8", "3", "--timeout", "100ms")
	if err != nil || deletes != 1 {
		t.Fatalf("error=%v deletes=%d", err, deletes)
	}
}
