package commands

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/basekick-labs/arcli/internal/client"
)

func TestGovernanceListTokenLookupBestEffort(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			governanceCommandServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/auth/tokens":
					calls++
					w.WriteHeader(status)
				case "/api/v1/governance/policies":
					_, _ = w.Write([]byte(`{"policies":[{"id":1,"token_id":12}]}`))
				default:
					t.Errorf("unexpected request %s", r.URL)
				}
			})
			out, _, err := execCmd(t, newGovernancePolicyListCmd(), "-o", "csv")
			if err != nil || calls != 1 || !strings.Contains(out, "12,,") {
				t.Fatalf("err=%v calls=%d out=%s", err, calls, out)
			}
		})
	}
}

func TestGovernanceOrphanPolicyNumericReference(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			policyReads, deletes := 0, 0
			governanceCommandServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/auth/tokens/12":
					w.WriteHeader(status)
					_, _ = w.Write([]byte(`{"error":"Token not found"}`))
				case "/api/v1/governance/policies/12":
					if r.Method == http.MethodDelete {
						deletes++
						_, _ = w.Write([]byte(`{"success":true}`))
						return
					}
					policyReads++
					_, _ = w.Write([]byte(`{"policy":` + commandGovernancePolicy + `}`))
				default:
					t.Errorf("unexpected request %s", r.URL)
				}
			})
			showOut, _, showErr := execCmd(t, newGovernancePolicyShowCmd(), "12")
			deleteOut, _, deleteErr := execCmd(t, newGovernancePolicyDeleteCmd(), "12", "--yes")
			if status == http.StatusNotFound {
				if showErr != nil || deleteErr != nil || deletes != 1 || policyReads != 2 {
					t.Fatalf("show=%v delete=%v reads=%d deletes=%d", showErr, deleteErr, policyReads, deletes)
				}
				if !strings.Contains(showOut, "token:                  - (token not found)\n") || deleteOut != "Deleted governance policy for token id 12 (token not found)\n" {
					t.Fatalf("show=%q delete=%q", showOut, deleteOut)
				}
			} else if showErr == nil || deleteErr == nil || policyReads != 0 || deletes != 0 {
				t.Fatalf("unexpected fallback show=%v delete=%v reads=%d deletes=%d", showErr, deleteErr, policyReads, deletes)
			}
		})
	}
}

func TestGovernanceCreateRequiresExplicitLimits(t *testing.T) {
	calls := 0
	governanceCommandServer(t, func(w http.ResponseWriter, r *http.Request) { calls++; t.Errorf("unexpected request %s", r.URL) })
	for _, args := range [][]string{{"12"}, {"12", "--unlimited", "--max-rows-per-query", "0"}, {"12", "--max-rows-per-query", "-1"}} {
		if _, _, err := execCmd(t, newGovernancePolicyCreateCmd(), args...); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	if calls != 0 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestGovernanceCreatePreflight(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		policy   string
		wantPost bool
	}{
		{"duplicate", 200, commandGovernancePolicy, false},
		{"absent", 404, "", true},
		{"defaults", 200, `{"id":0,"token_id":12,"max_rows_per_query":10}`, true},
		{"forbidden", 403, "", false},
		{"failure", 500, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			posts := 0
			governanceCommandServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/auth/tokens/12":
					serveGovernanceToken(w, 12)
				case "/api/v1/governance/policies/12":
					w.WriteHeader(tc.status)
					if tc.policy != "" {
						_, _ = w.Write([]byte(`{"policy":` + tc.policy + `}`))
					} else {
						_, _ = w.Write([]byte(`{"error":"No policy found for this token"}`))
					}
				case "/api/v1/governance/policies":
					posts++
					_, _ = w.Write([]byte(`{"policy":` + commandGovernancePolicy + `}`))
				default:
					t.Errorf("unexpected request %s", r.URL)
				}
			})
			_, _, err := execCmd(t, newGovernancePolicyCreateCmd(), "12", "--unlimited")
			if tc.wantPost && (err != nil || posts != 1) || !tc.wantPost && (err == nil || posts != 0) {
				t.Fatalf("err=%v posts=%d", err, posts)
			}
			if tc.name == "duplicate" && !strings.Contains(err.Error(), "policy update") {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestGovernanceUsageLimitSemantics(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy *client.GovernancePolicy
		want   []string
	}{
		{"defaults", &client.GovernancePolicy{}, []string{"queries this hour:              n/a", "queries this day:               n/a", "remaining rate limit per minute: unlimited", "remaining rate limit per hour:   unlimited", "server defaults (no token policy)", "policy max scan duration:       unlimited\n"}},
		{"exhausted", &client.GovernancePolicy{ID: 1, RateLimitPerMinute: 3, RateLimitPerHour: 10, MaxQueriesPerHour: 10, MaxQueriesPerDay: 20, MaxScanDurationSec: 30}, []string{"queries this hour:              0", "queries this day:               0", "remaining rate limit per minute: 0", "remaining rate limit per hour:   0", "policy:                         token policy", "policy max scan duration:       30 sec"}},
		{"none", nil, []string{"policy:                         none (no token policy or server defaults; no limits apply)", "queries this hour:              n/a (no query quota)", "remaining rate limit per minute: unlimited", "remaining rate limit per hour:   unlimited"}},
		// Arc counts both windows in one tracker once either quota is set.
		{"hourly quota only", &client.GovernancePolicy{ID: 1, MaxQueriesPerHour: 10}, []string{"queries this hour:              2", "queries this day:               2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			usage := &client.GovernanceUsage{TokenID: 12}
			if tc.name == "hourly quota only" {
				usage.QueriesThisHour, usage.QueriesThisDay = 2, 2
			}
			writeGovernanceUsageInfo(&b, usage, tc.policy, "", timeOutput{})
			for _, want := range tc.want {
				if !strings.Contains(b.String(), want) {
					t.Errorf("missing %q in %s", want, b.String())
				}
			}
		})
	}
	var b bytes.Buffer
	writeGovernancePolicyInfo(&b, &client.GovernancePolicy{}, timeOutput{})
	if strings.Contains(b.String(), "unlimited sec") {
		t.Fatalf("bad units: %s", b.String())
	}
}
