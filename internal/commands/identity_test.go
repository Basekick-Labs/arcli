package commands

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/basekick-labs/arcli/internal/client"
	"github.com/basekick-labs/arcli/internal/config"
)

// pingWith runs `arcli ping` against a fake server and returns the
// headers the server saw on the /health call.
func pingWith(t *testing.T, args ...string) http.Header {
	t.Helper()
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			got = r.Header.Clone()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	t.Cleanup(srv.Close)
	root := NewRoot(BuildInfo{Version: "9.9.9"})
	_, _, _ = execCmd(t, root, append([]string{"ping", "--endpoint", srv.URL, "--token", "t"}, args...)...)
	return got
}

func TestInstallationIDHeader(t *testing.T) {
	t.Setenv("DO_NOT_TRACK", "") // the developer's own opt-out must not fail the suite
	// A config file written by arcli carries an id; it is sent even when
	// the connection itself comes from --endpoint/--token.
	writeTestConfig(t, "http://unused", "tok")
	cfg, _ := config.Load()
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	id := cfg.InstallationID
	h := pingWith(t)
	if h.Get(client.HeaderInstallationID) != id || !strings.HasPrefix(h.Get("User-Agent"), "arcli/9.9.9 (") {
		t.Errorf("headers: %v", h)
	}

	// DO_NOT_TRACK suppresses the id but not the User-Agent.
	t.Setenv("DO_NOT_TRACK", "1")
	h = pingWith(t)
	if h.Get(client.HeaderInstallationID) != "" || !strings.HasPrefix(h.Get("User-Agent"), "arcli/9.9.9") {
		t.Errorf("DO_NOT_TRACK: %v", h)
	}
	t.Setenv("DO_NOT_TRACK", "")

	// send_installation_id = false in the file.
	off := false
	cfg.SendInstallationID = &off
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if h = pingWith(t); h.Get(client.HeaderInstallationID) != "" {
		t.Errorf("opt-out: %v", h)
	}

	// No config file at all (env-only / container): no id exists.
	t.Setenv("ARCLI_CONFIG", t.TempDir()+"/none.toml")
	if h = pingWith(t); h.Get(client.HeaderInstallationID) != "" || h.Get("User-Agent") == "" {
		t.Errorf("no config: %v", h)
	}
}

func TestConfigCurrentShowsInstallationID(t *testing.T) {
	t.Setenv("DO_NOT_TRACK", "")
	writeTestConfig(t, "http://a", "secret-value-xyz")
	cfg, _ := config.Load()
	_ = cfg.Save()
	out, _, err := execCmd(t, newConfigCmd(), "current")
	if err != nil || !strings.Contains(out, "installation_id:  "+cfg.InstallationID+" (sent to Arc servers: yes)") {
		t.Errorf("err=%v out=%q", err, out)
	}
	t.Setenv("DO_NOT_TRACK", "true")
	out, _, _ = execCmd(t, newConfigCmd(), "current")
	if !strings.Contains(out, "sent to Arc servers: no (DO_NOT_TRACK is set)") {
		t.Errorf("out=%q", out)
	}
	t.Setenv("DO_NOT_TRACK", "")
	off := false
	cfg.SendInstallationID = &off
	_ = cfg.Save()
	out, _, _ = execCmd(t, newConfigCmd(), "current")
	if !strings.Contains(out, "no (send_installation_id = false)") {
		t.Errorf("out=%q", out)
	}
	if strings.Contains(out, "secret-value-xyz") {
		t.Errorf("token leaked: %q", out)
	}
}

func TestConfigCreatePrintsInstallationNotice(t *testing.T) {
	t.Setenv("ARCLI_CONFIG", t.TempDir()+"/config.toml")
	t.Setenv("DO_NOT_TRACK", "")
	out, _, err := execCmd(t, newConfigCreateCmd(), "--name", "a", "--endpoint", "http://a", "--token", "t")
	if err != nil || !strings.Contains(out, "Generated installation id ") || !strings.Contains(out, "DO_NOT_TRACK=1") {
		t.Errorf("first create: err=%v out=%q", err, out)
	}
	out, _, _ = execCmd(t, newConfigCreateCmd(), "--name", "b", "--endpoint", "http://b", "--token", "t")
	if strings.Contains(out, "Generated installation id") {
		t.Errorf("second create must not re-announce: %q", out)
	}
	// With DO_NOT_TRACK set, the notice says the id is not sent.
	t.Setenv("ARCLI_CONFIG", t.TempDir()+"/config.toml")
	t.Setenv("DO_NOT_TRACK", "1")
	out, _, _ = execCmd(t, newConfigCreateCmd(), "--name", "a", "--endpoint", "http://a", "--token", "t")
	if !strings.Contains(out, "(not sent: DO_NOT_TRACK") || strings.Contains(out, "arcli sends it") {
		t.Errorf("DO_NOT_TRACK create: %q", out)
	}
}

