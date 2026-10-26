package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/basekick-labs/arcli/internal/client"
)

func serveLicenseHealth(w http.ResponseWriter, block string) {
	body := `{"status":"ok","time":"2026-10-25T12:00:00Z","uptime":"5s"`
	if block != "" {
		body += `,"license":` + block
	}
	fmt.Fprint(w, body+`}`)
}

func TestLicenseStatus_StatesAndFormats(t *testing.T) {
	for _, tc := range []struct{ name, block, status string }{
		{"absent", "", "absent"},
		{"null", "null", "absent"},
		{"active", `{"tier":"enterprise","status":"active","source":"file","expires_at":"2027-01-01T03:00:00.123-03:00","days_remaining":68,"site_license":true}`, "active"},
		{"expired", `{"tier":"enterprise","status":"expired","source":"cache"}`, "expired"},
		{"invalid", `{"tier":"enterprise","status":"invalid","source":"online"}`, "invalid"},
		{"unlicensed", `{"tier":"oss","status":"unlicensed","source":"none"}`, "unlicensed"},
		{"unknown", `{"tier":"future-tier","status":"future-status","source":"future-source"}`, "future-status"},
		{"explicit zero false", `{"tier":"enterprise","status":"active","days_remaining":0,"site_license":false}`, "active"},
	} {
		for _, format := range []string{"table", "json"} {
			t.Run(tc.name+"/"+format, func(t *testing.T) {
				calls := 0
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					if r.Method != http.MethodGet || r.URL.Path != "/health" {
						t.Errorf("unexpected request: %s %s", r.Method, r.URL)
					}
					if r.Header.Get("Authorization") != "" || r.Header.Get(client.HeaderDatabase) != "" {
						t.Error("health received token or database")
					}
					serveLicenseHealth(w, tc.block)
				}))
				defer srv.Close()
				path := writeTestConfig(t, srv.URL, "test-only")
				f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				_, err = fmt.Fprintln(f, `default_database = "metrics"`)
				if closeErr := f.Close(); err == nil {
					err = closeErr
				}
				if err != nil {
					t.Fatal(err)
				}
				out, stderr, err := execCmd(t, NewRoot(BuildInfo{Version: "test"}), "license", "status", "-o", format)
				if err != nil || stderr != "" || calls != 1 {
					t.Fatalf("err=%v stderr=%q calls=%d", err, stderr, calls)
				}
				if format == "table" {
					if !strings.Contains(out, tc.status) || !strings.Contains(out, "configured") {
						t.Fatalf("table=%s", out)
					}
					if tc.name == "active" {
						for _, v := range []string{"enterprise", "file", "2027-01-01T06:00:00.123Z", "68", "true"} {
							if !strings.Contains(out, v) {
								t.Errorf("missing %q: %s", v, out)
							}
						}
					}
					return
				}
				var report struct {
					Configured bool           `json:"configured"`
					License    map[string]any `json:"license"`
				}
				if err := json.Unmarshal([]byte(out), &report); err != nil {
					t.Fatalf("JSON: %v: %s", err, out)
				}
				if tc.status == "absent" {
					if report.Configured || report.License != nil || !strings.Contains(out, `"license": null`) {
						t.Errorf("absent=%s", out)
					}
				} else {
					if !report.Configured || report.License["status"] != tc.status {
						t.Errorf("report=%s", out)
					}
					if tc.name == "active" && report.License["expires_at"] != "2027-01-01T06:00:00.123Z" {
						t.Errorf("UTC JSON=%s", out)
					}
					if tc.name == "explicit zero false" && (report.License["days_remaining"] != float64(0) || report.License["site_license"] != false) {
						t.Errorf("optional fields=%s", out)
					}
					if tc.name == "unlicensed" && len(report.License) != 3 {
						t.Errorf("invented optional fields=%s", out)
					}
				}
			})
		}
	}
}

