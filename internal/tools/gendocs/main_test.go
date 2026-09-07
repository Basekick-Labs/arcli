package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGenerate(t *testing.T) {
	dir := t.TempDir()
	when := time.Date(2026, 9, 7, 12, 0, 0, 0, time.FixedZone("x", 2*3600))
	if err := generate(dir, "9.9.9", when); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{
		"man/arcli.1", "man/arcli.1.gz", "man/arcli-query.1", "man/arcli-config-create.1",
		"man/arcli-completion.1", "man/arcli-completion-zsh.1",
		"completions/arcli.bash", "completions/_arcli", "completions/arcli.fish", "completions/arcli.ps1",
	} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "man/arcli-help.1")); err == nil {
		t.Error("help must not get a page")
	}
	page, _ := os.ReadFile(filepath.Join(dir, "man/arcli.1"))
	s := string(page)
	// Header carries the version and the UTC commit date; no wall clock,
	// no cobra auto-gen footer.
	if !strings.Contains(s, `"arcli 9.9.9"`) || !strings.Contains(s, "Sep 2026") || !strings.Contains(s, "Arc CLI Manual") {
		t.Errorf("header: %s", strings.SplitN(s, "\n", 4)[0:3])
	}
	if strings.Contains(s, "Auto generated") {
		t.Error("auto-gen tag must be disabled")
	}
	if !strings.Contains(s, "SEE ALSO") || !strings.Contains(s, "arcli-query(1)") {
		t.Error("root page must cross-reference subcommands")
	}
	// Byte-identical across runs (reproducible packages).
	dir2 := t.TempDir()
	if err := generate(dir2, "9.9.9", when); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"man/arcli.1.gz", "completions/_arcli"} {
		a, _ := os.ReadFile(filepath.Join(dir, f))
		b, _ := os.ReadFile(filepath.Join(dir2, f))
		if string(a) != string(b) {
			t.Errorf("%s differs between runs", f)
		}
	}
}
