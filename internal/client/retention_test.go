package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestFeatureDisabled_RetentionAndCQ(t *testing.T) {
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_, _ = w.Write([]byte(`{"status":"ok","time":"t","uptime":"1s"}`))
			return
		}
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":"Cannot ` + r.Method + ` ` + r.URL.Path + `"}`))
	})
	ctx := context.Background()
	var fde *FeatureDisabledError
	if _, _, err := cli.ListRetentionPolicies(ctx); !errors.As(err, &fde) || fde.ConfigKey != "retention.enabled" {
		t.Errorf("retention: %v", err)
	}
	if _, _, err := cli.ListContinuousQueries(ctx, "", nil); !errors.As(err, &fde) || fde.ConfigKey != "continuous_query.enabled" {
		t.Errorf("cq: %v", err)
	}
	if !strings.Contains(fde.Error(), "continuous queries are disabled on this server (continuous_query.enabled=false)") {
		t.Errorf("Error() = %q", fde.Error())
	}
	// Scheduler status is always registered; a 404 there is NOT a feature flag.
	if _, err := cli.SchedulerStatus(ctx); errors.As(err, &fde) {
		t.Errorf("scheduler 404 must not be classified as feature disabled: %v", err)
	}
}

func TestFeatureDisabled_NonArcHostIsNotDisabled(t *testing.T) {
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":"Cannot GET /api/v1/retention/"}`))
	})
	_, _, err := cli.ListRetentionPolicies(context.Background())
	var fde *FeatureDisabledError
	if errors.As(err, &fde) || err == nil || !strings.Contains(err.Error(), "check the endpoint path") {
		t.Errorf("err = %v", err)
	}
}

func TestListRetentionPolicies_NullBecomesEmpty(t *testing.T) {
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertAuthHeaders(t, r)
		if r.URL.Path != "/api/v1/retention/" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`null`))
	})
	list, raw, err := cli.ListRetentionPolicies(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if list == nil || len(list) != 0 || string(raw) != "[]" {
		t.Errorf("list=%#v raw=%s", list, raw)
	}
}

func TestRetention_CreateValidatesAndDecodes(t *testing.T) {
	var got RetentionPolicyRequest
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/retention/" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"id":1,"name":"p","database":"d","measurement":null,"retention_days":30,"buffer_days":7,"is_active":true,"last_execution_time":null,"last_execution_status":null,"last_deleted_count":null,"created_at":"2026-09-07T19:37:42Z","updated_at":"2026-09-07T19:37:42Z"}`))
	})
	ctx := context.Background()
	bad := []RetentionPolicyRequest{
		{Database: "d", RetentionDays: 1},
		{Name: "p", RetentionDays: 1},
		{Name: "p", Database: "d", RetentionDays: 0},
		{Name: "p", Database: "d", RetentionDays: 5, BufferDays: 7},
		{Name: "p", Database: "d", RetentionDays: 5, BufferDays: -1},
	}
	for _, b := range bad {
		if _, _, err := cli.CreateRetentionPolicy(ctx, b); err == nil {
			t.Errorf("%+v should be rejected client-side", b)
		}
	}
	p, _, err := cli.CreateRetentionPolicy(ctx, RetentionPolicyRequest{Name: "p", Database: "d", RetentionDays: 30, BufferDays: 7, IsActive: true})
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != 1 || p.Measurement != nil || p.LastDeletedCount != nil || !p.IsActive {
		t.Errorf("p = %+v", p)
	}
	if got.Name != "p" || !got.IsActive || got.Measurement != nil {
		t.Errorf("body = %+v", got)
	}
}

func TestRetention_UpdateSendsFullBodyAndDelete(t *testing.T) {
	var seen []string
	var raw map[string]json.RawMessage
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodPut {
			_ = json.NewDecoder(r.Body).Decode(&raw)
			_, _ = w.Write([]byte(`{"id":3,"name":"p","database":"d","measurement":"cpu","retention_days":10,"buffer_days":0,"is_active":false,"created_at":"c","updated_at":"u"}`))
			return
		}
		_, _ = w.Write([]byte(`{"message":"Retention policy deleted successfully"}`))
	})
	m := "cpu"
	req := RequestFromPolicy(RetentionPolicy{Name: "p", Database: "d", Measurement: &m, RetentionDays: 10, BufferDays: 0, IsActive: false})
	p, _, err := cli.UpdateRetentionPolicy(context.Background(), 3, req)
	if err != nil {
		t.Fatal(err)
	}
	if p.Measurement == nil || *p.Measurement != "cpu" || p.IsActive {
		t.Errorf("p = %+v", p)
	}
	for _, key := range []string{"name", "database", "measurement", "retention_days", "buffer_days", "is_active"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("PUT body must carry every field (full replace); missing %s", key)
		}
	}
	if err := cli.DeleteRetentionPolicy(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	if strings.Join(seen, ",") != "PUT /api/v1/retention/3,DELETE /api/v1/retention/3" {
		t.Errorf("seen = %v", seen)
	}
}

