package commands

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

const commandGovernancePolicy = `{"id":4,"token_id":12,"token_name":"batch","rate_limit_per_minute":30,"rate_limit_per_hour":600,"max_queries_per_hour":500,"max_queries_per_day":5000,"max_rows_per_query":10000,"max_scan_duration_sec":60,"created_at":"2026-10-27T10:00:00Z","updated_at":"2026-10-27T10:00:00Z"}`

func governanceCommandServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	writeTestConfig(t, srv.URL, "admin-token")
	return srv
}

func serveGovernanceToken(w http.ResponseWriter, id int64) {
	_, _ = w.Write([]byte(`{"success":true,"token":{"id":` + strconv.FormatInt(id, 10) + `,"name":"batch","permissions":["admin"],"created_at":"2026-10-27T10:00:00Z","enabled":true}}`))
}

func TestGovernanceCommandListFormats(t *testing.T) {
	srv := governanceCommandServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer admin-token" {
			t.Errorf("Authorization=%q", r.Header.Get("Authorization"))
		}
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/governance/policies" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		_, _ = w.Write([]byte(`{"success":true,"policies":[` + commandGovernancePolicy + `],"count":1}`))
	})
	_ = srv

	for _, format := range []string{"table", "json", "csv"} {
		t.Run(format, func(t *testing.T) {
			out, stderr, err := execCmd(t, newGovernancePolicyListCmd(), "-o", format)
			if err != nil || stderr != "" {
				t.Fatalf("err=%v stderr=%q", err, stderr)
			}
			switch format {
			case "table":
				if !strings.Contains(out, "batch") || !strings.Contains(out, "5000") {
					t.Errorf("table output=%s", out)
				}
			case "json":
				var got struct {
					Policies []map[string]any `json:"policies"`
					Count    int              `json:"count"`
				}
				if err := json.Unmarshal([]byte(out), &got); err != nil || got.Count != 1 || len(got.Policies) != 1 || got.Policies[0]["token_name"] != "batch" {
					t.Errorf("json=%s err=%v", out, err)
				}
			case "csv":
				if !strings.HasPrefix(out, "token_id,token_name,") || !strings.Contains(out, "12,batch,30,600") {
					t.Errorf("csv output=%s", out)
				}
			}
		})
	}
}

