package config

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestInstallationID_MintedOnFirstSave(t *testing.T) {
	t.Setenv("DO_NOT_TRACK", "")
	withTempConfig(t, func() {
		cfg, err := Load()
		if err != nil || cfg.InstallationID != "" || !cfg.SendsInstallationID() {
			t.Fatalf("fresh load: cfg=%+v err=%v", cfg, err)
		}
		if cfg.OutboundInstallationID() != "" {
			t.Error("no file yet: nothing to send")
		}
		cfg.Connections["a"] = Connection{Endpoint: "http://a", Token: "t"}
		if err := cfg.Save(); err != nil {
			t.Fatal(err)
		}
		id := cfg.InstallationID
		if !uuidV4.MatchString(id) {
			t.Fatalf("installation id %q is not a v4 UUID", id)
		}
		// Stable across saves and loads; default opt-in is implicit in the file.
		again, err := Load()
		if err != nil || again.InstallationID != id || again.SendInstallationID != nil || again.OutboundInstallationID() != id {
			t.Fatalf("reload: cfg=%+v err=%v", again, err)
		}
		if err := again.Save(); err != nil {
			t.Fatal(err)
		}
		path, _ := ConfigPath()
		raw, _ := os.ReadFile(path)
		if !strings.Contains(string(raw), "installation_id = '"+id+"'") || strings.Contains(string(raw), "send_installation_id") {
			t.Errorf("file:\n%s", raw)
		}
		// Opt-out is persisted explicitly and honoured; the id is kept.
		off := false
		again.SendInstallationID = &off
		if err := again.Save(); err != nil {
			t.Fatal(err)
		}
		out, err := Load()
		if err != nil || out.SendsInstallationID() || out.InstallationID != id || out.OutboundInstallationID() != "" {
			t.Fatalf("opt-out: cfg=%+v err=%v", out, err)
		}
		raw, _ = os.ReadFile(path)
		if !strings.Contains(string(raw), "send_installation_id = false") {
			t.Errorf("file:\n%s", raw)
		}
	})
}

func TestInstallationID_DoNotTrack(t *testing.T) {
	withTempConfig(t, func() {
		cfg := &Config{Connections: map[string]Connection{}} // literal: still opted in
		if err := cfg.Save(); err != nil {
			t.Fatal(err)
		}
		if raw, _ := os.ReadFile(func() string { p, _ := ConfigPath(); return p }()); strings.Contains(string(raw), "send_installation_id") {
			t.Errorf("struct literal must not persist an opt-out:\n%s", raw)
		}
		for _, v := range []string{"1", "true", "YES", " on "} {
			t.Setenv("DO_NOT_TRACK", v)
			if !DoNotTrack() || cfg.OutboundInstallationID() != "" {
				t.Errorf("DO_NOT_TRACK=%q must suppress the id", v)
			}
		}
		for _, v := range []string{"", "0", "false", "no"} {
			t.Setenv("DO_NOT_TRACK", v)
			if DoNotTrack() || cfg.OutboundInstallationID() != cfg.InstallationID {
				t.Errorf("DO_NOT_TRACK=%q must not suppress the id", v)
			}
		}
	})
}

func TestInstallationID_OlderFileWithoutKey(t *testing.T) {
	t.Setenv("DO_NOT_TRACK", "")
	withTempConfig(t, func() {
		path, _ := ConfigPath()
		body := "active = 'x'\n\n[connections]\n\n[connections.x]\nendpoint = 'http://x'\ntoken = 'tok'\n"
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load()
		if err != nil || cfg.InstallationID != "" || !cfg.SendsInstallationID() || cfg.OutboundInstallationID() != "" {
			t.Fatalf("pre-id file: cfg=%+v err=%v", cfg, err)
		}
		// The next write backfills it.
		if err := cfg.Save(); err != nil {
			t.Fatal(err)
		}
		if !uuidV4.MatchString(cfg.InstallationID) {
			t.Errorf("backfill: %q", cfg.InstallationID)
		}
	})
}

func TestNewInstallationID_Unique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id, err := newInstallationID()
		if err != nil || !uuidV4.MatchString(id) || seen[id] {
			t.Fatalf("id %q err %v dup %v", id, err, seen[id])
		}
		seen[id] = true
	}
}
