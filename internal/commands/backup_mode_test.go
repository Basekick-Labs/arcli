package commands

import (
	"net/http"
	"net/http/httptest"
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

	scopedNote := "This backup is scoped to database(s) t85_a. On a cluster running Arc 27.01.1+ (with cluster.raft_data_dir), a restore without --mode runs as replace"
	incompleteNote := "This backup is incomplete, so on a cluster Arc refuses that replace, but only after accepting the restore"
	lateCheckNote := "Arc checks the backup's files only after accepting the restore, so without --wait a refused replace is reported as started"

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

	// Arc's CheckScopedReplace: a scope name with no data files in the
	// backup makes any replace a synchronous 400.
	missingScope := strings.Replace(scopedManifest(false), `"scope":["t85_a"]`, `"scope":["t85_a","t85_b"]`, 1)
	t.Run("explicit replace of a scope without files is refused before the prompt", func(t *testing.T) {
		f := newFakeBackupServerWith(t, missingScope)
		_, _, err := execCmd(t, newBackupRestoreCmd(), modeTestID, "--mode", "replace", "--yes")
		want := `restore refused: Arc refuses --mode replace for backup ` + modeTestID + `: it holds no data files for database "t85_b", so replace would only remove its current files and restore nothing; use --mode merge, or take the backup again once the database has hot files`
		if err == nil || err.Error() != want {
			t.Fatalf("err = %v\nwant  %s", err, want)
		}
		if len(f.lastRestoreRequest()) != 0 {
			t.Errorf("refused replace reached the server: %v", f.lastRestoreRequest())
		}
	})
	t.Run("omitted mode on a scope without files warns about the 400", func(t *testing.T) {
		got := declinedPrompt(t, newFakeBackupServerWith(t, missingScope))
		want := `This backup is scoped to database(s) t85_a, t85_b but holds no data files for "t85_b". On a cluster running Arc 27.01.1+, a restore without --mode would run as replace, which Arc refuses with HTTP 400 for such a backup; pass --mode merge.`
		if !strings.Contains(got, want) || strings.Contains(got, "runs as replace") || strings.Contains(got, lateCheckNote) {
			t.Errorf("prompt = %q, want only the 400 note", got)
		}
		if got := declinedPrompt(t, newFakeBackupServerWith(t, missingScope), "--mode", "merge"); strings.Contains(got, "HTTP 400") {
			t.Errorf("explicit merge prompt warns about the replace 400: %q", got)
		}
	})

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
		if !strings.Contains(got, scopedNote) || !strings.Contains(got, incompleteNote+" and pausing compaction. Without --wait arcli reports it as started and exits 0. Pass --mode merge, or --wait to see the outcome.") {
			t.Errorf("prompt = %q, want the scoped and incomplete notes", got)
		}
		got = declinedPrompt(t, newFakeBackupServerWith(t, scopedManifest(true)), "--wait")
		if !strings.Contains(got, incompleteNote+" and pausing compaction. Pass --mode merge to restore it.") || strings.Contains(got, "without --wait") || strings.Contains(got, "Without --wait") {
			t.Errorf("--wait prompt = %q, want the incomplete note without the --wait advice", got)
		}
	})

	t.Run("unscoped replace without databases says nothing is removed", func(t *testing.T) {
		got := declinedPrompt(t, newRestoreDefaultsServer(t, true, false), "--mode", "replace")
		if !strings.Contains(got, "On a cluster running Arc 27.01.1+, replace removes nothing for this backup, which holds no databases, then restores its files.") || strings.Contains(got, "these databases") {
			t.Errorf("prompt = %q", got)
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

// unregisteredManifest is an unscoped cluster backup whose node listed
// total-aux+unreg data files, unreg of them missing from its cluster manifest.
func unregisteredManifest(total, aux, unreg int64, scoped bool) string {
	scope := ""
	if scoped {
		scope = `"scope":["t85_a"],`
	}
	return `{"version":"dev","backup_id":"` + modeTestID + `","created_at":"2026-09-07T20:00:00Z","backup_type":"full",` + scope + `"databases":[{"name":"t85_a","measurements":[],"file_count":1,"size_bytes":10}],"total_files":` + itoa(total) + `,"auxiliary_files":` + itoa(aux) + `,"unregistered_skipped":` + itoa(unreg) + `,"total_size_bytes":10,"has_metadata":false,"has_config":false}`
}

func TestBackup_RestoreMirrorsUnregisteredRatio(t *testing.T) {
	// Arc restore.go:1642-1643: listed = 19-9+2 = 12, 2 > 1.2 refuses.
	// Ignoring auxiliary_files (listed 21, 2 > 2.1) would accept it.
	f := newFakeBackupServerWith(t, unregisteredManifest(19, 9, 2, false))
	_, _, err := execCmd(t, newBackupRestoreCmd(), modeTestID, "--mode", "replace", "--yes")
	want := "restore refused: --mode replace requires a backup taken against a caught-up cluster manifest, and 2 of the 12 data files the node that took backup " + modeTestID + " listed were not in its cluster manifest (>10%); use --mode merge, or take the backup again on a caught-up primary"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v\nwant  %s", err, want)
	}
	if len(f.lastRestoreRequest()) != 0 {
		t.Errorf("refused replace reached the server: %v", f.lastRestoreRequest())
	}
	// Boundary: listed = 10-1+1 = 10, 1 > 1.0 is false, so Arc accepts.
	f = newFakeBackupServerWith(t, unregisteredManifest(10, 1, 1, false))
	if _, stderr, err := execCmd(t, newBackupRestoreCmd(), modeTestID, "--mode", "replace", "--yes"); err != nil {
		t.Fatalf("restore at the ratio boundary: %v (stderr %q)", err, stderr)
	}
	// A scoped backup with no --mode gets the incomplete note for it.
	got := declinedPrompt(t, newFakeBackupServerWith(t, unregisteredManifest(19, 9, 2, true)))
	if !strings.Contains(got, "This backup is incomplete, so on a cluster Arc refuses that replace") {
		t.Errorf("prompt = %q, want the incomplete note", got)
	}
}

func TestBackup_RestoreReplaceWarnsAfterAcceptWithoutWait(t *testing.T) {
	const warning = "warning: Arc checks a replace restore against the backup's files only after accepting it; if it refuses, `arcli backup status` shows the failure. Re-run with --wait to wait for the outcome.\n"
	newRestoreDefaultsServer(t, true, true)
	out, stderr, err := execCmd(t, newBackupRestoreCmd(), modeTestID, "--mode", "replace", "--yes")
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !strings.Contains(stderr, warning) {
		t.Errorf("stderr = %q, want the post-accept warning", stderr)
	}
	if out != "Restore of "+modeTestID+" started (mode: replace)\n" {
		t.Errorf("stdout changed: %q", out)
	}
	for _, args := range [][]string{{"--mode", "replace", "--wait"}, {"--mode", "merge"}, {}} {
		newRestoreDefaultsServer(t, true, true)
		// The fake's status echoes no mode, so the replace --wait run fails
		// after waiting; only its stderr before that matters here.
		_, stderr, err := execCmd(t, newBackupRestoreCmd(), append([]string{modeTestID, "--yes"}, args...)...)
		if err != nil && !strings.Contains(err.Error(), "not confirmed by final status") {
			t.Fatalf("restore %v: %v", args, err)
		}
		if strings.Contains(stderr, "only after accepting it") {
			t.Errorf("restore %v printed the post-accept warning: %q", args, stderr)
		}
	}
}

func TestBackup_ListMarksScopedBackups(t *testing.T) {
	srv := newScopedListServer(t)
	writeTestConfig(t, srv, "tok")
	out, _, err := execCmd(t, newBackupListCmd())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out, "│ full (scoped) │") || !strings.Contains(out, "│ full          │") {
		t.Errorf("table = %q, want the scoped backup marked and the unscoped one plain", out)
	}
	out, _, err = execCmd(t, newBackupListCmd(), "-o", "csv")
	if err != nil {
		t.Fatalf("list csv: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.HasSuffix(lines[0], ",skipped_reconciled,scope") || !strings.HasSuffix(lines[1], `,0,"t85_a,t85_b"`) || !strings.HasSuffix(lines[2], ",0,") {
		t.Errorf("csv = %q", lines)
	}
}

// newScopedListServer lists one scoped and one unscoped backup.
func newScopedListServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"backups":[{"backup_id":"backup-20260907-200002-00000002","created_at":"2026-09-07T20:00:02Z","backup_type":"full","scope":["t85_a","t85_b"],"total_files":2,"total_size_bytes":20,"database_count":2},{"backup_id":"backup-20260907-200001-00000001","created_at":"2026-09-07T20:00:01Z","backup_type":"full","total_files":1,"total_size_bytes":10,"database_count":1}],"count":2}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}