func TestLicenseStatus_ConnectionResolution(t *testing.T) {
	for _, mode := range []string{"active", "explicit name", "explicit endpoint", "env name", "env endpoint"} {
		t.Run(mode, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "" || r.Header.Get(client.HeaderDatabase) != "" {
					t.Error("credentials/database leaked")
				}
				if r.Header.Get(client.HeaderInstallationID) != "" || !strings.HasPrefix(r.Header.Get("User-Agent"), "arcli/test ") {
					t.Error("identity opt-out or user agent lost")
				}
				serveLicenseHealth(w, "")
			}))
			defer srv.Close()
			path := filepath.Join(t.TempDir(), "config.toml")
			body := fmt.Sprintf("active = %q\nsend_installation_id = false\ninstallation_id = %q\n[connections.local]\nendpoint = %q\ntoken = %q\ndefault_database = %q\n[connections.other]\nendpoint = %q\ntoken = %q\n", "local", "test-installation", srv.URL, "profile-test-only", "metrics", unreachable, "other-test-only")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("ARCLI_CONFIG", path)
			t.Setenv("ARC_CONNECTION", "")
			t.Setenv("ARC_ENDPOINT", "")
			t.Setenv("ARC_TOKEN", "")
			args := []string{"license", "status", "-o", "json", "--timeout", "2s"}
			switch mode {
			case "explicit name":
				t.Setenv("ARC_CONNECTION", "other")
				t.Setenv("ARC_ENDPOINT", unreachable)
				args = append(args, "-c", "local", "--endpoint", unreachable, "--token", "flag-test-only")
			case "explicit endpoint":
				t.Setenv("ARC_CONNECTION", "other")
				args = append(args, "--endpoint", srv.URL+"///")
			case "env name":
				t.Setenv("ARC_CONNECTION", "local")
				t.Setenv("ARC_ENDPOINT", unreachable)
				t.Setenv("ARC_TOKEN", "env-test-only")
			case "env endpoint":
				t.Setenv("ARC_ENDPOINT", srv.URL)
				t.Setenv("ARC_TOKEN", "")
				args = append(args, "--insecure")
			}
			out, stderr, err := execCmd(t, NewRoot(BuildInfo{Version: "test"}), args...)
			if err != nil || stderr != "" || !strings.Contains(out, `"configured": false`) {
				t.Fatalf("out=%s stderr=%s err=%v", out, stderr, err)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != body {
				t.Fatal("read-only command modified config")
			}
		})
	}
}

