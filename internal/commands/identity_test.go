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
