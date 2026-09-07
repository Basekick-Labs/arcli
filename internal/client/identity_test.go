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
