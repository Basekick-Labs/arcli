package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

const governancePolicyJSON = `{"id":4,"token_id":12,"token_name":"batch","rate_limit_per_minute":30,"rate_limit_per_hour":600,"max_queries_per_hour":500,"max_queries_per_day":5000,"max_rows_per_query":10000,"max_scan_duration_sec":60,"created_at":"2026-10-27T10:00:00Z","updated_at":"2026-10-27T10:00:00Z"}`

func TestGovernanceListAndNullNormalization(t *testing.T) {
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertAuthHeaders(t, r)
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/governance/policies" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		_, _ = w.Write([]byte(`{"success":true,"policies":null,"count":0}`))
	})
	policies, err := cli.ListGovernancePolicies(context.Background())
	if err != nil || policies == nil || len(policies) != 0 {
		t.Fatalf("policies=%#v err=%v", policies, err)
	}
}

func TestGovernanceCreateValidatesAndDecodes(t *testing.T) {
	var got GovernancePolicyRequest
	requests := 0
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertAuthHeaders(t, r)
		requests++
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/governance/policies" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"success":true,"policy":` + governancePolicyJSON + `}`))
	})
	base := GovernancePolicyRequest{TokenID: 12, RateLimitPerMinute: 30, RateLimitPerHour: 600, MaxQueriesPerHour: 500, MaxQueriesPerDay: 5000, MaxRowsPerQuery: 10000, MaxScanDurationSec: 60}
	for _, tc := range []struct {
		name string
		req  GovernancePolicyRequest
	}{
		{"zero token", GovernancePolicyRequest{}},
		{"negative token", GovernancePolicyRequest{TokenID: -1}},
		{"negative minute limit", GovernancePolicyRequest{TokenID: 12, RateLimitPerMinute: -1}},
		{"negative hour limit", GovernancePolicyRequest{TokenID: 12, RateLimitPerHour: -1}},
		{"negative hourly query quota", GovernancePolicyRequest{TokenID: 12, MaxQueriesPerHour: -1}},
		{"negative query quota", GovernancePolicyRequest{TokenID: 12, MaxQueriesPerDay: -1}},
		{"negative row limit", GovernancePolicyRequest{TokenID: 12, MaxRowsPerQuery: -1}},
		{"negative scan limit", GovernancePolicyRequest{TokenID: 12, MaxScanDurationSec: -1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := cli.CreateGovernancePolicy(context.Background(), tc.req); err == nil {
				t.Error("invalid request should fail before HTTP")
			}
		})
	}
	policy, err := cli.CreateGovernancePolicy(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 || policy.ID != 4 || policy.TokenID != 12 || policy.TokenName != "batch" || policy.MaxRowsPerQuery != 10000 {
		t.Errorf("requests=%d policy=%+v", requests, policy)
	}
	if got != base {
		t.Errorf("request body=%+v, want %+v", got, base)
	}
}

func TestGovernanceGetUpdateDeleteAndUsage(t *testing.T) {
	var methods []string
	var put GovernancePolicyRequest
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertAuthHeaders(t, r)
		methods = append(methods, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/governance/policies/12":
			_, _ = w.Write([]byte(`{"success":true,"policy":` + governancePolicyJSON + `}`))
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/governance/policies/12":
			if err := json.NewDecoder(r.Body).Decode(&put); err != nil {
				t.Error(err)
			}
			_, _ = w.Write([]byte(`{"success":true,"policy":` + governancePolicyJSON + `}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/governance/policies/12":
			_, _ = w.Write([]byte(`{"success":true,"message":"Policy deleted"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/governance/usage/12":
			_, _ = w.Write([]byte(`{"success":true,"usage":{"token_id":12,"queries_this_hour":7,"queries_this_day":42,"hour_reset_at":"2026-10-27T11:00:00Z","day_reset_at":"2026-10-28T00:00:00Z","rate_limit_remaining_per_minute":23,"rate_limit_remaining_per_hour":593},"policy":` + governancePolicyJSON + `}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ctx := context.Background()
	policy, err := cli.GetGovernancePolicy(ctx, 12)
	if err != nil || policy.TokenName != "batch" {
		t.Fatalf("policy=%+v err=%v", policy, err)
	}
	updated, err := cli.UpdateGovernancePolicy(ctx, 12, GovernancePolicyRequest{RateLimitPerMinute: 45})
	if err != nil || updated.ID != 4 {
		t.Fatalf("updated=%+v err=%v", updated, err)
	}
	if put.TokenID != 12 || put.RateLimitPerMinute != 45 {
		t.Errorf("PUT body=%+v", put)
	}
	if err := cli.DeleteGovernancePolicy(ctx, 12); err != nil {
		t.Fatal(err)
	}
	usage, usagePolicy, err := cli.GetGovernanceUsage(ctx, 12)
	if err != nil || usage.QueriesThisHour != 7 || usage.RateLimitRemainingPerMin != 23 || usagePolicy == nil || usagePolicy.MaxQueriesPerDay != 5000 {
		t.Fatalf("usage=%+v policy=%+v err=%v", usage, usagePolicy, err)
	}
	want := "GET /api/v1/governance/policies/12,PUT /api/v1/governance/policies/12,DELETE /api/v1/governance/policies/12,GET /api/v1/governance/usage/12"
	if strings.Join(methods, ",") != want {
		t.Errorf("methods=%v", methods)
	}
}

func TestGovernancePreservesLicenseAdminAndNotFoundErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		message string
	}{
		{"licence", http.StatusForbidden, "Query governance requires an enterprise license"},
		{"admin", http.StatusForbidden, "Permission denied: admin required"},
		{"policy not found", http.StatusNotFound, "No policy found for this token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"success":false,"error":"` + tc.message + `"}`))
			})
			_, err := cli.GetGovernancePolicy(context.Background(), 12)
			var httpErr *HTTPError
			if !errors.As(err, &httpErr) || httpErr.Status != tc.status || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}
