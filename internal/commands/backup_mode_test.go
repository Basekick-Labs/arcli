package commands

import (
	"strings"
	"testing"
)

const modeTestID = "backup-20260907-200009-00000009"

// scopedManifest is an Arc 27.01.1 scoped backup of t85_a. gaps adds the
// skipped/unaddressable counts Arc refuses a replace for.
func scopedManifest(gaps bool) string {
	extra := ""
	if gaps {
		extra = `"skipped_files":1,"skipped_sample":["t85_a/cpu/2026/09/07/20/gone.parquet"],"unaddressable_files":1,`
	}
	return `{"version":"dev","backup_id":"` + modeTestID + `","created_at":"2026-09-07T20:00:00Z","backup_type":"full","scope":["t85_a"],"databases":[{"name":"t85_a","measurements":[{"name":"cpu","file_count":1,"size_bytes":1064}],"file_count":1,"size_bytes":1064}],"total_files":1,"total_size_bytes":1064,` + extra + `"has_metadata":false,"has_config":false}`
}

// declinedPrompt runs the restore, answers n and returns the prompt.
func declinedPrompt(t *testing.T, f *fakeBackupServer, args ...string) string {
	t.Helper()
	cmd := newBackupRestoreCmd()
	cmd.SetIn(strings.NewReader("n\n"))
	_, stderr, err := execCmd(t, cmd, append([]string{modeTestID}, args...)...)
	if err == nil || err.Error() != "aborted" {
		t.Fatalf("err = %v; want declined confirmation (stderr %q)", err, stderr)
	}
	if len(f.lastRestoreRequest()) != 0 {
		t.Fatalf("declined restore reached the server: %v", f.lastRestoreRequest())
	}
	return stderr
}

func TestBackup_RestoreModeIsNormalised(t *testing.T) {
	for _, tc := range []struct {
		flag, sent, out string
	}{
		{"Replace", `"replace"`, "Restore of " + modeTestID + " started (mode: replace)\n"},
		{" MERGE ", `"merge"`, "Restore of " + modeTestID + " started\n"},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			f := newRestoreDefaultsServer(t, true, true)
			out, stderr, err := execCmd(t, newBackupRestoreCmd(), modeTestID, "--mode", tc.flag, "--yes")
			if err != nil {
				t.Fatalf("restore --mode %q: %v (stderr %q)", tc.flag, err, stderr)
			}
			if got := string(f.lastRestoreRequest()["mode"]); got != tc.sent {
				t.Errorf("sent mode = %s, want %s", got, tc.sent)
			}
			if out != tc.out {
				t.Errorf("stdout = %q, want %q", out, tc.out)
			}
			if strings.Contains(stderr, "not confirmed") {
				t.Errorf("normalised mode reported as unconfirmed: %q", stderr)
			}
		})
	}

	t.Run("blank is the same as omitted", func(t *testing.T) {
		f := newRestoreDefaultsServer(t, true, true)
		if _, stderr, err := execCmd(t, newBackupRestoreCmd(), modeTestID, "--mode", " ", "--yes"); err != nil {
			t.Fatalf("restore --mode ' ': %v (stderr %q)", err, stderr)
		}
		if raw, ok := f.lastRestoreRequest()["mode"]; ok {
			t.Errorf("blank --mode sent mode %s; want no mode key", raw)
		}
		omitted := declinedPrompt(t, newRestoreDefaultsServer(t, true, true))
		blank := declinedPrompt(t, newRestoreDefaultsServer(t, true, true), "--mode", " ")
		if blank != omitted {
			t.Errorf("blank --mode prompt differs from the omitted one:\n%q\n%q", blank, omitted)
		}
	})

	t.Run("invalid quotes the raw value", func(t *testing.T) {
		f := newRestoreDefaultsServer(t, true, true)
		_, _, err := execCmd(t, newBackupRestoreCmd(), modeTestID, "--mode", "BOGUS", "--yes")
		if err == nil || err.Error() != `invalid --mode "BOGUS" (valid: merge, replace)` {
			t.Fatalf("err = %v", err)
		}
		if len(f.lastRestoreRequest()) != 0 {
			t.Errorf("invalid mode reached the server: %v", f.lastRestoreRequest())
		}
	})
}