func TestLicenseStatus_TLS(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serveLicenseHealth(w, "") }))
	defer srv.Close()
	for _, mode := range []string{"verified", "flag", "stored"} {
		t.Run(mode, func(t *testing.T) {
			path := writeTestConfig(t, srv.URL, "test-only")
			args := []string{"license", "status", "-o", "json"}
			if mode == "flag" {
				args = append(args, "--insecure")
			}
			if mode == "stored" {
				b, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				b = []byte(strings.Replace(string(b), "[connections.local]", "[connections.local]\ninsecure_tls = true", 1))
				if err := os.WriteFile(path, b, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			out, stderr, err := execCmd(t, NewRoot(BuildInfo{}), args...)
			if mode == "verified" {
				if err == nil || out != "" {
					t.Fatalf("verified TLS accepted self-signed: out=%s err=%v", out, err)
				}
				return
			}
			if err != nil || !json.Valid([]byte(out)) || !strings.Contains(stderr, "WARNING: TLS certificate verification disabled") {
				t.Fatalf("out=%s stderr=%s err=%v", out, stderr, err)
			}
		})
	}
}

func TestLicenseStatus_FirstRunHelpAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.toml")
	t.Setenv("ARCLI_CONFIG", path)
	t.Setenv("ARC_CONNECTION", "")
	t.Setenv("ARC_ENDPOINT", "")
	t.Setenv("ARC_TOKEN", "")
	for _, args := range [][]string{{"--help"}, {"--version"}, {"license", "--help"}, {"license", "status", "--help"}} {
		if out, _, err := execCmd(t, NewRoot(BuildInfo{Version: "test"}), args...); err != nil || out == "" {
			t.Fatalf("%v: out=%s err=%v", args, out, err)
		}
	}
	for _, args := range [][]string{{"license", "status"}, {"license", "status", "extra"}, {"license", "status", "-o", "csv"}, {"license", "status", "-o", "arrow"}, {"license", "status", "--timeout", "0s"}, {"license", "status", "--token", "test-only"}} {
		if out, _, err := execCmd(t, NewRoot(BuildInfo{}), args...); err == nil || out != "" {
			t.Fatalf("%v: out=%s err=%v", args, out, err)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("first run created config")
	}
	if out := completeArgs(t, "license", "status", "-o", ""); out != "table\njson\n:4\n" {
		t.Errorf("completion=%q", out)
	}
}

func TestLicenseStatus_Failures(t *testing.T) {
	for _, body := range []string{`{}`, `{"status":"ok","time":"t","uptime":"5s","license":{}}`, `{"status":"ok","time":"t","uptime":"5s","license":[]}`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
		writeTestConfig(t, srv.URL, "")
		for _, format := range []string{"table", "json"} {
			if out, _, err := execCmd(t, NewRoot(BuildInfo{}), "license", "status", "-o", format); err == nil || out != "" {
				t.Errorf("out=%s err=%v", out, err)
			}
		}
		srv.Close()
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer srv.Close()
	writeTestConfig(t, srv.URL, "")
	if out, _, err := execCmd(t, NewRoot(BuildInfo{}), "license", "status", "--timeout", "20ms"); err == nil || out != "" {
		t.Errorf("timeout out=%s err=%v", out, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	root := NewRoot(BuildInfo{})
	root.SetContext(ctx)
	if out, _, err := execCmd(t, root, "license", "status"); !errors.Is(err, context.Canceled) || out != "" {
		t.Errorf("cancellation out=%s err=%v", out, err)
	}
}

type licenseFailWriter struct{}

func (licenseFailWriter) Write(p []byte) (int, error) { return 0, errors.New("test output failure") }

func TestLicenseStatus_OutputErrorsAndSanitization(t *testing.T) {
	block := `{"tier":"enterprise\u001b[31m","status":"active\n\u0007","source":"file\u202e"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serveLicenseHealth(w, block) }))
	defer srv.Close()
	writeTestConfig(t, srv.URL, "")
	for _, format := range []string{"table", "json"} {
		out, _, err := execCmd(t, NewRoot(BuildInfo{}), "license", "status", "-o", format)
		if err != nil {
			t.Fatal(err)
		}
		if format == "table" {
			if strings.ContainsAny(out, "\x1b\a\u202e") {
				t.Errorf("unsafe table=%q", out)
			}
		} else {
			var report struct {
				License client.HealthLicense `json:"license"`
			}
			if err := json.Unmarshal([]byte(out), &report); err != nil || report.License.Status != "active\n\a" {
				t.Errorf("JSON corrupted=%s err=%v", out, err)
			}
		}
		cmd := newLicenseStatusCmd()
		cmd.SetOut(licenseFailWriter{})
		cmd.SetErr(licenseFailWriter{})
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		cmd.SetArgs([]string{"-o", format})
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "test output failure") {
			t.Errorf("output error=%v", err)
		}
	}
}

func TestPing_RawLicenseRemainsCompatible(t *testing.T) {
	for _, block := range []string{`{"tier":"enterprise","status":"active","future_field":{"v":1},"expires_at":"not-a-date"}`, `[1,2]`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/health" {
				serveLicenseHealth(w, block)
				return
			}
			http.NotFound(w, r)
		}))
		writeTestConfig(t, srv.URL, "")
		out, _, err := execCmd(t, NewRoot(BuildInfo{}), "ping", "-o", "json")
		if err != nil {
			t.Fatal(err)
		}
		var report struct {
			Health struct {
				License json.RawMessage `json:"license"`
			} `json:"health"`
		}
		if err := json.Unmarshal([]byte(out), &report); err != nil {
			t.Fatal(err)
		}
		var want, got any
		if err := json.Unmarshal([]byte(block), &want); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(report.Health.License, &got); err != nil {
			t.Fatal(err)
		}
		wantJSON, _ := json.Marshal(want)
		gotJSON, _ := json.Marshal(got)
		if string(wantJSON) != string(gotJSON) {
			t.Errorf("ping license=%s want=%s", gotJSON, wantJSON)
		}
		srv.Close()
	}
}
