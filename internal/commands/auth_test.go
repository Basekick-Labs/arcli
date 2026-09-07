package commands

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/basekick-labs/arcli/internal/client"
	"github.com/basekick-labs/arcli/internal/config"
)

func tokenFixture(id int64, name string, enabled bool) client.TokenInfo {
	return client.TokenInfo{
		ID: id, Name: name, Permissions: []string{"read", "write"}, Enabled: enabled,
		CreatedAt: time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC),
	}
}

func TestRenderTokenList_EmptyTableAndJSON(t *testing.T) {
	var buf bytes.Buffer
	cmd := newTestCmd()
	cmd.SetOut(&buf)
	if err := renderTokenList(cmd, nil, "table", false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "(no tokens)") {
		t.Errorf("table = %q", buf.String())
	}
	buf.Reset()
	if err := renderTokenList(cmd, nil, "json", false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"tokens": []`) || strings.Contains(buf.String(), "null") {
		t.Errorf("json must encode [] not null: %s", buf.String())
	}
}

func TestRenderTokenList_SortedByIDWithNullableColumns(t *testing.T) {
	exp := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	b := tokenFixture(2, "b", false)
	b.ExpiresAt = &exp
	a := tokenFixture(1, "a", true)
	a.Permissions = nil // server never sends null here, but the renderer must not trip

	var buf bytes.Buffer
	cmd := newTestCmd()
	cmd.SetOut(&buf)
	if err := renderTokenList(cmd, []client.TokenInfo{b, a}, "csv", false); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("csv lines = %v", lines)
	}
	if !strings.HasPrefix(lines[1], "1,a,,true,,,2026-09-07T10:00:00Z") {
		t.Errorf("row 1 = %q (want sorted first, empty perms, empty expiry)", lines[1])
	}
	if !strings.HasPrefix(lines[2], "2,b,\"read,write\",false,2026-10-01T00:00:00Z,") {
		t.Errorf("row 2 = %q", lines[2])
	}

	buf.Reset()
	if err := renderTokenList(cmd, []client.TokenInfo{b, a}, "table", false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Index(out, "│ 1 ") > strings.Index(out, "│ 2 ") {
		t.Errorf("table not sorted by id:\n%s", out)
	}
	if !strings.Contains(out, "never") || !strings.Contains(out, "no permissions") {
		t.Errorf("table missing 'never' / 'no permissions' placeholders:\n%s", out)
	}
}

func TestWriteTokenInfo_NullableFields(t *testing.T) {
	var buf bytes.Buffer
	tok := tokenFixture(4, "svc", true)
	tok.Description = "desc"
	writeTokenInfo(&buf, &tok)
	out := buf.String()
	for _, want := range []string{"id:          4", "name:        svc", "description: desc", "permissions: read,write", "expires:     never", "last used:   never"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestNormalisePermissions(t *testing.T) {
	got := normalisePermissions([]string{" Read ", "write", "", "read", "ADMIN"})
	if strings.Join(got, ",") != "read,write,admin" {
		t.Errorf("got %v", got)
	}
}

// ---- flag validation: no network must be touched ----------------------------

func execCmd(t *testing.T, c *cobra.Command, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var o, e bytes.Buffer
	c.SetOut(&o)
	c.SetErr(&e)
	// The real root sets SilenceUsage/SilenceErrors and children inherit
	// at execution time; standalone subcommands must set them here so an
	// error doesn't append usage text to stdout.
	c.SilenceUsage = true
	c.SilenceErrors = true
	c.SetArgs(args)
	err = c.Execute()
	return o.String(), e.String(), err
}

// unreachable is an endpoint that fails fast if any command wrongly
// reaches the network before flag validation.
const unreachable = "http://127.0.0.1:1"

func TestAuthTokenCreate_FlagValidation(t *testing.T) {
	t.Setenv("ARCLI_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"--endpoint", unreachable, "--token", "t"}, `required flag(s) "name" not set`},
		{[]string{"--name", "x", "--permission", "root", "--endpoint", unreachable, "--token", "t"}, `invalid permission "root"`},
		{[]string{"--name", "x", "--permission", "read", "--no-permissions", "--endpoint", unreachable, "--token", "t"}, "mutually exclusive"},
		{[]string{"--name", "x", "--expires-in", "1w", "--endpoint", unreachable, "--token", "t"}, "invalid expires-in"},
		{[]string{"--name", "x", "-o", "csv", "--endpoint", unreachable, "--token", "t"}, "invalid --output"},
		// pflag turns `--permission ,` into ["",""] and `--permission ""` into
		// []; neither may silently become the RBAC-only empty set.
		{[]string{"--name", "x", "--permission", ",", "--endpoint", unreachable, "--token", "t"}, "no permission names"},
		{[]string{"--name", "x", "--permission", "", "--endpoint", unreachable, "--token", "t"}, "no permission names"},
	}
	for _, tc := range cases {
		_, _, err := execCmd(t, newAuthTokenCreateCmd(), tc.args...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("args %v: err = %v, want %q", tc.args, err, tc.want)
		}
	}
}

func TestAuthTokenUpdate_NothingToUpdate(t *testing.T) {
	t.Setenv("ARCLI_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	_, _, err := execCmd(t, newAuthTokenUpdateCmd(), "7", "--endpoint", unreachable, "--token", "t")
	if err == nil || !strings.Contains(err.Error(), "nothing to update") {
		t.Errorf("err = %v", err)
	}
	_, _, err = execCmd(t, newAuthTokenUpdateCmd(), "7", "--name", "", "--endpoint", unreachable, "--token", "t")
	if err == nil || !strings.Contains(err.Error(), "--name must not be empty") {
		t.Errorf("err = %v", err)
	}
	_, _, err = execCmd(t, newAuthTokenUpdateCmd(), "7", "--permission", "", "--endpoint", unreachable, "--token", "t")
	if err == nil || !strings.Contains(err.Error(), "no permission names") {
		t.Errorf("empty --permission must not clear permissions: err = %v", err)
	}
}

func TestAuthTokenRotate_SaveRefusedForAdHocConnection(t *testing.T) {
	t.Setenv("ARCLI_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	t.Setenv("ARC_CONNECTION", "")
	t.Setenv("ARC_ENDPOINT", "")
	t.Setenv("ARC_TOKEN", "")
	// --endpoint/--token resolve to the "(flags)" sentinel; --save must be
	// refused before any HTTP (endpoint is unreachable, so a network
	// attempt would surface as a dial error instead).
	_, _, err := execCmd(t, newAuthTokenRotateCmd(), "1", "--save", "--yes", "--endpoint", unreachable, "--token", "t")
	if err == nil || !strings.Contains(err.Error(), "--save requires a named connection profile") {
		t.Errorf("err = %v", err)
	}
}

func TestConfirmOrAbort(t *testing.T) {
	cmd := newTestCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)

	cmd.SetIn(strings.NewReader("y\n"))
	if err := confirmOrAbort(cmd, "Go?", false); err != nil {
		t.Errorf("y: %v", err)
	}
	if !strings.Contains(errOut.String(), "Go? [y/N]") || out.Len() != 0 {
		t.Errorf("prompt must be on stderr only: out=%q err=%q", out.String(), errOut.String())
	}
	cmd.SetIn(strings.NewReader("n\n"))
	if err := confirmOrAbort(cmd, "Go?", false); err != errAborted {
		t.Errorf("n: err = %v", err)
	}
	cmd.SetIn(strings.NewReader(""))
	if err := confirmOrAbort(cmd, "Go?", false); err != errAborted {
		t.Errorf("EOF: err = %v", err)
	}
	if err := confirmOrAbort(cmd, "Go?", true); err != nil {
		t.Errorf("--yes: %v", err)
	}
	// A real non-terminal *os.File on stdin without --yes must refuse.
	f, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cmd.SetIn(f)
	if err := confirmOrAbort(cmd, "Go?", false); err == nil || !strings.Contains(err.Error(), "stdin is not a terminal") {
		t.Errorf("file stdin: err = %v", err)
	}
	// /dev/null is a char device but nobody is there to answer.
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	cmd.SetIn(devnull)
	if err := confirmOrAbort(cmd, "Go?", false); err == nil || !strings.Contains(err.Error(), "stdin is not a terminal") {
		t.Errorf("/dev/null stdin: err = %v", err)
	}
}

// ---- end-to-end against an httptest fake of the auth routes -----------------

// fakeAuthServer emulates the subset of Arc's auth routes the commands
// use, with an in-memory token table. Token id 1 is the caller.
func fakeAuthServer(t *testing.T) (*httptest.Server, map[int64]*client.TokenInfo) {
	t.Helper()
	tokens := map[int64]*client.TokenInfo{1: ptr(tokenFixture(1, "admin", true))}
	tokens[1].Permissions = []string{"read", "write", "delete", "admin"}
	var nextID int64 = 2
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok","time":"2026-09-07T00:00:00Z","uptime":"1s","uptime_sec":1}`))
	})
	mux.HandleFunc("/api/v1/auth/verify", func(w http.ResponseWriter, r *http.Request) {
		// Accept the rotated secret too: tests that --save and then run a
		// second command authenticate with the new value from the config.
		if a := r.Header.Get("Authorization"); a != "Bearer admin-secret" && a != "Bearer rotated-secret==" {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"valid":false,"error":"Invalid or expired token"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"valid": true, "token_info": tokens[1]})
	})
	mux.HandleFunc("/api/v1/auth/tokens", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			list := []client.TokenInfo{}
			for _, tk := range tokens {
				list = append(list, *tk)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "tokens": list, "count": len(list)})
		case http.MethodPost:
			var req client.CreateTokenOptions
			_ = json.NewDecoder(r.Body).Decode(&req)
			tk := tokenFixture(nextID, req.Name, true)
			nextID++
			tokens[tk.ID] = &tk
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"success":true,"token":"new-secret==","message":"m"}`))
		}
	})
	mux.HandleFunc("/api/v1/auth/tokens/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/v1/auth/tokens/")
		parts := strings.SplitN(rest, "/", 2)
		var id int64
		for _, ch := range parts[0] {
			id = id*10 + int64(ch-'0')
		}
		tk, ok := tokens[id]
		if !ok {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"success":false,"error":"Token not found"}`))
			return
		}
		action := ""
		if len(parts) == 2 {
			action = parts[1]
		}
		switch {
		case r.Method == http.MethodGet && action == "":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "token": tk})
		case r.Method == http.MethodPost && action == "rotate":
			_, _ = w.Write([]byte(`{"success":true,"new_token":"rotated-secret==","message":"m"}`))
		case r.Method == http.MethodPost && action == "revoke":
			tk.Enabled = false
			_, _ = w.Write([]byte(`{"success":true}`))
		case r.Method == http.MethodDelete:
			delete(tokens, id)
			_, _ = w.Write([]byte(`{"success":true}`))
		default:
			w.WriteHeader(405)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, tokens
}

