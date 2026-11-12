package commands

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/basekick-labs/arcli/internal/client"
)

// governanceUsageGolden is the usage text earlier releases print for a token
// policy. A server that reports no scope, or "node", must still get it byte
// for byte.
const governanceUsageGolden = `token id:                       12
queries this hour:              7
queries this day:               42
hour resets:                    2026-10-27T11:00:00Z
day resets:                     2026-10-28T00:00:00Z
remaining rate limit per minute: 23
remaining rate limit per hour:   593
policy:                         token policy
scope:                          this node only (not replicated)
policy rate limit per minute:   30
policy rate limit per hour:     600
policy max queries per hour:    500
policy max queries per day:     5000
policy max rows per query:      10000
policy max scan duration:       60 sec
`

func governanceScopeUsage() *client.GovernanceUsage {
	return &client.GovernanceUsage{TokenID: 12, QueriesThisHour: 7, QueriesThisDay: 42,
		HourResetAt: "2026-10-27T11:00:00Z", DayResetAt: "2026-10-28T00:00:00Z",
		RateLimitRemainingPerMin: 23, RateLimitRemainingPerHour: 593}
}

func governanceScopePolicy(id int64) *client.GovernancePolicy {
	return &client.GovernancePolicy{ID: id, TokenID: 12, RateLimitPerMinute: 30, RateLimitPerHour: 600,
		MaxQueriesPerHour: 500, MaxQueriesPerDay: 5000, MaxRowsPerQuery: 10000, MaxScanDurationSec: 60}
}

func TestGovernanceUsageScopeLine(t *testing.T) {
	legacy := "scope:                          this node only (not replicated)\n"
	for _, tc := range []struct {
		name   string
		policy *client.GovernancePolicy
		scope  string
		want   string // the scope line; "" when none is printed
	}{
		{"no scope (older Arc)", governanceScopePolicy(4), "", legacy},
		{"node", governanceScopePolicy(4), "node", legacy},
		{"no scope, server defaults", governanceScopePolicy(0), "", legacy},
		{"node, server defaults", governanceScopePolicy(0), "node", legacy},
		{"cluster", governanceScopePolicy(4), "cluster", "scope:                          cluster-wide (rate limits and quotas are counted per node)\n"},
		{"cluster, server defaults", governanceScopePolicy(0), "cluster", "scope:                          this node's server defaults (the cluster holds no policy for this token)\n"},
		{"unknown value", governanceScopePolicy(4), "region\x1b[31m", "scope:                          region[31m\n"},
		{"no policy, cluster", nil, "cluster", ""},
		{"no policy, no scope", nil, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			writeGovernanceUsageInfo(&b, governanceScopeUsage(), tc.policy, tc.scope, timeOutput{})
			out := b.String()
			if tc.want == "" {
				if strings.Contains(out, "scope:") {
					t.Fatalf("unexpected scope line:\n%s", out)
				}
				return
			}
			if !strings.Contains(out, tc.want) || strings.Count(out, "scope:") != 1 {
				t.Fatalf("want %q once in:\n%s", tc.want, out)
			}
			// ID 0 keeps meaning the server defaults.
			wantPolicy := "policy:                         token policy\n"
			if tc.policy.ID == 0 {
				wantPolicy = "policy:                         server defaults (no token policy)\n"
			}
			if !strings.Contains(out, wantPolicy) {
				t.Fatalf("want %q in:\n%s", wantPolicy, out)
			}
		})
	}
}

// Without a scope, or with "node", the usage text is the earlier releases'
// byte for byte.
func TestGovernanceUsageLegacyGolden(t *testing.T) {
	for _, scope := range []string{"", "node"} {
		var b bytes.Buffer
		writeGovernanceUsageInfo(&b, governanceScopeUsage(), governanceScopePolicy(4), scope, timeOutput{})
		if b.String() != governanceUsageGolden {
			t.Fatalf("scope %q changed the legacy output:\n got:\n%s\nwant:\n%s", scope, b.String(), governanceUsageGolden)
		}
	}
}

