package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestTieringEndpointsDecodeAndKeepRawResponses(t *testing.T) {
	var calls int
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		assertAuthHeaders(t, r)
		switch r.URL.Path {
		case "/api/v1/tiering/status":
			if r.Method != http.MethodGet {
				t.Errorf("status method = %s", r.Method)
			}
			_, _ = w.Write([]byte(`{"enabled":true,"license_valid":true,"quarantined_files":2,"tiers":{"hot":{"tier":"hot","enabled":true,"backend":"local","file_count":3,"total_size_mb":4}},"last_scan":{"files_scanned":8,"truncated":true},"future_field":true}`))
		case "/api/v1/tiering/files":
			if r.Method != http.MethodGet || r.URL.Query().Get("limit") != "2" {
				t.Errorf("files request = %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
			}
			tier, database := r.URL.Query().Get("tier"), r.URL.Query().Get("database")
			if !(tier == "" && database == "") && !(tier == "cold" && database == "") && !(tier == "" && database == "metrics") {
				t.Errorf("unexpected filters = %v", r.URL.Query())
			}
			_, _ = w.Write([]byte(`{"files":[{"id":3,"path":"metrics/cpu/1.parquet","database":"metrics","measurement":"cpu","tier":"cold","size_bytes":9}],"count":1}`))
		case "/api/v1/tiering/stats":
			_, _ = w.Write([]byte(`{"tier_stats":{"cold":{"tier":"cold","enabled":true,"backend":"s3","file_count":1,"total_size_mb":2}},"recent_migrations":[{"id":5,"file_path":"p","database":"metrics","from_tier":"hot","to_tier":"cold","size_bytes":9,"started_at":"2026-10-31T00:00:00Z"}]}`))
		case "/api/v1/tiering/migrate":
			if r.Method != http.MethodPost {
				t.Errorf("migrate method = %s", r.Method)
			}
			var got TieringMigrationRequest
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Errorf("decode migration request: %v", err)
			}
			if got.Database != "metrics" || got.Measurement != "cpu" || !got.DryRun {
				t.Errorf("migration request = %+v", got)
			}
			_, _ = w.Write([]byte(`{"status":"dry_run","would_migrate":2,"total_bytes":99,"from_tier":"hot","to_tier":"cold"}`))
		case "/api/v1/tiering/scan":
			if r.Method != http.MethodPost {
				t.Errorf("scan method = %s", r.Method)
			}
			_, _ = w.Write([]byte(`{"files_scanned":8,"files_registered":7,"files_skipped":1,"errors":0,"cold_synced":2,"hot_retired":1,"hot_rows_written":4,"truncated":false}`))
		case "/api/v1/tiering/policies/":
			_, _ = w.Write([]byte(`{"policies":[{"id":7,"database":"metrics","hot_only":true,"hot_max_age_days":0,"created_at":"2026-10-31T00:00:00Z","updated_at":"2026-10-31T00:00:00Z"}],"count":1}`))
		case "/api/v1/tiering/policies/metrics":
			switch r.Method {
			case http.MethodGet:
				_, _ = w.Write([]byte(`{"id":7,"database":"metrics","hot_only":true,"hot_max_age_days":0,"created_at":"2026-10-31T00:00:00Z","updated_at":"2026-10-31T00:00:00Z"}`))
			case http.MethodPut:
				var got TieringPolicyRequest
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Errorf("decode policy request: %v", err)
				}
				if !got.HotOnly || got.HotMaxAgeDays == nil || *got.HotMaxAgeDays != 0 {
					t.Errorf("policy request = %+v", got)
				}
				_, _ = w.Write([]byte(`{"message":"Policy updated successfully","database":"metrics","policy":{"database":"metrics","hot_only":true,"hot_max_age_days":0},"cold_files":{"recorded_before_update":2,"scope":"node_local_metadata"}}`))
			case http.MethodDelete:
				_, _ = w.Write([]byte(`{"message":"Policy deleted, database now uses global defaults","database":"metrics"}`))
			default:
				t.Errorf("unexpected policy method %s", r.Method)
			}
		case "/api/v1/tiering/policies/metrics/effective":
			_, _ = w.Write([]byte(`{"database":"metrics","hot_only":false,"hot_max_age_days":30,"source":"global"}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
	ctx := context.Background()

	status, err := cli.TieringStatus(ctx)
	if err != nil || !status.Enabled || status.QuarantinedFiles != 2 || status.LastScan == nil || !status.LastScan.Truncated || !strings.Contains(string(status.Raw), `"future_field"`) {
		t.Errorf("status=%+v err=%v", status, err)
	}

	files, raw, err := cli.ListTieringFiles(ctx, "cold", "", 2)
	if err != nil || len(files) != 1 || files[0].SizeBytes != 9 || !strings.Contains(string(raw), `"files"`) {
		t.Errorf("files=%+v raw=%s err=%v", files, raw, err)
	}
	if _, _, err := cli.ListTieringFiles(ctx, "", "metrics", 2); err != nil {
		t.Errorf("database filter: %v", err)
	}

	stats, err := cli.TieringStats(ctx)
	if err != nil || len(stats.TierStats) != 1 || len(stats.RecentMigrations) != 1 || stats.RecentMigrations[0].ID != 5 {
		t.Errorf("stats=%+v err=%v", stats, err)
	}

	migration, err := cli.MigrateTiering(ctx, TieringMigrationRequest{Database: "metrics", Measurement: "cpu", DryRun: true})
	if err != nil || migration.WouldMigrate == nil || *migration.WouldMigrate != 2 || migration.TotalBytes == nil || *migration.TotalBytes != 99 {
		t.Errorf("migration=%+v err=%v", migration, err)
	}

	scan, _, err := cli.ScanTieringFiles(ctx)
	if err != nil || scan.FilesScanned != 8 || scan.HotRowsWritten != 4 {
		t.Errorf("scan=%+v err=%v", scan, err)
	}

	policies, err := cli.ListTieringPolicies(ctx)
	if err != nil || policies.Count != 1 || len(policies.Policies) != 1 || policies.Policies[0].HotMaxAgeDays == nil || *policies.Policies[0].HotMaxAgeDays != 0 {
		t.Errorf("policies=%+v err=%v", policies, err)
	}

	policy, _, err := cli.GetTieringPolicy(ctx, "metrics")
	if err != nil || policy.Database != "metrics" || policy.HotMaxAgeDays == nil || *policy.HotMaxAgeDays != 0 {
		t.Errorf("policy=%+v err=%v", policy, err)
	}

	mutation, err := cli.SetTieringPolicy(ctx, "metrics", TieringPolicyRequest{HotOnly: true, HotMaxAgeDays: func() *int { n := 0; return &n }()})
	if err != nil || mutation.Policy == nil || mutation.Policy.Database != "metrics" || !strings.Contains(string(mutation.ColdFiles), "recorded_before_update") {
		t.Errorf("set=%+v err=%v", mutation, err)
	}

	mutation, err = cli.DeleteTieringPolicy(ctx, "metrics")
	if err != nil || !strings.Contains(mutation.Message, "global defaults") {
		t.Errorf("delete=%+v err=%v", mutation, err)
	}

	effective, _, err := cli.EffectiveTieringPolicy(ctx, "metrics")
	if err != nil || effective.HotMaxAgeDays != 30 || effective.Source != "global" {
		t.Errorf("effective=%+v err=%v", effective, err)
	}
	if calls != 11 {
		t.Errorf("request count = %d, want 11", calls)
	}
}

func TestTieringValidationRejectsBeforeRequest(t *testing.T) {
	calls := 0
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{}`))
	})
	ctx := context.Background()
	bad := []func() error{
		func() error { _, _, err := cli.ListTieringFiles(ctx, "warm", "", 10); return err },
		func() error { _, _, err := cli.ListTieringFiles(ctx, "", "bad name", 10); return err },
		func() error { _, _, err := cli.ListTieringFiles(ctx, "", "", -1); return err },
		func() error { _, _, err := cli.ListTieringFiles(ctx, "cold", "metrics", 10); return err },
		func() error {
			_, err := cli.MigrateTiering(ctx, TieringMigrationRequest{Database: "bad/name"})
			return err
		},
		func() error {
			_, err := cli.MigrateTiering(ctx, TieringMigrationRequest{Measurement: "bad name"})
			return err
		},
		func() error { _, err := cli.MigrateTiering(ctx, TieringMigrationRequest{FromTier: "cold"}); return err },
		func() error { _, err := cli.MigrateTiering(ctx, TieringMigrationRequest{ToTier: "hot"}); return err },
		func() error { _, err := cli.SetTieringPolicy(ctx, "bad/name", TieringPolicyRequest{}); return err },
		func() error {
			n := -1
			_, err := cli.SetTieringPolicy(ctx, "metrics", TieringPolicyRequest{HotMaxAgeDays: &n})
			return err
		},
		func() error { _, _, err := cli.GetTieringPolicy(ctx, "bad/name"); return err },
		func() error { _, err := cli.DeleteTieringPolicy(ctx, "bad/name"); return err },
		func() error { _, _, err := cli.EffectiveTieringPolicy(ctx, "bad/name"); return err },
	}
	for i, call := range bad {
		if err := call(); err == nil {
			t.Errorf("case %d: expected validation error", i)
		}
	}
	if calls != 0 {
		t.Errorf("invalid inputs made %d requests", calls)
	}
	if _, _, err := cli.ListTieringFiles(ctx, "", "", 0); err != nil {
		t.Fatalf("zero limit should preserve Arc's empty-result contract: %v", err)
	}
}

