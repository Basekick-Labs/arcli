package commands

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/basekick-labs/arcli/internal/config"
)

// Exercise the command gates with real file descriptors as well as prompt
// answers: a reader containing "yes" must not authorize a piped invocation.
func confirmationInput(t *testing.T, kind string) io.Reader {
	t.Helper()
	switch kind {
	case "decline":
		return strings.NewReader("n\n")
	case "eof":
		return strings.NewReader("")
	case "accept":
		return strings.NewReader("yes\n")
	case "pipe":
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { r.Close() })
		t.Cleanup(func() { w.Close() })
		if _, err := w.WriteString("yes\n"); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		return r
	case "file":
		path := t.TempDir() + "/stdin"
		if err := os.WriteFile(path, []byte("yes\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
		return f
	case "null":
		f, err := os.Open(os.DevNull)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
		return f
	default:
		t.Fatalf("unknown input kind %q", kind)
		return nil
	}
}

func TestDestructiveCommandConfirmation(t *testing.T) {
	const nonInteractive = "confirmation required but stdin is not a terminal; pass --yes"
	for _, command := range []string{"db", "config"} {
		for _, tc := range []struct {
			input   string
			yes     bool
			wantErr string
		}{
			{input: "decline", wantErr: "aborted"},
			{input: "eof", wantErr: "aborted"},
			{input: "pipe", wantErr: nonInteractive},
			{input: "file", wantErr: nonInteractive},
			{input: "null", wantErr: nonInteractive},
			{input: "accept"},
			{input: "decline", yes: true},
			{input: "pipe", yes: true},
			{input: "file", yes: true},
			{input: "null", yes: true},
		} {
			name := command + "/" + tc.input
			if tc.yes {
				name += "/--yes"
			}
			t.Run(name, func(t *testing.T) {
				var requests atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					if r.Method != http.MethodDelete || r.URL.Path != "/api/v1/databases/staging" || r.URL.RawQuery != "confirm=true" {
						t.Errorf("unexpected request: %s %s", r.Method, r.URL)
					}
					if r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("x-arc-database") != "" {
						t.Error("delete must preserve authentication and omit the profile database header")
					}
					w.WriteHeader(http.StatusNoContent)
				}))
				defer srv.Close()
				path := writeTestConfig(t, srv.URL, "tok")
				cfg, err := config.Load()
				if err != nil {
					t.Fatal(err)
				}
				conn := cfg.Connections["local"]
				conn.DefaultDatabase = "metrics"
				cfg.Connections["local"] = conn
				if err := cfg.Save(); err != nil {
					t.Fatal(err)
				}
				before, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				args := []string{"db", "drop", "staging"}
				wantOut := "Deleted database \"staging\"\n"
				question := "Delete database \"staging\" and ALL its files? [y/N] "
				if command == "config" {
					args = []string{"config", "delete", "local"}
					wantOut = "Deleted connection \"local\"\n"
					question = "Delete connection \"local\"? [y/N] "
				}
				if tc.yes {
					args = append(args, "--yes")
				}
				root := NewRoot(BuildInfo{Version: "test"})
				root.SetIn(confirmationInput(t, tc.input))
				out, errOut, err := execCmd(t, root, args...)
				if tc.wantErr != "" {
					if err == nil || err.Error() != tc.wantErr {
						t.Fatalf("error = %v, want %q", err, tc.wantErr)
					}
					wantOut = ""
				} else if err != nil {
					t.Fatal(err)
				}
				if out != wantOut {
					t.Errorf("stdout = %q, want %q", out, wantOut)
				}
				wantPrompt := ""
				if !tc.yes && (tc.input == "decline" || tc.input == "eof" || tc.input == "accept") {
					wantPrompt = question
				}
				if errOut != wantPrompt {
					t.Errorf("stderr = %q, want %q", errOut, wantPrompt)
				}
				wantRequests := int32(0)
				if command == "db" && tc.wantErr == "" {
					wantRequests = 1
				}
				if got := requests.Load(); got != wantRequests {
					t.Errorf("requests = %d, want %d", got, wantRequests)
				}
				after, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if command == "db" || tc.wantErr != "" {
					if !bytes.Equal(before, after) {
						t.Error("command must leave the config file unchanged")
					}
				} else {
					cfg, err := config.Load()
					if err != nil {
						t.Fatal(err)
					}
					if _, exists := cfg.Connections["local"]; exists || cfg.Active != "" {
						t.Error("successful deletion must remove the active profile and clear active")
					}
					if len(cfg.Connections) != 2 || cfg.Connections["twin"].Endpoint != srv.URL || cfg.Connections["other"].Token != "other-secret" {
						t.Error("successful deletion must preserve other profiles")
					}
				}
			})
		}
	}
}

