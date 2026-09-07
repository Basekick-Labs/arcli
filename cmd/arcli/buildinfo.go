package main

import (
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"github.com/basekick-labs/arcli/internal/commands"
)

// pseudoVersion matches Go's <timestamp>-<12-hex> pseudo-version tail
// (with an optional +dirty / +incompatible suffix).
var pseudoVersion = regexp.MustCompile(`\d{14}-[0-9a-f]{12}(\+[a-z]+)?$`)

// resolveBuild merges the -ldflags values with what the Go toolchain
// embeds on its own, so `go build` and `go install …@v1.0.0` report
// something useful without the release pipeline:
//
//   - version: ldflags, else the module version when the binary was
//     built from a tagged module, else "dev". Note Go only recognises
//     v0/v1 tags for this module path, so with CalVer tags (v26.09.1)
//     this branch fires only for the release build's ldflags.
//   - commit: ldflags, else vcs.revision shortened to 7 with "+dirty"
//     appended when vcs.modified is set.
//   - date: ldflags, else vcs.time. Always re-rendered as UTC RFC3339.
func resolveBuild(version, commit, date string, bi *debug.BuildInfo) commands.BuildInfo {
	out := commands.BuildInfo{Version: version, Commit: commit, Date: date}
	if bi != nil {
		// Main.Version is a real tag only for `go install …@vX.Y.Z` or a
		// build inside a checkout sitting exactly on a tag. "(devel)" and
		// pseudo-versions (v0.0.0-<ts>-<sha>, v1.0.1-0.<ts>-<sha>,
		// v1.0.0-rc.1.0.<ts>-<sha>) say nothing the commit does not, and
		// the second form would claim an unreleased patch. The "+dirty"
		// suffix is reported on the commit instead.
		if (out.Version == "" || out.Version == "dev") && bi.Main.Version != "" && bi.Main.Version != "(devel)" && !pseudoVersion.MatchString(bi.Main.Version) {
			out.Version = strings.TrimSuffix(strings.TrimPrefix(bi.Main.Version, "v"), "+dirty")
		}
		var rev, vtime string
		dirty := false
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.time":
				vtime = s.Value
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
		if out.Commit == "" && rev != "" {
			if len(rev) > 7 {
				rev = rev[:7]
			}
			if dirty {
				rev += "+dirty"
			}
			out.Commit = rev
		}
		if out.Date == "" {
			out.Date = vtime
		}
	}
	if out.Version == "" {
		out.Version = "dev"
	}
	if t, err := time.Parse(time.RFC3339, out.Date); err == nil {
		out.Date = t.UTC().Format(time.RFC3339)
	}
	return out
}