func TestTieringNullListsPreserveOriginalJSON(t *testing.T) {
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/tiering/files":
			_, _ = w.Write([]byte(`{"files":null,"count":0,"future":true}`))
		case "/api/v1/tiering/stats":
			_, _ = w.Write([]byte(`{"tier_stats":null,"recent_migrations":null,"future":true}`))
		case "/api/v1/tiering/policies/":
			_, _ = w.Write([]byte(`{"policies":null,"count":0,"future":true}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	ctx := context.Background()
	files, rawFiles, err := cli.ListTieringFiles(ctx, "", "", 100)
	if err != nil || files == nil || len(files) != 0 || !strings.Contains(string(rawFiles), `"files":null`) || !strings.Contains(string(rawFiles), `"future":true`) {
		t.Errorf("files=%#v raw=%s err=%v", files, rawFiles, err)
	}
	stats, err := cli.TieringStats(ctx)
	if err != nil || stats.TierStats == nil || stats.RecentMigrations == nil || !strings.Contains(string(stats.Raw), `"tier_stats":null`) || !strings.Contains(string(stats.Raw), `"future":true`) {
		t.Errorf("stats=%+v err=%v", stats, err)
	}
	policies, err := cli.ListTieringPolicies(ctx)
	if err != nil || policies.Policies == nil || len(policies.Policies) != 0 || !strings.Contains(string(policies.Raw), `"policies":null`) || !strings.Contains(string(policies.Raw), `"future":true`) {
		t.Errorf("policies=%+v err=%v", policies, err)
	}
}

func TestTieringLicenseAndAdminErrorsRemainVisible(t *testing.T) {
	calls := 0
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusForbidden)
		if calls == 1 {
			_, _ = w.Write([]byte(`{"success":false,"error":"Tiered storage requires an enterprise license with the 'tiering' feature enabled"}`))
			return
		}
		_, _ = w.Write([]byte(`{"error":"admin token required"}`))
	})
	_, err := cli.TieringStatus(context.Background())
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != http.StatusForbidden || !strings.Contains(err.Error(), "enterprise license") {
		t.Fatalf("err = %v", err)
	}
	_, _, err = cli.ListTieringFiles(context.Background(), "", "", 100)
	if !errors.As(err, &he) || he.Status != http.StatusForbidden || !strings.Contains(err.Error(), "admin token required") {
		t.Fatalf("admin error = %v", err)
	}
}

func TestTieringRouteMissingPreservesHTTPError(t *testing.T) {
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.Write([]byte(`{"status":"ok","time":"t","uptime":"1s"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"Cannot GET /api/v1/tiering/status"}`))
	})
	_, err := cli.TieringStatus(context.Background())
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != http.StatusNotFound || he.Message != "Cannot GET /api/v1/tiering/status" {
		t.Fatalf("HTTP error=%v", err)
	}
	if strings.Contains(err.Error(), "enabled=false") {
		t.Fatalf("inferred disabled: %v", err)
	}
	var fde *FeatureDisabledError
	if errors.As(err, &fde) {
		t.Fatalf("tiering must not report FeatureDisabledError: %v", err)
	}
	const want = "Cannot GET /api/v1/tiering/status (HTTP 404); Arc is reachable but did not register this route; check tiered_storage.enabled, the license, the server log, and that this Arc version supports this command"
	if !strings.HasSuffix(err.Error(), want) {
		t.Fatalf("err = %q\nwant suffix %q", err.Error(), want)
	}
}

func TestTieringReviewCredentialRedaction(t *testing.T) {
	for _, text := range []string{
		`Get "https://h/c?X-Amz-Signature=SECRET": EOF`,
		`AccountKey=SECRET; SharedAccessSignature=sv=1&sig=SECRET; sig=SECRET`,
		`parse "https://h/c?sig=SECRET\n": invalid URL`,
	} {
		raw, _ := json.Marshal(map[string]any{"tier_stats": map[string]any{"hot": map[string]any{"file_count": 1}}, "recent_migrations": []any{map[string]any{"error": text}}, "future": json.RawMessage(`9007199254740993`)})
		cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(raw) })
		stats, err := cli.TieringStats(context.Background())
		if err != nil || len(stats.TierStats) != 1 {
			t.Fatalf("stats=%+v err=%v", stats, err)
		}
		if strings.Contains(string(stats.Raw), "SECRET") || strings.Contains(stats.RecentMigrations[0].Error, "SECRET") || !strings.Contains(string(stats.Raw), "9007199254740993") {
			t.Fatalf("unsafe or lossy response: %s", stats.Raw)
		}
	}
}

func TestTieringReviewPartialScanAndConflictDetails(t *testing.T) {
	for _, status := range []int{500, 503} {
		cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"scan failed AccountKey=SECRET","message":"retry later","result":{"files_scanned":7,"errors":1,"truncated":true}}`))
		})
		result, raw, err := cli.ScanTieringFiles(context.Background())
		var he *HTTPError
		if result == nil || result.FilesScanned != 7 || !errors.As(err, &he) || he.Status != status || !strings.Contains(err.Error(), "retry later") {
			t.Fatalf("result=%+v error=%v", result, err)
		}
		if strings.Contains(string(raw)+err.Error()+string(he.Body), "SECRET") {
			t.Fatal("credential leaked")
		}
	}
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(409)
		_, _ = w.Write([]byte(`{"error":"wrong node","message":"Retry against the primary writer"}`))
	})
	_, err := cli.MigrateTiering(context.Background(), TieringMigrationRequest{})
	if err == nil || !strings.Contains(err.Error(), "Retry against the primary writer") {
		t.Fatalf("error=%v", err)
	}
}
