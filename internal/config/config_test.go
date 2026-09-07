package config

import (
	"os"
	"path/filepath"
	"testing"
)

// withTempConfig runs fn with ARCLI_CONFIG pointed at a fresh tempdir-
// scoped path so tests don't touch the real ~/.arcli/config.toml.
func withTempConfig(t *testing.T, fn func()) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	t.Setenv("ARCLI_CONFIG", path)
	fn()
}

// clearEnv unsets the env vars Resolve consults so a test's expected
// precedence isn't polluted by the developer's shell env.
func clearEnv(t *testing.T) {
	t.Helper()
	t.Setenv("ARC_CONNECTION", "")
	t.Setenv("ARC_ENDPOINT", "")
	t.Setenv("ARC_TOKEN", "")
}

func TestLoad_MissingFile_ReturnsEmpty(t *testing.T) {
	withTempConfig(t, func() {
		cfg, err := Load()
		if err != nil {
			t.Fatalf("load missing file: %v", err)
		}
		if cfg == nil {
			t.Fatal("cfg is nil")
		}
		if len(cfg.Connections) != 0 {
			t.Errorf("expected 0 connections, got %d", len(cfg.Connections))
		}
		if cfg.Active != "" {
			t.Errorf("expected empty active, got %q", cfg.Active)
		}
	})
}

func TestSaveAndLoad_Roundtrip(t *testing.T) {
	withTempConfig(t, func() {
		cfg := &Config{
			Active: "prod",
			Connections: map[string]Connection{
				"prod": {
					Endpoint:        "https://arc.prod.example.com",
					Token:           "prod-token-12345",
					DefaultDatabase: "metrics",
				},
				"local": {
					Endpoint: "http://localhost:8000",
					Token:    "local-token-abcde",
				},
			},
		}
		if err := cfg.Save(); err != nil {
			t.Fatalf("save: %v", err)
		}

		// Mode 0600 (tokens are sensitive — same posture as ~/.aws/credentials).
		path, _ := ConfigPath()
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat saved file: %v", err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("config file mode = %#o, want 0600", mode)
		}

		// Round-trip loads back identical.
		got, err := Load()
		if err != nil {
			t.Fatalf("load after save: %v", err)
		}
		if got.Active != "prod" {
			t.Errorf("active = %q, want %q", got.Active, "prod")
		}
		if len(got.Connections) != 2 {
			t.Fatalf("got %d connections, want 2", len(got.Connections))
		}
		if got.Connections["prod"].Endpoint != "https://arc.prod.example.com" {
			t.Errorf("prod endpoint = %q", got.Connections["prod"].Endpoint)
		}
		if got.Connections["prod"].DefaultDatabase != "metrics" {
			t.Errorf("prod default_database = %q", got.Connections["prod"].DefaultDatabase)
		}
		if got.Connections["local"].Endpoint != "http://localhost:8000" {
			t.Errorf("local endpoint = %q", got.Connections["local"].Endpoint)
		}
	})
}

