package main

import (
	"runtime/debug"
	"testing"
)

func TestResolveBuild(t *testing.T) {
	bi := &debug.BuildInfo{
		Main: debug.Module{Version: "v1.2.3+dirty"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "0123456789abcdef"},
			{Key: "vcs.time", Value: "2026-09-07T14:30:00+02:00"},
			{Key: "vcs.modified", Value: "true"},
		},
	}
	// Plain `go build` / `go install`: everything from the toolchain, in
	// UTC; "+dirty" is reported once, on the commit.
	got := resolveBuild("dev", "", "", bi)
	if got.Version != "1.2.3" || got.Commit != "0123456+dirty" || got.Date != "2026-09-07T12:30:00Z" {
		t.Errorf("fallback: %+v", got)
	}
	// ldflags win, and the date is still normalised to UTC.
	got = resolveBuild("1.0.0", "abc1234", "2026-09-07T10:00:00-03:00", bi)
	if got.Version != "1.0.0" || got.Commit != "abc1234" || got.Date != "2026-09-07T13:00:00Z" {
		t.Errorf("ldflags: %+v", got)
	}
	// No build info at all (stripped binary): never empty, never a panic.
	got = resolveBuild("", "", "", nil)
	if got.Version != "dev" || got.Commit != "" || got.Date != "" {
		t.Errorf("nil: %+v", got)
	}
	// Pseudo-versions (untagged checkout, N commits after a tag, after a
	// prerelease) are not versions: the second form would claim an
	// unreleased patch release.
	for _, pv := range []string{
		"v0.0.0-20260907211436-ff677d71a5e3+dirty",
		"v0.0.0-20260907211436-ff677d71a5e3",
		"v1.0.1-0.20260907215038-a321d31b5862",
		"v1.0.0-rc.1.0.20260907215038-a321d31b5862+dirty",
	} {
		if got := resolveBuild("dev", "", "", &debug.BuildInfo{Main: debug.Module{Version: pv}}); got.Version != "dev" {
			t.Errorf("pseudo %s: %+v", pv, got)
		}
	}
	// A real prerelease tag is kept.
	if got := resolveBuild("dev", "", "", &debug.BuildInfo{Main: debug.Module{Version: "v1.0.0-rc.1"}}); got.Version != "1.0.0-rc.1" {
		t.Errorf("rc: %+v", got)
	}
	// "(devel)" is not a version; an unparsable date is passed through.
	got = resolveBuild("dev", "", "yesterday", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}})
	if got.Version != "dev" || got.Date != "yesterday" {
		t.Errorf("devel: %+v", got)
	}
}
