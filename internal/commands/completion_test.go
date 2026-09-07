package commands

import (
	"os"
	"strings"
	"testing"
)

func completeArgs(t *testing.T, args ...string) string {
	t.Helper()
	root := NewRoot(BuildInfo{Version: "test"})
	out, _, err := execCmd(t, root, append([]string{"__complete"}, args...)...)
	if err != nil {
		t.Fatalf("__complete %v: %v", args, err)
	}
	return out
}

func TestCompletion(t *testing.T) {
	writeTestConfig(t, "http://a", "tok")
	// -c completes connection names with the endpoint as description and
	// the active one marked; directive 4 = NoFileComp.
	out := completeArgs(t, "query", "--connection", "")
	for _, want := range []string{"local\thttp://a (active)\n", "other\thttp://a\n", "twin\thttp://a\n", ":4\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("-c: missing %q in %q", want, out)
		}
	}
	if strings.Contains(out, "tok") || strings.Contains(out, "secret") {
		t.Errorf("-c must not leak tokens: %q", out)
	}
	if out := completeArgs(t, "logs", "-c", "t"); !strings.HasPrefix(out, "twin\t") || strings.Contains(out, "local") {
		t.Errorf("prefix filter: %q", out)
	}
	// Positional connection name on config update/set-active/delete; a
	// second positional completes to nothing.
	if out := completeArgs(t, "config", "delete", ""); !strings.Contains(out, "local\t") || !strings.HasSuffix(out, ":4\n") {
		t.Errorf("config delete: %q", out)
	}
	if out := completeArgs(t, "config", "set-active", "local", ""); strings.Contains(out, "local") || !strings.HasSuffix(out, ":4\n") {
		t.Errorf("config set-active second arg: %q", out)
	}
	// Enum flags come from the usage text.
	if out := completeArgs(t, "query", "--output", ""); out != "table\njson\ncsv\narrow\n:4\n" {
		t.Errorf("query -o: %q", out)
	}
	if out := completeArgs(t, "db", "list", "-o", ""); out != "table\njson\ncsv\n:4\n" {
		t.Errorf("db list -o: %q", out)
	}
	if out := completeArgs(t, "write", "--format", ""); out != "lp\nmsgpack\njson\n:4\n" {
		t.Errorf("write --format: %q", out)
	}
	if out := completeArgs(t, "logs", "--level", ""); out != "debug\ninfo\nwarn\nerror\nfatal\n:4\n" {
		t.Errorf("logs --level: %q", out)
	}
	if out := completeArgs(t, "auth", "token", "create", "--permission", ""); out != "read\nwrite\ndelete\nadmin\n:4\n" {
		t.Errorf("--permission: %q", out)
	}
	// File flags keep file completion (directive 0 = Default).
	for _, args := range [][]string{{"query", "--file", ""}, {"write", "-f", ""}, {"import", "csv", "-f", ""}, {"cq", "create", "--query-file", ""}, {"cq", "update", "x", "--query-file", ""}} {
		if out := completeArgs(t, args...); out != ":0\n" {
			t.Errorf("%v: %q", args, out)
		}
	}
	// Everything else: no suggestions and no file listing.
	for _, args := range [][]string{{"query", "--database", ""}, {"query", "--endpoint", ""}, {"db", "drop", ""}, {"query", ""}} {
		if out := completeArgs(t, args...); out != ":4\n" {
			t.Errorf("%v: %q", args, out)
		}
	}
}

func TestCompletionWithoutConfig(t *testing.T) {
	// No config file, and a malformed one: connection completion is
	// silent in both cases, never an error in the stream.
	writeTestConfig(t, "http://a", "tok")
	t.Setenv("ARCLI_CONFIG", t.TempDir()+"/missing.toml")
	if out := completeArgs(t, "query", "-c", ""); out != ":4\n" {
		t.Errorf("missing config: %q", out)
	}
	bad := t.TempDir() + "/bad.toml"
	if err := os.WriteFile(bad, []byte("this is = not [toml"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARCLI_CONFIG", bad)
	if out := completeArgs(t, "query", "-c", ""); out != ":4\n" {
		t.Errorf("malformed config: %q", out)
	}
}

func TestEnumFromUsage(t *testing.T) {
	cases := map[string][]string{
		"output format: table|json|csv":                                 {"table", "json", "csv"},
		"permission to grant (read|write|delete|admin); repeat":         {"read", "write", "delete", "admin"},
		"epoch_s|epoch_ms|epoch_us|epoch_ns (empty = let DuckDB infer)": {"epoch_s", "epoch_ms", "epoch_us", "epoch_ns"},
		"named connection (overrides active)":                           nil,
		"a | b":                                                         nil,
		"Path|With|Caps":                                                nil,
	}
	for in, want := range cases {
		got := enumFromUsage(in)
		if len(got) != len(want) {
			t.Errorf("%q: got %v want %v", in, got, want)
			continue
		}
		for i := range want {
			if string(got[i]) != want[i] {
				t.Errorf("%q: got %v want %v", in, got, want)
			}
		}
	}
}

func TestVersionFlag(t *testing.T) {
	root := NewRoot(BuildInfo{Version: "1.2.3", Commit: "abc1234", Date: "2026-09-07T12:00:00Z"})
	out, _, err := execCmd(t, root, "--version")
	if err != nil || !strings.HasPrefix(out, "arcli version 1.2.3 (commit abc1234 at 2026-09-07T12:00:00Z, go") {
		t.Errorf("--version: %q err=%v", out, err)
	}
	// -v is reserved for a future verbose mode, not a version shorthand.
	if _, _, err := execCmd(t, NewRoot(BuildInfo{Version: "x"}), "-v"); err == nil || !strings.Contains(err.Error(), "unknown shorthand flag") {
		t.Errorf("-v: err = %v", err)
	}
	if s := (BuildInfo{Version: "dev"}).String(); !strings.HasPrefix(s, "dev (go") {
		t.Errorf("bare: %q", s)
	}
}
