package client

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// routeMissingArc emulates an Arc server whose /health answers but which
// did not register the requested feature routes.
func routeMissingArc(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/health" {
		_, _ = w.Write([]byte(`{"status":"ok","time":"t","uptime":"1s"}`))
		return
	}
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`{"error":"Cannot ` + r.Method + ` ` + r.URL.Path + `"}`))
}

// The published messages of the legacy feature groups are pinned exactly:
// operator scripts match on them.
func TestFeatureRouteMissing_LegacyMessagesPinned(t *testing.T) {
	cli, _ := newAuthTestClient(t, routeMissingArc)
	ctx := context.Background()
	_, _, retErr := cli.ListRetentionPolicies(ctx)
	_, _, cqErr := cli.ListContinuousQueries(ctx, "", nil)
	_, _, backupErr := cli.ListBackups(ctx)
	cases := []struct {
		name, want string
		err        error
	}{
		{"retention", "retention policies are disabled on this server (retention.enabled=false)", retErr},
		{"cq", "continuous queries are disabled on this server (continuous_query.enabled=false)", cqErr},
		{"backup", "backups are disabled on this server (backup.enabled=false); if backup.enabled is true, the backup manager failed to start (see the server log)", backupErr},
	}
	for _, tc := range cases {
		var fde *FeatureDisabledError
		if !errors.As(tc.err, &fde) {
			t.Fatalf("%s: want *FeatureDisabledError, got %T: %v", tc.name, tc.err, tc.err)
		}
		if got := tc.err.Error(); got != tc.want {
			t.Errorf("%s: message changed\n got: %q\nwant: %q", tc.name, got, tc.want)
		}
		// commands/backup.go treats an *HTTPError 404 as "backup deleted
		// concurrently"; a route-missing legacy error must never be one.
		var he *HTTPError
		if errors.As(tc.err, &he) {
			t.Errorf("%s: legacy error must not unwrap to *HTTPError", tc.name)
		}
	}
	// The backup note only extends the legacy text: substring matches hold.
	if !strings.HasPrefix(backupErr.Error(), "backups are disabled on this server (backup.enabled=false)") {
		t.Errorf("backup message no longer starts with the legacy text: %q", backupErr.Error())
	}
}

// Feature groups outside the legacy set keep the server's *HTTPError and
// get a neutral hint; they never claim a configuration value.
func TestFeatureRouteMissing_NonLegacyKeepsHTTPError(t *testing.T) {
	cli, _ := newAuthTestClient(t, routeMissingArc)
	_, err := cli.featureJSON(context.Background(), "tiered storage", "tiered_storage.enabled", http.MethodGet, "/api/v1/tiering/files", nil, 1024, nil)
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != http.StatusNotFound || he.Message != "Cannot GET /api/v1/tiering/files" {
		t.Fatalf("want wrapped *HTTPError 404, got %T: %v", err, err)
	}
	var fde *FeatureDisabledError
	if errors.As(err, &fde) {
		t.Fatalf("non-legacy group must not be FeatureDisabledError: %v", err)
	}
	want := "arc: Cannot GET /api/v1/tiering/files (HTTP 404); Arc is reachable but did not register this route; check tiered_storage.enabled, the license, the server log, and that this Arc version supports this command"
	if err.Error() != want {
		t.Errorf("message\n got: %q\nwant: %q", err.Error(), want)
	}
	if strings.Contains(err.Error(), "enabled=false") {
		t.Errorf("hint must not assert a configuration value: %q", err.Error())
	}
}

func TestFeatureRouteMissing_NonLegacyEmptyConfigKey(t *testing.T) {
	cli, _ := newAuthTestClient(t, routeMissingArc)
	_, err := cli.featureJSON(context.Background(), "MQTT subscriptions", "", http.MethodGet, "/api/v1/mqtt/subscriptions", nil, 1024, nil)
	want := "arc: Cannot GET /api/v1/mqtt/subscriptions (HTTP 404); Arc is reachable but did not register this route; check the license, the server log, and that this Arc version supports this command"
	if err == nil || err.Error() != want {
		t.Errorf("message\n got: %v\nwant: %q", err, want)
	}
}

// When /health fails the endpoint is suspect: report that, without the
// route hint, and keep the *HTTPError inspectable.
func TestFeatureRouteMissing_NonLegacyWrongEndpoint(t *testing.T) {
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"Cannot ` + r.Method + ` ` + r.URL.Path + `"}`))
	})
	_, err := cli.featureJSON(context.Background(), "tiered storage", "tiered_storage.enabled", http.MethodGet, "/api/v1/tiering/files", nil, 1024, nil)
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != http.StatusNotFound {
		t.Fatalf("want wrapped *HTTPError 404, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "check the endpoint path") || strings.Contains(err.Error(), "did not register this route") {
		t.Errorf("wrong-endpoint message: %q", err.Error())
	}
}

// Errors that are not route-missing pass through untouched.
func TestFeatureRouteMissing_OtherErrorsUnchanged(t *testing.T) {
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"This feature requires an enterprise license"}`))
	})
	_, err := cli.featureJSON(context.Background(), "tiered storage", "tiered_storage.enabled", http.MethodGet, "/api/v1/tiering/files", nil, 1024, nil)
	if err == nil || err.Error() != "arc: This feature requires an enterprise license (HTTP 403)" {
		t.Errorf("err = %v", err)
	}
}