func ptr[T any](v T) *T { return &v }

func writeTestConfig(t *testing.T, endpoint, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("ARCLI_CONFIG", path)
	t.Setenv("ARC_CONNECTION", "")
	t.Setenv("ARC_ENDPOINT", "")
	t.Setenv("ARC_TOKEN", "")
	body := "active = \"local\"\n\n[connections]\n\n[connections.local]\nendpoint = \"" + endpoint + "\"\ntoken = \"" + token + "\"\n\n[connections.twin]\nendpoint = \"" + endpoint + "\"\ntoken = \"" + token + "\"\n\n[connections.other]\nendpoint = \"" + endpoint + "\"\ntoken = \"other-secret\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAuthTokenCreate_StdoutIsSecretOnly(t *testing.T) {
	srv, _ := fakeAuthServer(t)
	writeTestConfig(t, srv.URL, "admin-secret")
	out, errOut, err := execCmd(t, newAuthTokenCreateCmd(), "--name", "ci", "--permission", "read")
	if err != nil {
		t.Fatal(err)
	}
	if out != "new-secret==\n" {
		t.Errorf("stdout must be the bare secret, got %q", out)
	}
	if !strings.Contains(errOut, `Created token "ci" (id 2)`) {
		t.Errorf("stderr = %q", errOut)
	}

	out, _, err = execCmd(t, newAuthTokenCreateCmd(), "--name", "ci2", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Token string `json:"token"`
		ID    int64  `json:"id"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.Token != "new-secret==" || got.ID != 3 {
		t.Errorf("json = %s (err %v)", out, err)
	}
}

func TestAuthTokenRotate_SaveUpdatesEveryProfileHoldingOldToken(t *testing.T) {
	srv, _ := fakeAuthServer(t)
	path := writeTestConfig(t, srv.URL, "admin-secret")
	out, errOut, err := execCmd(t, newAuthTokenRotateCmd(), "admin", "--save", "--yes")
	if err != nil {
		t.Fatalf("err = %v (stderr %q)", err, errOut)
	}
	if out != "rotated-secret==\n" {
		t.Errorf("stdout = %q", out)
	}
	if !strings.Contains(errOut, "Updated connection(s): local ("+srv.URL+"), twin ("+srv.URL+")") {
		t.Errorf("stderr = %q", errOut)
	}
	raw, _ := os.ReadFile(path)
	cfg := string(raw)
	if strings.Count(cfg, "rotated-secret==") != 2 || !strings.Contains(cfg, "other-secret") || strings.Contains(cfg, "admin-secret") {
		t.Errorf("config after save:\n%s", cfg)
	}

	// JSON mode carries the secret, id, and name only; the saved
	// profiles are reported on stderr.
	out, errOut, err = execCmd(t, newAuthTokenRotateCmd(), "1", "--save", "--yes", "-o", "json")
	if err != nil {
		t.Fatalf("err = %v (stderr %q)", err, errOut)
	}
	var got map[string]any
	if jerr := json.Unmarshal([]byte(out), &got); jerr != nil || got["token"] != "rotated-secret==" || got["id"] != float64(1) {
		t.Errorf("json = %s", out)
	}
	if _, has := got["saved_to"]; has {
		t.Error("saved_to must not be in the JSON (it is written after the secret is emitted)")
	}
	if !strings.Contains(errOut, "Updated connection(s): local (") {
		t.Errorf("stderr = %q", errOut)
	}
}

// The prompt can sit open while another shell edits the config; --save
// must apply the substitution to a fresh copy rather than write back the
// pre-prompt snapshot (security review M1).
func TestAuthTokenRotate_SaveDoesNotClobberConcurrentEdit(t *testing.T) {
	// Simulate the concurrent edit from inside the fake server's rotate
	// handler: that is exactly the window between the command's initial
	// Load and its Save.
	srv2, _ := fakeAuthServer(t)
	writeTestConfig(t, srv2.URL, "admin-secret")
	edited := false
	orig := srv2.Config.Handler
	srv2.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/rotate") && !edited {
			edited = true
			cfg, err := config.Load()
			if err != nil {
				t.Fatal(err)
			}
			cfg.Connections["added-meanwhile"] = config.Connection{Endpoint: "http://elsewhere", Token: "zzz"}
			delete(cfg.Connections, "other")
			if err := cfg.Save(); err != nil {
				t.Fatal(err)
			}
		}
		orig.ServeHTTP(w, r)
	})
	_, errOut, err := execCmd(t, newAuthTokenRotateCmd(), "admin", "--save", "--yes")
	if err != nil {
		t.Fatalf("err = %v (stderr %q)", err, errOut)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, kept := cfg.Connections["added-meanwhile"]; !kept {
		t.Error("concurrent add was clobbered")
	}
	if _, resurrected := cfg.Connections["other"]; resurrected {
		t.Error("concurrently deleted profile was resurrected")
	}
	if cfg.Connections["local"].Token != "rotated-secret==" || cfg.Connections["twin"].Token != "rotated-secret==" {
		t.Errorf("rotation not applied to fresh copy: %+v", cfg.Connections)
	}
}

func TestAuthTokenRotate_SaveRefusedWhenTargetIsNotOwnToken(t *testing.T) {
	srv, _ := fakeAuthServer(t)
	writeTestConfig(t, srv.URL, "admin-secret")
	// Create a second token, then try to --save while rotating it.
	if _, _, err := execCmd(t, newAuthTokenCreateCmd(), "--name", "ci"); err != nil {
		t.Fatal(err)
	}
	_, _, err := execCmd(t, newAuthTokenRotateCmd(), "ci", "--save", "--yes")
	if err == nil || !strings.Contains(err.Error(), "--save only applies when rotating the token in use") {
		t.Errorf("err = %v", err)
	}
}

func TestAuthTokenRotate_RefusesRevokedToken(t *testing.T) {
	srv, tokens := fakeAuthServer(t)
	writeTestConfig(t, srv.URL, "admin-secret")
	tokens[9] = ptr(tokenFixture(9, "dead", false))
	_, _, err := execCmd(t, newAuthTokenRotateCmd(), "dead", "--yes")
	if err == nil || !strings.Contains(err.Error(), "is revoked") {
		t.Errorf("err = %v", err)
	}
}

func TestAuthTokenDelete_LastAdminGuard(t *testing.T) {
	srv, _ := fakeAuthServer(t)
	writeTestConfig(t, srv.URL, "admin-secret")
	_, _, err := execCmd(t, newAuthTokenDeleteCmd(), "admin", "--yes")
	if err == nil || !strings.Contains(err.Error(), "last enabled admin token") {
		t.Errorf("err = %v", err)
	}
	_, _, err = execCmd(t, newAuthTokenRevokeCmd(), "admin", "--yes")
	if err == nil || !strings.Contains(err.Error(), "last enabled admin token") {
		t.Errorf("revoke err = %v", err)
	}
}

func TestAuthTokenDelete_SelfWarningAndByName(t *testing.T) {
	srv, tokens := fakeAuthServer(t)
	writeTestConfig(t, srv.URL, "admin-secret")
	out, errOut, err := execCmd(t, newAuthTokenDeleteCmd(), "admin", "--yes", "--force")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut, "WARNING") || !strings.Contains(errOut, "is the token this connection is using") {
		t.Errorf("self warning missing: %q", errOut)
	}
	if !strings.Contains(out, `Deleted token "admin" (id 1)`) {
		t.Errorf("stdout = %q", out)
	}
	if _, still := tokens[1]; still {
		t.Error("token 1 should be gone")
	}
	_, _, err = execCmd(t, newAuthTokenDeleteCmd(), "nope", "--yes")
	if err == nil || !strings.Contains(err.Error(), `no token named "nope"`) {
		t.Errorf("err = %v", err)
	}
}

func TestPing_OKAndBadToken(t *testing.T) {
	srv, _ := fakeAuthServer(t)
	writeTestConfig(t, srv.URL, "admin-secret")
	out, _, err := execCmd(t, newPingCmd())
	if err != nil {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if !strings.Contains(out, "health:     ok") || !strings.Contains(out, `auth:       ok as "admin" (id 1`) {
		t.Errorf("out = %s", out)
	}

	writeTestConfig(t, srv.URL, "wrong")
	out, _, err = execCmd(t, newPingCmd(), "-o", "json")
	if err == nil || !strings.Contains(err.Error(), "authentication failed") {
		t.Errorf("err = %v", err)
	}
	var rep struct {
		Health *struct{} `json:"health"`
		Auth   struct {
			Valid bool   `json:"valid"`
			Error string `json:"error"`
		} `json:"auth"`
	}
	if jerr := json.Unmarshal([]byte(out), &rep); jerr != nil || rep.Health == nil || rep.Auth.Valid || rep.Auth.Error == "" {
		t.Errorf("json on failure = %s (err %v)", out, jerr)
	}
}

func TestPing_AuthDisabledServerIsNotFailure(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok","time":"t","uptime":"1s"}`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":"Cannot GET /api/v1/auth/verify"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	writeTestConfig(t, srv.URL, "anything")
	out, _, err := execCmd(t, newPingCmd())
	if err != nil || !strings.Contains(out, "auth:       disabled on server") {
		t.Errorf("err=%v out=%s", err, out)
	}
}

func TestConfigUpdate(t *testing.T) {
	writeTestConfig(t, "http://a", "tok")
	_, _, err := execCmd(t, newConfigUpdateCmd(), "local")
	if err == nil || !strings.Contains(err.Error(), "nothing to update") {
		t.Errorf("err = %v", err)
	}
	_, _, err = execCmd(t, newConfigUpdateCmd(), "missing", "--token", "x")
	if err == nil || !strings.Contains(err.Error(), `connection "missing" not found`) {
		t.Errorf("err = %v", err)
	}
	_, _, err = execCmd(t, newConfigUpdateCmd(), "local", "--endpoint", "localhost:8000")
	if err == nil || !strings.Contains(err.Error(), "--endpoint must be an http:// or https:// URL") {
		t.Errorf("err = %v", err)
	}
	out, _, err := execCmd(t, newConfigUpdateCmd(), "local", "--token", "x", "--default-database", "m", "--insecure=true")
	if err != nil || !strings.Contains(out, `Updated connection "local" (token, default_database, insecure_tls)`) {
		t.Errorf("err=%v out=%q", err, out)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	local := cfg.Connections["local"]
	if local.Token != "x" || local.DefaultDatabase != "m" || !local.InsecureTLS || local.Endpoint != "http://a" {
		t.Errorf("local = %+v", local)
	}
	if twin := cfg.Connections["twin"]; twin.Token != "tok" || twin.InsecureTLS {
		t.Errorf("twin profile must be untouched: %+v", twin)
	}
}

// A host whose /health is a bare {"status":"ok"} (a load balancer, some
// other service) must NOT be reported as a healthy Arc with auth
// disabled just because /api/v1/auth/verify 404s there.
func TestPing_NonArcHealthIsFailure(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	writeTestConfig(t, srv.URL, "anything")
	out, _, err := execCmd(t, newPingCmd(), "-o", "json")
	if err == nil || !strings.Contains(err.Error(), "not an Arc health response") {
		t.Errorf("err = %v\n%s", err, out)
	}
	if !strings.Contains(out, `"error": "not attempted"`) {
		t.Errorf("auth must be reported as not attempted when health fails: %s", out)
	}
}

func TestClean_StripsControlChars(t *testing.T) {
	if got := clean("ok\x1b[31mred\x07\n"); got != "ok[31mred" {
		t.Errorf("clean = %q", got)
	}
	// C1 controls (8-bit CSI) and bidi overrides are stripped too.
	if got := clean("a\u009bb\u202ec"); got != "abc" {
		t.Errorf("clean C1/bidi = %q", got)
	}
	if got := cleanMultiline("SELECT 1\n\tFROM x\x1b\u202e"); got != "SELECT 1\n\tFROM x" {
		t.Errorf("cleanMultiline = %q", got)
	}
}

func TestConfigCreate_RejectsSentinelNameAndUserinfo(t *testing.T) {
	writeTestConfig(t, "http://a", "tok")
	_, _, err := execCmd(t, newConfigCreateCmd(), "--name", "(flags)", "--endpoint", "http://x", "--token", "t")
	if err == nil || !strings.Contains(err.Error(), `must not start with "("`) {
		t.Errorf("err = %v", err)
	}
	_, _, err = execCmd(t, newConfigCreateCmd(), "--name", "ok", "--endpoint", "http://u:p@x", "--token", "t")
	if err == nil || !strings.Contains(err.Error(), "must not contain credentials") {
		t.Errorf("err = %v", err)
	}
}
