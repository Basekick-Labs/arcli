package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func licenseHealthBody(block string) string {
	base := `{"status":"ok","time":"2026-10-25T12:00:00Z","uptime":"5s"`
	if block != "" {
		base += `,"license":` + block
	}
	return base + `}`
}

func TestLicenseStatus_Shapes(t *testing.T) {
	for _, tc := range []struct {
		name, block, status string
	}{
		{"absent", "", ""},
		{"null", "null", ""},
		{"active", `{"tier":"enterprise","status":"active","source":"file","expires_at":"2027-01-01T03:00:00.123-03:00","days_remaining":68,"site_license":true}`, "active"},
		{"expired", `{"tier":"enterprise","status":"expired","source":"cache","expires_at":"2020-01-01T00:00:00Z"}`, "expired"},
		{"invalid", `{"tier":"enterprise","status":"invalid","source":"online"}`, "invalid"},
		{"unlicensed", `{"tier":"oss","status":"unlicensed","source":"none"}`, "unlicensed"},
		{"empty source", `{"tier":"oss","status":"unlicensed","source":""}`, "unlicensed"},
		{"future status", `{"tier":"enterprise","status":"future","unknown":{"v":1}}`, "future"},
		{"explicit zero and false", `{"tier":"enterprise","status":"active","days_remaining":0,"site_license":false,"expires_at":null}`, "active"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodGet || r.URL.Path != "/health" || r.URL.RawQuery != "" {
					t.Errorf("request = %s %s", r.Method, r.URL)
				}
				if r.Header.Get("Authorization") != "" || r.Header.Get(HeaderDatabase) != "" {
					t.Error("public health received authorization or database header")
				}
				if r.Header.Get("Accept") != "application/json" || r.Header.Get("User-Agent") != "arcli/test" || r.Header.Get(HeaderInstallationID) != "test-installation" {
					t.Error("missing identity/accept headers")
				}
				fmt.Fprint(w, licenseHealthBody(tc.block))
			}))
			defer srv.Close()
			cli, err := New(Config{Endpoint: srv.URL + "///", Token: "test-only", Database: "metrics", UserAgent: "arcli/test", InstallationID: "test-installation"})
			if err != nil {
				t.Fatal(err)
			}
			got, err := cli.LicenseStatus(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 || got.Configured != (tc.status != "") {
				t.Fatalf("calls=%d report=%+v", calls, got)
			}
			if tc.status == "" {
				b, _ := json.Marshal(got)
				if string(b) != `{"configured":false,"license":null}` {
					t.Errorf("JSON = %s", b)
				}
				return
			}
			if got.License == nil || got.License.Status != tc.status {
				t.Fatalf("license = %+v", got.License)
			}
			if tc.name == "active" {
				if got.License.ExpiresAt.Format(time.RFC3339Nano) != "2027-01-01T06:00:00.123Z" || *got.License.DaysRemaining != 68 || !*got.License.SiteLicense {
					t.Fatalf("license = %+v", got.License)
				}
			} else if tc.name == "explicit zero and false" {
				if got.License.DaysRemaining == nil || *got.License.DaysRemaining != 0 || got.License.SiteLicense == nil || *got.License.SiteLicense || got.License.ExpiresAt != nil {
					t.Fatalf("optional fields = %+v", got.License)
				}
			} else if got.License.DaysRemaining != nil || got.License.SiteLicense != nil {
				t.Errorf("invented optional fields: %+v", got.License)
			}
		})
	}
}

func TestLicenseStatus_Malformed(t *testing.T) {
	for _, block := range []string{`{}`, `[]`, `true`, `"active"`, `{"tier":"enterprise"}`, `{"status":"active"}`, `{"tier":null,"status":"active"}`, `{"tier":"enterprise","status":""}`, `{"tier":12,"status":"active"}`, `{"tier":"enterprise","status":"active","source":42}`, `{"tier":"enterprise","status":"active","expires_at":"tomorrow"}`, `{"tier":"enterprise","status":"active","days_remaining":"5"}`, `{"tier":"enterprise","status":"active","site_license":1}`} {
		t.Run(block, func(t *testing.T) {
			cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, licenseHealthBody(block)) })
			got, err := cli.LicenseStatus(context.Background())
			if err == nil || got != nil || !strings.Contains(err.Error(), "license") {
				t.Fatalf("report=%+v err=%v", got, err)
			}
		})
	}
}

func TestLicenseStatus_HealthFailures(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		code       int
	}{
		{"http error", `{"error":"unavailable"}`, 503},
		{"non Arc", `{"status":"ok"}`, 200},
		{"server failure", `{"status":"error","time":"t","uptime":"5s"}`, 200},
		{"malformed", `<html>proxy</html>`, 200},
		{"truncated", `{"status":"ok","time":"t","uptime":"5s","license":`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.code); fmt.Fprint(w, tc.body) })
			if got, err := cli.LicenseStatus(context.Background()); err == nil || got != nil {
				t.Fatalf("report=%+v err=%v", got, err)
			}
		})
	}
}

func TestLicenseStatus_TruncatedStream(t *testing.T) {
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "9999")
		fmt.Fprint(w, licenseHealthBody(""))
	})
	if got, err := cli.LicenseStatus(context.Background()); err == nil || got != nil {
		t.Fatalf("truncated stream: report=%+v err=%v", got, err)
	}
}

func TestLicenseStatus_CancellationAndTimeout(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(fmt.Sprint(timeout), func(t *testing.T) {
			started := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done() }))
			defer srv.Close()
			cli, err := New(Config{Endpoint: srv.URL})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			if timeout {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 50*time.Millisecond)
			}
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := cli.LicenseStatus(ctx); done <- err }()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("request did not start")
			}
			if !timeout {
				cancel()
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("expected cancellation/timeout")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("request did not stop")
			}
		})
	}
}

func TestLicenseStatus_RedirectNoToken(t *testing.T) {
	for _, sameOrigin := range []bool{false, true} {
		t.Run(fmt.Sprint(sameOrigin), func(t *testing.T) {
			seen := false
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = true
				if r.Header.Get("Authorization") != "" || r.Header.Get(HeaderDatabase) != "" {
					t.Error("redirect received credentials/database")
				}
				fmt.Fprint(w, licenseHealthBody(""))
			}))
			defer target.Close()
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "" {
					t.Error("origin received token")
				}
				if sameOrigin && r.URL.Path == "/redirected" {
					seen = true
					fmt.Fprint(w, licenseHealthBody(""))
					return
				}
				location := target.URL
				if sameOrigin {
					location = "/redirected"
				}
				http.Redirect(w, r, location, http.StatusFound)
			}))
			defer origin.Close()
			cli, err := New(Config{Endpoint: origin.URL, Token: "test-only", Database: "metrics"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := cli.LicenseStatus(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !seen {
				t.Fatal("redirect not followed")
			}
		})
	}
}