// A real tiering migration moves files to cold storage, so it follows the
// same gate as the commands above; --dry-run never prompts.
func TestTieringMigrateConfirmation(t *testing.T) {
	const nonInteractive = "confirmation required but stdin is not a terminal; pass --yes"
	const question = "Move eligible files from hot to cold storage (database \"metrics\")? Arc 26.09 and earlier ignore these filters and migrate every eligible file. [y/N] "
	for _, tc := range []struct {
		input   string
		yes     bool
		dryRun  bool
		wantErr string
	}{
		{input: "decline", wantErr: "aborted"},
		{input: "eof", wantErr: "aborted"},
		{input: "pipe", wantErr: nonInteractive},
		{input: "file", wantErr: nonInteractive},
		{input: "null", wantErr: nonInteractive},
		{input: "accept"},
		{input: "decline", yes: true},
		{input: "pipe", yes: true},
		{input: "file", yes: true},
		{input: "null", yes: true},
		{input: "pipe", dryRun: true},
		{input: "null", dryRun: true},
	} {
		name := tc.input
		if tc.yes {
			name += "/--yes"
		}
		if tc.dryRun {
			name += "/--dry-run"
		}
		t.Run(name, func(t *testing.T) {
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/api/v1/tiering/migrate" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
				}
				var req map[string]any
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Errorf("decode request: %v", err)
				}
				if req["database"] != "metrics" || (req["dry_run"] == true) != tc.dryRun {
					t.Errorf("request = %v", req)
				}
				if tc.dryRun {
					_, _ = w.Write([]byte(`{"status":"dry_run","would_migrate":3,"total_bytes":3018,"from_tier":"hot","to_tier":"cold"}`))
					return
				}
				_, _ = w.Write([]byte(`{"message":"Migration completed successfully","status":"completed","migrated":3,"errors":0}`))
			}))
			defer srv.Close()
			writeTestConfig(t, srv.URL, "tok")
			args := []string{"tiering", "migrate", "--database", "metrics", "-o", "json"}
			if tc.yes {
				args = append(args, "--yes")
			}
			if tc.dryRun {
				args = append(args, "--dry-run")
			}
			root := NewRoot(BuildInfo{Version: "test"})
			root.SetIn(confirmationInput(t, tc.input))
			out, errOut, err := execCmd(t, root, args...)
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				if out != "" {
					t.Errorf("stdout = %q, want empty", out)
				}
			} else if err != nil {
				t.Fatal(err)
			} else if !strings.Contains(out, `"status": "`) {
				t.Errorf("stdout = %q", out)
			}
			wantPrompt := ""
			if !tc.yes && !tc.dryRun && (tc.input == "decline" || tc.input == "eof" || tc.input == "accept") {
				wantPrompt = question
			}
			if errOut != wantPrompt {
				t.Errorf("stderr = %q, want %q", errOut, wantPrompt)
			}
			wantRequests := int32(0)
			if tc.wantErr == "" {
				wantRequests = 1
			}
			if got := requests.Load(); got != wantRequests {
				t.Errorf("requests = %d, want %d", got, wantRequests)
			}
		})
	}
}