// config delete goes through confirmOrAbort (arcli#23): declined or
// non-interactive prompts exit 1 and leave the file untouched.
func TestConfigDelete_DeclinedPromptIsAnError(t *testing.T) {
	writeTestConfig(t, "http://a", "tok")
	c := newConfigCmd()
	c.SetIn(strings.NewReader("n\n"))
	_, errOut, err := execCmd(t, c, "delete", "twin")
	if err != errAborted || !strings.Contains(errOut, `Delete connection "twin"? [y/N]`) {
		t.Fatalf("declined: err=%v stderr=%q", err, errOut)
	}
	cfg, _ := config.Load()
	if _, ok := cfg.Connections["twin"]; !ok {
		t.Fatal("declined prompt must not delete the profile")
	}
	c = newConfigCmd()
	c.SetIn(strings.NewReader("yes\n"))
	if out, _, err := execCmd(t, c, "delete", "twin"); err != nil || !strings.Contains(out, `Deleted connection "twin"`) {
		t.Fatalf("accepted: err=%v out=%q", err, out)
	}
	if out, _, err := execCmd(t, newConfigCmd(), "delete", "other", "--yes"); err != nil || !strings.Contains(out, `Deleted connection "other"`) {
		t.Fatalf("--yes: err=%v out=%q", err, out)
	}
}

// Token-less profiles: allowed for servers with auth.enabled = false.
func TestTokenlessProfile(t *testing.T) {
	t.Setenv("ARCLI_CONFIG", t.TempDir()+"/config.toml")
	t.Setenv("ARC_CONNECTION", "")
	t.Setenv("ARC_ENDPOINT", "")
	t.Setenv("ARC_TOKEN", "")
	out, errOut, err := execCmd(t, newConfigCreateCmd(), "--name", "lab", "--endpoint", "http://lab:8000")
	if err != nil || !strings.Contains(out, `Created connection "lab"`) || !strings.Contains(errOut, "no token stored") {
		t.Fatalf("create without token: err=%v out=%q stderr=%q", err, out, errOut)
	}
	out, _, _ = execCmd(t, newConfigCmd(), "list")
	if !strings.Contains(out, "(none)") {
		t.Errorf("list must show (none) for an empty token: %q", out)
	}
	// --token "" clears a stored token, with the same note.
	if _, _, err := execCmd(t, newConfigUpdateCmd(), "lab", "--token", "secret-token-1234"); err != nil {
		t.Fatal(err)
	}
	_, errOut, err = execCmd(t, newConfigUpdateCmd(), "lab", "--token", "")
	cfg, _ := config.Load()
	if err != nil || cfg.Connections["lab"].Token != "" || !strings.Contains(errOut, "token cleared") {
		t.Errorf("clear: err=%v token=%q stderr=%q", err, cfg.Connections["lab"].Token, errOut)
	}
	// A name and an endpoint are still required.
	if _, _, err := execCmd(t, newConfigCreateCmd(), "--name", "x"); err == nil || !strings.Contains(err.Error(), "--name and --endpoint are required") {
		t.Errorf("missing endpoint: %v", err)
	}
}

// ping against a server with authentication disabled (verify 404s)
// succeeds without a token; against one that has auth it fails with a
// message naming the missing token rather than a bare 401.
func TestPing_NoToken(t *testing.T) {
	authOn := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"status":"ok","time":"t","uptime":"1s"}`))
		case "/api/v1/auth/verify":
			if authOn {
				w.WriteHeader(401)
				_, _ = w.Write([]byte(`{"error":"Invalid or expired token"}`))
			} else {
				w.WriteHeader(404)
				_, _ = w.Write([]byte(`{"error":"Cannot GET /api/v1/auth/verify"}`))
			}
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ARCLI_CONFIG", t.TempDir()+"/none.toml")
	t.Setenv("ARC_CONNECTION", "")
	t.Setenv("ARC_TOKEN", "")
	t.Setenv("ARC_ENDPOINT", srv.URL) // endpoint alone, no token
	out, _, err := execCmd(t, newPingCmd())
	if err != nil || !strings.Contains(out, "auth:       disabled on server (no token required)") || !strings.Contains(out, "(connection (env))") {
		t.Fatalf("auth off: err=%v out=%q", err, out)
	}
	authOn = true
	out, _, err = execCmd(t, newPingCmd())
	if err == nil || !strings.Contains(err.Error(), "server requires a token but this connection has none") || !strings.Contains(out, "auth:       FAILED (server requires a token") {
		t.Errorf("auth on: err=%v out=%q", err, out)
	}
	// An ad-hoc --endpoint without --token works the same way.
	t.Setenv("ARC_ENDPOINT", "")
	authOn = false
	if _, _, err := execCmd(t, newPingCmd(), "--endpoint", srv.URL); err != nil {
		t.Errorf("--endpoint alone: %v", err)
	}
	// A token without an endpoint is still an error.
	if _, _, err := execCmd(t, newPingCmd(), "--token", "t"); err == nil || !strings.Contains(err.Error(), "--token needs --endpoint") {
		t.Errorf("--token alone: %v", err)
	}
}
