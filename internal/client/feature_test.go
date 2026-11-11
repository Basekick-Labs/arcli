package client

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func TestFeatureJSONMissingRoutePreservesHTTPError(t *testing.T) {
	const message = "Cannot GET /api/v1/unavailable"
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_, _ = w.Write([]byte(`{"status":"ok","time":"t","uptime":"1s"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"` + message + `"}`))
	})
	_, err := cli.featureJSON(context.Background(), "test feature", "test.enabled", http.MethodGet, "/api/v1/unavailable", nil, 1024, nil)
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("expected inspectable HTTPError, got %T: %v", err, err)
	}
	if httpErr.Status != http.StatusNotFound || httpErr.Message != message {
		t.Fatalf("HTTP error changed: %+v", httpErr)
	}
	var disabled *FeatureDisabledError
	if errors.As(err, &disabled) {
		t.Fatalf("missing route inferred disabled feature: %v", err)
	}
}

func TestFeatureJSONPreservesExplicitServerCause(t *testing.T) {
	const message = "This feature requires an enterprise license"
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"` + message + `"}`))
	})
	_, err := cli.featureJSON(context.Background(), "test feature", "test.enabled", http.MethodGet, "/api/v1/unavailable", nil, 1024, nil)
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != http.StatusForbidden || httpErr.Message != message {
		t.Fatalf("explicit server cause lost: %v", err)
	}
}
