package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIdentityHeaders(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	// Configured: both headers, on a per-database endpoint and on a
	// cross-database one.
	cli, err := New(Config{Endpoint: srv.URL, Token: "t", UserAgent: "arcli/9.9.9 (test/arch)", InstallationID: "0f0f0f0f-0f0f-4f0f-8f0f-0f0f0f0f0f0f"})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = cli.Health(context.Background())
	if got.Get("User-Agent") != "arcli/9.9.9 (test/arch)" || got.Get(HeaderInstallationID) != "0f0f0f0f-0f0f-4f0f-8f0f-0f0f0f0f0f0f" {
		t.Errorf("cross-db headers: %v", got)
	}
	_ = cli.WriteLineProtocol(context.Background(), nil, "db", "")
	if got.Get("User-Agent") != "arcli/9.9.9 (test/arch)" || got.Get(HeaderInstallationID) == "" {
		t.Errorf("per-db headers: %v", got)
	}

	// Not configured: plain User-Agent, no id header at all.
	cli, _ = New(Config{Endpoint: srv.URL, Token: "t"})
	_, _ = cli.Health(context.Background())
	if got.Get("User-Agent") != "arcli" || got.Get(HeaderInstallationID) != "" {
		t.Errorf("unconfigured headers: %v", got)
	}
}

// A connection without a token sends no Authorization header at all
// (a server with auth.enabled = false accepts the request; one with
// auth answers a clear 401 rather than a malformed-header error).
func TestNoTokenSendsNoAuthorizationHeader(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()
	cli, err := New(Config{Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = cli.Health(context.Background())
	if _, present := got["Authorization"]; present {
		t.Errorf("cross-db request must carry no Authorization header: %v", got)
	}
	_ = cli.WriteLineProtocol(context.Background(), nil, "db", "")
	if _, present := got["Authorization"]; present {
		t.Errorf("per-db request must carry no Authorization header: %v", got)
	}
	// With a token the header is back (on an authenticated endpoint;
	// /health is public and never carries one).
	cli, _ = New(Config{Endpoint: srv.URL, Token: "t"})
	_ = cli.WriteLineProtocol(context.Background(), nil, "db", "")
	if got.Get("Authorization") != "Bearer t" {
		t.Errorf("with token: %v", got)
	}
}