func TestRetention_ExecuteConfirmSemanticsAndNullMeasurements(t *testing.T) {
	var bodies []string
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		bs, _ := json.Marshal(b)
		bodies = append(bodies, string(bs))
		_, _ = w.Write([]byte(`{"policy_id":1,"policy_name":"p","deleted_count":0,"files_deleted":0,"execution_time_ms":2,"dry_run":true,"cutoff_date":"2026-08-01T19:37:42Z","affected_measurements":null}`))
	})
	res, err := cli.ExecuteRetentionPolicy(context.Background(), 1, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.AffectedMeasurements == nil || len(res.AffectedMeasurements) != 0 || res.CutoffDate == "" {
		t.Errorf("res = %+v", res)
	}
	if _, err := cli.ExecuteRetentionPolicy(context.Background(), 1, false); err != nil {
		t.Fatal(err)
	}
	if bodies[0] != `{"confirm":false,"dry_run":true}` || bodies[1] != `{"confirm":true,"dry_run":false}` {
		t.Errorf("bodies = %v", bodies)
	}
}

func TestRetention_ExecutionsNullAndLimit(t *testing.T) {
	var q string
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		q = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"executions":null,"policy_id":1}`))
	})
	if _, _, err := cli.ListRetentionExecutions(context.Background(), 1, 0); err == nil {
		t.Error("limit 0 must be rejected")
	}
	ex, _, err := cli.ListRetentionExecutions(context.Background(), 1, 5)
	if err != nil || ex == nil || len(ex) != 0 || q != "limit=5" {
		t.Errorf("ex=%#v q=%q err=%v", ex, q, err)
	}
}

func TestCQ_ValidationRules(t *testing.T) {
	base := ContinuousQueryRequest{
		Name: "cq", Database: "d", SourceMeasurement: "cpu", DestinationMeasurement: "cpu_1m",
		Query: "SELECT 1 FROM d.cpu WHERE time >= {start_time} AND time < {end_time}", Interval: "1m",
	}
	if err := ValidateCQRequest(base); err != nil {
		t.Fatalf("base must validate: %v", err)
	}
	mut := func(f func(r *ContinuousQueryRequest)) ContinuousQueryRequest { r := base; f(&r); return r }
	cases := map[string]ContinuousQueryRequest{
		"no name":          mut(func(r *ContinuousQueryRequest) { r.Name = "" }),
		"no db":            mut(func(r *ContinuousQueryRequest) { r.Database = "" }),
		"no source":        mut(func(r *ContinuousQueryRequest) { r.SourceMeasurement = "" }),
		"bad dest":         mut(func(r *ContinuousQueryRequest) { r.DestinationMeasurement = "1bad" }),
		"long dest":        mut(func(r *ContinuousQueryRequest) { r.DestinationMeasurement = "a" + strings.Repeat("b", 128) }),
		"no placeholders":  mut(func(r *ContinuousQueryRequest) { r.Query = "SELECT 1" }),
		"one placeholder":  mut(func(r *ContinuousQueryRequest) { r.Query = "SELECT {start_time}" }),
		"interval no unit": mut(func(r *ContinuousQueryRequest) { r.Interval = "5" }),
		"interval too low": mut(func(r *ContinuousQueryRequest) { r.Interval = "9s" }),
		"tag time":         mut(func(r *ContinuousQueryRequest) { r.TagColumns = []string{"Time"} }),
		"tag bad":          mut(func(r *ContinuousQueryRequest) { r.TagColumns = []string{"a b"} }),
		"long query":       mut(func(r *ContinuousQueryRequest) { r.Query = strings.Repeat("x", 10001) + "{start_time}{end_time}" }),
	}
	for name, r := range cases {
		if err := ValidateCQRequest(r); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if d, err := ParseCQInterval("10s"); err != nil || d != 10*time.Second {
		t.Errorf("10s: %v %v", d, err)
	}
	if err := ValidateCQRequest(mut(func(r *ContinuousQueryRequest) { r.Database = "1bad" })); err == nil {
		t.Error("database grammar must be checked")
	}
	if err := ValidateCQRequest(mut(func(r *ContinuousQueryRequest) { r.SourceMeasurement = "bad name" })); err == nil {
		t.Error("source grammar must be checked")
	}
}

func TestHasSourceReference(t *testing.T) {
	yes := []string{
		"SELECT 1 FROM d.cpu WHERE time >= {start_time}",
		"select avg(x) from D.CPU group by 1",
		"WITH c AS (SELECT 1 FROM d.cpu) SELECT * FROM c",
	}
	no := []string{
		"SELECT 1 FROM cpu",
		// A CTE merely named like the source never reads it.
		"WITH cpu AS (SELECT 1) SELECT * FROM cpu",
		"SELECT 1 FROM d.cpu_other",
		"SELECT 1 FROM read_parquet('x')",
	}
	if !HasSourceReference("SELECT 1 FROM d.cpu- WHERE 1", "d", "cpu-") {
		t.Error("a source ending in - must still be recognised")
	}
	if !HasSourceReference("SELECT 1", "bad\xff", "cpu") {
		t.Error("invalid UTF-8 must suppress the warning, not panic")
	}
	for _, q := range yes {
		if !HasSourceReference(q, "d", "cpu") {
			t.Errorf("should match: %s", q)
		}
	}
	for _, q := range no {
		if HasSourceReference(q, "d", "cpu") {
			t.Errorf("should not match: %s", q)
		}
	}
}

func TestCQ_ListFiltersNullAndTagColumns(t *testing.T) {
	var q string
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		q = r.URL.RawQuery
		if q == "" {
			_, _ = w.Write([]byte(`null`))
			return
		}
		_, _ = w.Write([]byte(`[{"id":1,"name":"a","database":"d","source_measurement":"s","destination_measurement":"t","query":"q","interval":"1m","tag_columns":null,"is_active":true,"created_at":"c","updated_at":"u"}]`))
	})
	list, raw, err := cli.ListContinuousQueries(context.Background(), "", nil)
	if err != nil || list == nil || len(list) != 0 || string(raw) != "[]" {
		t.Errorf("empty: list=%#v raw=%s err=%v", list, raw, err)
	}
	f := false
	list, _, err = cli.ListContinuousQueries(context.Background(), "d", &f)
	if err != nil {
		t.Fatal(err)
	}
	if q != "database=d&is_active=false" {
		t.Errorf("query = %q", q)
	}
	if list[0].TagColumns == nil || len(list[0].TagColumns) != 0 {
		t.Errorf("tag_columns null must normalise to []: %#v", list[0].TagColumns)
	}
}

func TestCQ_ExecuteBodyAndDryRun(t *testing.T) {
	var raw map[string]any
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/continuous_queries/7/execute" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&raw)
		_, _ = w.Write([]byte(`{"query_id":7,"query_name":"a","execution_id":"cq-exec-1","status":"dry_run","start_time":"s","end_time":"e","records_read":null,"records_written":0,"execution_time_seconds":0.1,"destination_measurement":"t","dry_run":true,"executed_at":"x","executed_query":"SELECT ..."}`))
	})
	ctx := context.Background()
	s := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	e := s.Add(time.Hour)
	if _, err := cli.ExecuteContinuousQuery(ctx, 7, CQExecuteOptions{Start: &e, End: &s}); err == nil {
		t.Error("start >= end must be rejected client-side")
	}
	res, err := cli.ExecuteContinuousQuery(ctx, 7, CQExecuteOptions{Start: &s, End: &e, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if raw["start_time"] != "2026-09-07T10:00:00Z" || raw["end_time"] != "2026-09-07T11:00:00Z" || raw["dry_run"] != true {
		t.Errorf("body = %v", raw)
	}
	if res.Status != "dry_run" || res.ExecutedQuery == "" {
		t.Errorf("res = %+v", res)
	}
	raw = nil
	if _, err := cli.ExecuteContinuousQuery(ctx, 7, CQExecuteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, has := raw["start_time"]; has {
		t.Error("nil start must be omitted so the server applies its default")
	}
}

func TestSchedulerStatus_TwoShapes(t *testing.T) {
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/schedulers/" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"cq_scheduler":{"enabled":false,"reason":"Enterprise license required"},"retention_scheduler":{"running":true,"schedule":"0 2 * * *","license_valid":true,"next_run":"2026-09-08T02:00:00Z","can_run":true,"gate_role":"writer"}}`))
	})
	st, err := cli.SchedulerStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.CQ.NotRunning() || st.CQ.Reason == "" || st.CQ.Jobs == nil {
		t.Errorf("cq = %+v", st.CQ)
	}
	if st.Retention.NotRunning() || !st.Retention.Running || st.Retention.NextRun == "" || st.Retention.CanRun == nil || !*st.Retention.CanRun {
		t.Errorf("retention = %+v", st.Retention)
	}
}
