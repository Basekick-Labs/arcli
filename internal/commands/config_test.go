package commands

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/basekick-labs/arcli/internal/config"
)

func TestConfigConnectionNames(t *testing.T) {
	t.Setenv("ARCLI_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	t.Setenv("ARC_CONNECTION", "")
	t.Setenv("ARC_ENDPOINT", "")
	t.Setenv("ARC_TOKEN", "")

	// Case variants must coexist, and dots must remain part of the name.
	names := []string{"Prod.EU", "prod.eu", "LOCAL"}
	for _, name := range names {
		_, _, err := execCmd(t, newConfigCreateCmd(), "--name", name, "--endpoint", "https://example.com")
		if err != nil {
			t.Fatalf("create %q: %v", name, err)
		}
	}
	for _, name := range names {
		_, _, err := execCmd(t, newConfigCreateCmd(), "--name", name, "--endpoint", "https://other.example.com")
		if err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("duplicate %q: err=%v", name, err)
		}
		if _, _, err := execCmd(t, newConfigUpdateCmd(), name, "--default-database", name); err != nil {
			t.Fatalf("update %q: %v", name, err)
		}
	}
	for _, name := range names {
		if _, _, err := execCmd(t, newConfigSetActiveCmd(), name); err != nil {
			t.Fatalf("set active %q: %v", name, err)
		}
		out, _, err := execCmd(t, newConfigCurrentCmd())
		if err != nil || !strings.Contains(out, "name:             "+name+"\n") {
			t.Fatalf("current %q: out=%q err=%v", name, out, err)
		}
		cfg, err := config.Load()
		if err != nil {
			t.Fatal(err)
		}
		conn, gotName, err := cfg.Resolve(config.ResolveOptions{})
		if err != nil || gotName != name || conn.DefaultDatabase != name {
			t.Fatalf("resolve %q: conn=%+v name=%q err=%v", name, conn, gotName, err)
		}
	}
	for _, name := range names {
		if _, _, err := execCmd(t, newConfigDeleteCmd(), name, "--yes"); err != nil {
			t.Fatalf("delete %q: %v", name, err)
		}
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Connections) != 0 || cfg.Active != "" {
		t.Errorf("after deleting all connections: %+v", cfg)
	}
}