func TestBackup_RestorePromptForMode(t *testing.T) {
	// The prompt for an unscoped backup without --mode is published text:
	// it must not change (legacy rule).
	t.Run("unscoped without mode keeps the published prompt", func(t *testing.T) {
		got := declinedPrompt(t, newRestoreDefaultsServer(t, true, true))
		want := "Restore backup " + modeTestID + " (2026-09-07T20:00:00Z, 1 files, 10 B) over the live storage? Existing files at the same paths are replaced. Metadata restoration follows the server default: enabled on standalone nodes and disabled on clusters. If restored, it is staged until the next server start; use --with-metadata to request it explicitly. Stop writers and compaction first. [y/N] "
		if got != want {
			t.Errorf("prompt = %q\nwant     %q", got, want)
		}
	})

	t.Run("replace states the real effect", func(t *testing.T) {
		got := declinedPrompt(t, newFakeBackupServerWith(t, scopedManifest(false)), "--mode", "REPLACE")
		want := "On a cluster running Arc 27.01.1+, replace first removes the current hot-tier files of database(s) t85_a on every node, including data written after this backup was taken, then restores the backup's files. Standalone servers refuse replace. Older Arc versions ignore --mode and merge instead."
		if !strings.Contains(got, want) {
			t.Errorf("prompt = %q, want it to contain %q", got, want)
		}
		if strings.Contains(got, "manifest entries") || strings.Contains(got, "This backup is scoped") {
			t.Errorf("replace prompt kept old or scoped-default text: %q", got)
		}
	})

	scopedNote := "This backup is scoped to database(s) t85_a. On a cluster running Arc 27.01.1+, a restore without --mode runs as replace"
	incompleteNote := "This backup is incomplete, so on a cluster Arc refuses that replace, but only after accepting the restore"

	for _, args := range [][]string{nil, {"--mode", " "}} {
		t.Run("scoped without mode names the scope "+strings.Join(args, ""), func(t *testing.T) {
			got := declinedPrompt(t, newFakeBackupServerWith(t, scopedManifest(false)), args...)
			if !strings.Contains(got, scopedNote) {
				t.Errorf("prompt = %q, want the scoped note", got)
			}
			if strings.Contains(got, incompleteNote) {
				t.Errorf("complete backup got the incomplete note: %q", got)
			}
		})
	}

	t.Run("scoped note names the scope, not the databases held", func(t *testing.T) {
		manifest := strings.Replace(scopedManifest(false), `"scope":["t85_a"]`, `"scope":["t85_a","t85_b"]`, 1)
		got := declinedPrompt(t, newFakeBackupServerWith(t, manifest))
		if !strings.Contains(got, "This backup is scoped to database(s) t85_a, t85_b.") {
			t.Errorf("prompt = %q, want both scoped databases", got)
		}
		got = declinedPrompt(t, newFakeBackupServerWith(t, manifest), "--mode", "replace")
		if !strings.Contains(got, "hot-tier files of database(s) t85_a, t85_b on every node") {
			t.Errorf("replace prompt = %q, want both scoped databases", got)
		}
	})

	lateCheckNote := "Arc checks the backup's files only after accepting the restore, so without --wait a refused replace is reported as started"
	t.Run("scoped without mode and without wait warns about late checks", func(t *testing.T) {
		got := declinedPrompt(t, newFakeBackupServerWith(t, scopedManifest(false)))
		if !strings.Contains(got, lateCheckNote) {
			t.Errorf("prompt = %q, want the late-check note", got)
		}
		got = declinedPrompt(t, newFakeBackupServerWith(t, scopedManifest(false)), "--wait")
		if strings.Contains(got, lateCheckNote) || !strings.Contains(got, scopedNote) {
			t.Errorf("--wait prompt = %q, want the scoped note without the late-check note", got)
		}
		got = declinedPrompt(t, newFakeBackupServerWith(t, scopedManifest(true)))
		if strings.Contains(got, lateCheckNote) {
			t.Errorf("incomplete prompt = %q, want only the incomplete note", got)
		}
	})

	t.Run("scoped blank mode sends no mode", func(t *testing.T) {
		f := newFakeBackupServerWith(t, scopedManifest(false))
		if _, stderr, err := execCmd(t, newBackupRestoreCmd(), modeTestID, "--mode", " ", "--yes"); err != nil {
			t.Fatalf("restore: %v (stderr %q)", err, stderr)
		}
		if raw, ok := f.lastRestoreRequest()["mode"]; ok {
			t.Errorf("blank --mode sent mode %s; want no mode key", raw)
		}
	})

	t.Run("scoped incomplete without mode warns about the late refusal", func(t *testing.T) {
		got := declinedPrompt(t, newFakeBackupServerWith(t, scopedManifest(true)))
		if !strings.Contains(got, scopedNote) || !strings.Contains(got, incompleteNote) {
			t.Errorf("prompt = %q, want the scoped and incomplete notes", got)
		}
	})

	t.Run("scoped with explicit merge has no scoped note", func(t *testing.T) {
		got := declinedPrompt(t, newFakeBackupServerWith(t, scopedManifest(true)), "--mode", "Merge")
		if strings.Contains(got, "This backup is scoped") || strings.Contains(got, "removes the current") {
			t.Errorf("explicit merge prompt mentions replace: %q", got)
		}
	})
}

func TestBackup_ShowPrintsScope(t *testing.T) {
	newFakeBackupServerWith(t, scopedManifest(false))
	out, _, err := execCmd(t, newBackupShowCmd(), modeTestID)
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if !strings.Contains(out, "type:        full (server dev)\nscope:       t85_a\ncontents:") {
		t.Errorf("show output lacks the scope line: %q", out)
	}
	newRestoreDefaultsServer(t, true, true)
	out, _, err = execCmd(t, newBackupShowCmd(), modeTestID)
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if strings.Contains(out, "scope:") {
		t.Errorf("unscoped show printed a scope line: %q", out)
	}
}

// newFakeBackupServerWith serves manifest as the cluster backup modeTestID.
func newFakeBackupServerWith(t *testing.T, manifest string) *fakeBackupServer {
	t.Helper()
	f := newRestoreDefaultsServer(t, true, false)
	f.backups[modeTestID] = manifest
	return f
}