func TestResolve_PrecedenceOrder(t *testing.T) {
	// The full precedence ladder, exercised in order. Each subtest sets
	// up exactly one signal and confirms it wins over anything lower.
	withTempConfig(t, func() {
		cfg := &Config{
			Active: "active-conn",
			Connections: map[string]Connection{
				"active-conn":   {Endpoint: "http://active", Token: "active-token-aaaaa"},
				"override-conn": {Endpoint: "http://override", Token: "override-token-bbbbb"},
			},
		}

		t.Run("1_flag_connection_wins_over_everything", func(t *testing.T) {
			t.Setenv("ARC_CONNECTION", "active-conn")
			t.Setenv("ARC_ENDPOINT", "http://env")
			t.Setenv("ARC_TOKEN", "env-token-cccccc")
			conn, name, err := cfg.Resolve(ResolveOptions{ConnectionName: "override-conn"})
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if name != "override-conn" || conn.Endpoint != "http://override" {
				t.Errorf("flag connection did not win: name=%q endpoint=%q", name, conn.Endpoint)
			}
		})

		t.Run("2_flag_endpoint_plus_token_wins_over_env", func(t *testing.T) {
			t.Setenv("ARC_CONNECTION", "active-conn")
			t.Setenv("ARC_ENDPOINT", "http://env")
			t.Setenv("ARC_TOKEN", "env-token-cccccc")
			conn, name, err := cfg.Resolve(ResolveOptions{
				Endpoint: "http://flag",
				Token:    "flag-token-dddddd",
			})
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if name != "(flags)" || conn.Endpoint != "http://flag" {
				t.Errorf("ad-hoc flag override did not win: name=%q endpoint=%q", name, conn.Endpoint)
			}
		})

		t.Run("3_ARC_CONNECTION_env_wins_over_active", func(t *testing.T) {
			clearEnv(t)
			t.Setenv("ARC_CONNECTION", "override-conn")
			conn, name, err := cfg.Resolve(ResolveOptions{})
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if name != "override-conn" || conn.Endpoint != "http://override" {
				t.Errorf("ARC_CONNECTION did not win: name=%q endpoint=%q", name, conn.Endpoint)
			}
		})

		t.Run("4_ARC_ENDPOINT_plus_ARC_TOKEN_env_wins_over_active", func(t *testing.T) {
			clearEnv(t)
			t.Setenv("ARC_ENDPOINT", "http://env-only")
			t.Setenv("ARC_TOKEN", "env-only-token-eeee")
			conn, name, err := cfg.Resolve(ResolveOptions{})
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if name != "(env)" || conn.Endpoint != "http://env-only" {
				t.Errorf("env ad-hoc did not win: name=%q endpoint=%q", name, conn.Endpoint)
			}
		})

		t.Run("5_active_connection_in_config_is_default", func(t *testing.T) {
			clearEnv(t)
			conn, name, err := cfg.Resolve(ResolveOptions{})
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if name != "active-conn" || conn.Endpoint != "http://active" {
				t.Errorf("active connection did not win: name=%q endpoint=%q", name, conn.Endpoint)
			}
		})
	})
}

func TestResolve_Errors(t *testing.T) {
	withTempConfig(t, func() {
		t.Run("unknown_connection_name", func(t *testing.T) {
			cfg := &Config{Connections: map[string]Connection{}}
			_, _, err := cfg.Resolve(ResolveOptions{ConnectionName: "nope"})
			if err == nil {
				t.Error("expected error for unknown connection name")
			}
		})

		t.Run("flag_endpoint_without_token", func(t *testing.T) {
			cfg := &Config{Connections: map[string]Connection{}}
			_, _, err := cfg.Resolve(ResolveOptions{Endpoint: "http://x"})
			if err == nil {
				t.Error("expected error for endpoint without token")
			}
		})

		t.Run("env_endpoint_without_token", func(t *testing.T) {
			clearEnv(t)
			t.Setenv("ARC_ENDPOINT", "http://x")
			cfg := &Config{Connections: map[string]Connection{}}
			_, _, err := cfg.Resolve(ResolveOptions{})
			if err == nil {
				t.Error("expected error for ARC_ENDPOINT without ARC_TOKEN")
			}
		})

		t.Run("no_active_no_env_no_flags", func(t *testing.T) {
			clearEnv(t)
			cfg := &Config{Connections: map[string]Connection{}}
			_, _, err := cfg.Resolve(ResolveOptions{})
			if err == nil {
				t.Error("expected error when nothing is set")
			}
		})

		t.Run("active_references_missing_connection", func(t *testing.T) {
			clearEnv(t)
			cfg := &Config{
				Active:      "missing",
				Connections: map[string]Connection{"other": {Endpoint: "http://x", Token: "t"}},
			}
			_, _, err := cfg.Resolve(ResolveOptions{})
			if err == nil {
				t.Error("expected error when active points to undefined connection")
			}
		})
	})
}

func TestRedactToken(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "************"},
		{"short", "************"},
		{"abcdefghijkl", "abcd...ijkl"},
		{"abcdefghijklmnop", "abcd...mnop"},
	}
	for _, c := range cases {
		got := RedactToken(c.in)
		if got != c.want {
			t.Errorf("RedactToken(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