func TestGovernanceCommandsResolveNamesAndMergeUpdates(t *testing.T) {
	var put map[string]any
	srv := governanceCommandServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/tokens":
			_, _ = w.Write([]byte(`{"success":true,"tokens":[{"id":12,"name":"batch","permissions":["admin"],"created_at":"2026-10-27T10:00:00Z","enabled":true}],"count":1}`))
		case "/api/v1/governance/policies/12":
			switch r.Method {
			case http.MethodGet:
				_, _ = w.Write([]byte(`{"success":true,"policy":` + commandGovernancePolicy + `}`))
			case http.MethodPut:
				if err := json.NewDecoder(r.Body).Decode(&put); err != nil {
					t.Error(err)
				}
				_, _ = w.Write([]byte(`{"success":true,"policy":` + commandGovernancePolicy + `}`))
			default:
				t.Errorf("unexpected %s %s", r.Method, r.URL)
			}
		case "/api/v1/governance/usage/12":
			_, _ = w.Write([]byte(`{"success":true,"usage":{"token_id":12,"queries_this_hour":7,"queries_this_day":42,"hour_reset_at":"2026-10-27T11:00:00Z","day_reset_at":"2026-10-28T00:00:00Z","rate_limit_remaining_per_minute":23,"rate_limit_remaining_per_hour":593},"policy":` + commandGovernancePolicy + `}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	_ = srv

	out, _, err := execCmd(t, newGovernancePolicyShowCmd(), "batch")
	if err != nil || !strings.Contains(out, "token:                  batch") {
		t.Fatalf("show err=%v out=%q", err, out)
	}
	out, _, err = execCmd(t, newGovernancePolicyUpdateCmd(), "batch", "--rate-limit-per-minute", "0", "-o", "json")
	if err != nil {
		t.Fatalf("update err=%v out=%q", err, out)
	}
	if put["token_id"] != float64(12) || put["rate_limit_per_minute"] != float64(0) || put["rate_limit_per_hour"] != float64(600) || put["max_queries_per_day"] != float64(5000) || put["max_scan_duration_sec"] != float64(60) {
		t.Errorf("PUT body did not merge existing limits: %#v", put)
	}
	var updated map[string]any
	if err := json.Unmarshal([]byte(out), &updated); err != nil || updated["token_id"] != float64(12) {
		t.Errorf("update JSON=%s err=%v", out, err)
	}
	out, _, err = execCmd(t, newGovernanceUsageCmd(), "batch", "-o", "json")
	if err != nil || !strings.Contains(out, `"queries_this_hour": 7`) || !strings.Contains(out, `"max_queries_per_day": 5000`) {
		t.Fatalf("usage err=%v out=%s", err, out)
	}
}

func TestGovernanceCreateAndDelete(t *testing.T) {
	var created map[string]any
	var deleted bool
	srv := governanceCommandServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/tokens/12":
			serveGovernanceToken(w, 12)
		case "/api/v1/governance/policies":
			if r.Method != http.MethodPost {
				t.Errorf("unexpected method %s", r.Method)
			}
			if err := json.NewDecoder(r.Body).Decode(&created); err != nil {
				t.Error(err)
			}
			_, _ = w.Write([]byte(`{"success":true,"policy":` + commandGovernancePolicy + `}`))
		case "/api/v1/governance/policies/12":
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(`{"success":true,"policy":` + commandGovernancePolicy + `}`))
				return
			}
			if r.Method == http.MethodDelete {
				deleted = true
				_, _ = w.Write([]byte(`{"success":true,"message":"Policy deleted"}`))
				return
			}
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	_ = srv

	out, _, err := execCmd(t, newGovernancePolicyCreateCmd(), "12", "--rate-limit-per-hour", "0", "--max-rows-per-query", "9000", "-o", "json")
	if err != nil {
		t.Fatalf("create err=%v out=%s", err, out)
	}
	if created["token_id"] != float64(12) || created["rate_limit_per_hour"] != float64(0) || created["max_rows_per_query"] != float64(9000) || created["rate_limit_per_minute"] != float64(0) {
		t.Errorf("create request=%#v", created)
	}
	if _, _, err := execCmd(t, newGovernancePolicyDeleteCmd(), "12", "--yes"); err != nil || !deleted {
		t.Fatalf("delete err=%v deleted=%t", err, deleted)
	}
}

func TestGovernanceDeleteConfirmationAndValidation(t *testing.T) {
	var deletes int
	srv := governanceCommandServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/tokens/12":
			serveGovernanceToken(w, 12)
		case "/api/v1/governance/policies/12":
			if r.Method == http.MethodDelete {
				deletes++
				_, _ = w.Write([]byte(`{"success":true}`))
				return
			}
			_, _ = w.Write([]byte(`{"success":true,"policy":` + commandGovernancePolicy + `}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
	})
	_ = srv

	c := newGovernancePolicyDeleteCmd()
	c.SetIn(confirmationInput(t, "pipe"))
	_, _, err := execCmd(t, c, "12")
	if err == nil || err.Error() != "confirmation required but stdin is not a terminal; pass --yes" || deletes != 0 {
		t.Fatalf("piped delete err=%v deletes=%d", err, deletes)
	}
	for _, args := range [][]string{{"12", "--max-rows-per-query", "-1"}, {"12"}} {
		_, _, err := execCmd(t, newGovernancePolicyUpdateCmd(), args...)
		if err == nil {
			t.Errorf("update args %v should fail before requests", args)
		}
	}
}

func TestGovernanceCommandKeepsServerErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		message string
	}{
		{"licence", http.StatusForbidden, "Query governance requires an enterprise license"},
		{"admin", http.StatusForbidden, "Permission denied: admin required"},
		{"not found", http.StatusNotFound, "No policy found for this token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			governanceCommandServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"success":false,"error":"` + tc.message + `"}`))
			})
			_, _, err := execCmd(t, newGovernancePolicyListCmd())
			if err == nil || !strings.Contains(err.Error(), tc.message) || !strings.Contains(err.Error(), strconv.Itoa(tc.status)) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestGovernanceRootRegistration(t *testing.T) {
	out, _, err := execCmd(t, NewRoot(BuildInfo{Version: "test"}), "governance", "policy", "create", "--help")
	if err != nil || !strings.Contains(out, "arcli governance policy create <token-id|name>") || !strings.Contains(out, "--rate-limit-per-minute") {
		t.Fatalf("help err=%v output=%s", err, out)
	}
}

func TestGovernanceLimitZeroRendersUnlimited(t *testing.T) {
	if got := governanceLimit(0); got != "unlimited" {
		t.Errorf("governanceLimit(0)=%q", got)
	}
}