// governanceScopeServer answers the governance routes, adding scope (when
// non-empty) beside the policy, the policies and the usage, as Arc 27.01.1+.
func governanceScopeServer(t *testing.T, scope string) {
	t.Helper()
	field := ""
	if scope != "" {
		field = `,"scope":"` + scope + `"`
	}
	governanceCommandServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/auth/tokens/12":
			serveGovernanceToken(w, 12)
		case r.URL.Path == "/api/v1/auth/tokens":
			_, _ = w.Write([]byte(`{"tokens":[{"id":12,"name":"batch"}]}`))
		case r.URL.Path == "/api/v1/governance/policies":
			_, _ = w.Write([]byte(`{"success":true,"policies":[` + commandGovernancePolicy + `],"count":1` + field + `}`))
		case r.URL.Path == "/api/v1/governance/policies/12":
			_, _ = w.Write([]byte(`{"success":true,"policy":` + commandGovernancePolicy + field + `}`))
		case r.URL.Path == "/api/v1/governance/usage/12":
			_, _ = w.Write([]byte(`{"success":true,"usage":{"token_id":12,"queries_this_hour":7,"queries_this_day":42,"hour_reset_at":"2026-10-27T11:00:00Z","day_reset_at":"2026-10-28T00:00:00Z","rate_limit_remaining_per_minute":23,"rate_limit_remaining_per_hour":593},"policy":` + commandGovernancePolicy + field + `}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func TestGovernanceCommandsReportScope(t *testing.T) {
	const showScope = "scope:                  "
	for _, tc := range []struct {
		scope     string
		showLine  string // "" when show prints none
		usageLine string
	}{
		{"", "", "scope:                          this node only (not replicated)\n"},
		{"node", showScope + "this node only (not replicated)\n", "scope:                          this node only (not replicated)\n"},
		{"cluster", showScope + "cluster-wide (rate limits and quotas are counted per node)\n", "scope:                          cluster-wide (rate limits and quotas are counted per node)\n"},
	} {
		t.Run("scope="+tc.scope, func(t *testing.T) {
			governanceScopeServer(t, tc.scope)

			show, stderr, err := execCmd(t, newGovernancePolicyShowCmd(), "12")
			if err != nil || stderr != "" {
				t.Fatalf("show: err=%v stderr=%q", err, stderr)
			}
			if tc.showLine == "" {
				if strings.Contains(show, "scope:") {
					t.Errorf("show printed a scope for a server that reports none:\n%s", show)
				}
				if !strings.HasSuffix(show, "updated:                2026-10-27T10:00:00Z\n") {
					t.Errorf("show output changed:\n%s", show)
				}
			} else if !strings.HasSuffix(show, tc.showLine) {
				t.Errorf("show: want final line %q in:\n%s", tc.showLine, show)
			}

			usage, _, err := execCmd(t, newGovernanceUsageCmd(), "12")
			if err != nil || !strings.Contains(usage, tc.usageLine) {
				t.Errorf("usage: err=%v want %q in:\n%s", err, tc.usageLine, usage)
			}

			// JSON: scope is added beside the list and the usage only when the
			// server reports one; show's JSON stays the policy object.
			for _, c := range []struct {
				name string
				out  func() (string, error)
			}{
				{"list", func() (string, error) {
					out, _, err := execCmd(t, newGovernancePolicyListCmd(), "-o", "json")
					return out, err
				}},
				{"usage", func() (string, error) {
					out, _, err := execCmd(t, newGovernanceUsageCmd(), "12", "-o", "json")
					return out, err
				}},
				{"show", func() (string, error) {
					out, _, err := execCmd(t, newGovernancePolicyShowCmd(), "12", "-o", "json")
					return out, err
				}},
			} {
				out, err := c.out()
				if err != nil {
					t.Fatalf("%s json: %v", c.name, err)
				}
				var got map[string]any
				if err := json.Unmarshal([]byte(out), &got); err != nil {
					t.Fatalf("%s json: %v\n%s", c.name, err, out)
				}
				scope, has := got["scope"]
				wantHas := tc.scope != "" && c.name != "show"
				if has != wantHas || (has && scope != tc.scope) {
					t.Errorf("%s json: scope=%v (present %v), want present %v: %s", c.name, scope, has, wantHas, out)
				}
			}

			// The list table and CSV are unchanged by a scope.
			for _, format := range []string{"table", "csv"} {
				out, _, err := execCmd(t, newGovernancePolicyListCmd(), "-o", format)
				if err != nil || strings.Contains(out, "scope") || strings.Contains(out, "cluster") {
					t.Errorf("list %s: err=%v\n%s", format, err, out)
				}
			}
		})
	}
}
