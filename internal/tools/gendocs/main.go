// Command gendocs renders the man pages and shell completion scripts
// that the release archives, the Linux packages and the Homebrew
// formula install. It is run by GoReleaser's before-hook:
//
//	go run ./internal/tools/gendocs -version 26.9.0 -date 2026-09-07T12:00:00Z .gen
//
// Output layout under the target directory:
//
//	man/arcli.1, man/arcli-<cmd>.1, …   (plus .1.gz copies for packages)
//	completions/arcli.bash, _arcli, arcli.fish, arcli.ps1
//
// Pages are reproducible: the header date is the commit date passed in
// (UTC), never the wall clock, and cobra's auto-generated footer is off.
package main

import (
	"bytes"
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"

	"github.com/basekick-labs/arcli/internal/commands"
)

func main() {
	version := flag.String("version", "dev", "version shown in the man page header")
	date := flag.String("date", "", "RFC3339 date for the man page header (default: now, UTC)")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: gendocs [-version V] [-date RFC3339] <outdir>")
		os.Exit(2)
	}
	when := time.Now().UTC()
	if *date != "" {
		t, err := time.Parse(time.RFC3339, *date)
		if err != nil {
			fmt.Fprintln(os.Stderr, "gendocs: -date:", err)
			os.Exit(2)
		}
		when = t.UTC()
	}
	if err := generate(flag.Arg(0), *version, when); err != nil {
		fmt.Fprintln(os.Stderr, "gendocs:", err)
		os.Exit(1)
	}
}

func generate(outdir, version string, when time.Time) error {
	manDir := filepath.Join(outdir, "man")
	compDir := filepath.Join(outdir, "completions")
	for _, d := range []string{manDir, compDir} {
		// Start clean so a renamed command does not leave a stale page
		// behind from an earlier run.
		if err := os.RemoveAll(d); err != nil {
			return err
		}
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}

	root := commands.NewRoot(commands.BuildInfo{Version: version})
	// The completion command is added lazily at execution time; add it
	// now so `arcli completion` and its subcommands get pages too.
	root.InitDefaultCompletionCmd()
	disableAutoGenTag(root)

	when = when.UTC()
	header := &doc.GenManHeader{
		Title:   "ARCLI",
		Section: "1",
		Date:    &when,
		Source:  "arcli " + version,
		Manual:  "Arc CLI Manual",
	}
	if err := doc.GenManTree(root, header, manDir); err != nil {
		return fmt.Errorf("man pages: %w", err)
	}
	pages, err := filepath.Glob(filepath.Join(manDir, "*.1"))
	if err != nil {
		return err
	}
	for _, p := range pages {
		if err := gzipCopy(p, p+".gz", when); err != nil {
			return err
		}
	}

	gens := []struct {
		name string
		gen  func(io.Writer) error
	}{
		{"arcli.bash", func(w io.Writer) error { return root.GenBashCompletionV2(w, true) }},
		{"_arcli", root.GenZshCompletion},
		{"arcli.fish", func(w io.Writer) error { return root.GenFishCompletion(w, true) }},
		{"arcli.ps1", root.GenPowerShellCompletionWithDesc},
	}
	for _, g := range gens {
		var buf bytes.Buffer
		if err := g.gen(&buf); err != nil {
			return fmt.Errorf("%s: %w", g.name, err)
		}
		if err := os.WriteFile(filepath.Join(compDir, g.name), buf.Bytes(), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func disableAutoGenTag(c *cobra.Command) {
	c.DisableAutoGenTag = true
	for _, sub := range c.Commands() {
		disableAutoGenTag(sub)
	}
}

// gzipCopy writes a gzip copy of src with a fixed header time so the
// output is byte-identical across runs for the same input.
func gzipCopy(src, dst string, when time.Time) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Name = strings.TrimSuffix(filepath.Base(dst), ".gz")
	zw.ModTime = when
	if _, err := zw.Write(data); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return os.WriteFile(dst, buf.Bytes(), 0o644)
}
