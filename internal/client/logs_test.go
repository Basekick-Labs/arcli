package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestEstimateQuery_SuccessFalseAt200IsError(t *testing.T) {
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query/estimate" || r.Header.Get(HeaderDatabase) != "smoke" {
			t.Errorf("unexpected %s db=%q", r.URL.Path, r.Header.Get(HeaderDatabase))
		}
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "nosuch") {
			_, _ = w.Write([]byte(`{"success":false,"estimated_rows":null,"warning_level":"error","execution_time_ms":1,"error":"Cannot estimate query: no files"}`))
			return
		}
		_, _ = w.Write([]byte(`{"success":true,"estimated_rows":123456,"warning_level":"medium","warning_message":"Medium query: 123,456 rows.","execution_time_ms":12}`))
	})
	ctx := context.Background()
	res, err := cli.EstimateQuery(ctx, "SELECT * FROM cpu", "smoke")
	if err != nil || res.EstimatedRows == nil || *res.EstimatedRows != 123456 || res.WarningLevel != "medium" {
		t.Errorf("res=%+v err=%v", res, err)
	}
	res, err = cli.EstimateQuery(ctx, "SELECT * FROM nosuch", "smoke")
	if err == nil || !strings.Contains(err.Error(), "Cannot estimate query: no files") || res == nil || res.EstimatedRows != nil {
		t.Errorf("200/success:false must be an error with the decoded result: res=%+v err=%v", res, err)
	}
	if _, err := cli.EstimateQuery(ctx, "", "smoke"); err == nil {
		t.Error("empty sql must be rejected")
	}
}

func TestEstimateQuery_400And504(t *testing.T) {
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "SHOW") {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"success":false,"estimated_rows":null,"warning_level":"error","error":"SHOW DATABASES is not supported on the estimate endpoint; use /api/v1/query instead"}`))
			return
		}
		w.WriteHeader(504)
		_, _ = w.Write([]byte(`{"success":false,"error":"Query timed out","warning_level":"error","execution_time_ms":30000}`))
	})
	_, err := cli.EstimateQuery(context.Background(), "SHOW DATABASES", "")
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 400 || !strings.Contains(he.Message, "not supported on the estimate endpoint") {
		t.Errorf("err = %v", err)
	}
	_, err = cli.EstimateQuery(context.Background(), "SELECT 1", "")
	if !errors.As(err, &he) || he.Status != 504 {
		t.Errorf("err = %v", err)
	}
}

func TestLogs_ParamsValidationAndNull(t *testing.T) {
	var q string
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertAuthHeaders(t, r)
		q = r.URL.RawQuery
		if strings.Contains(q, "fatal") {
			_, _ = w.Write([]byte(`{"count":0,"level_filter":"fatal","limit":2,"logs":null,"since_minutes":1,"timestamp":"t"}`))
			return
		}
		_, _ = w.Write([]byte(`{"count":1,"level_filter":"","limit":100,"logs":[{"timestamp":"2026-09-07T20:42:09Z","level":"ERROR","component":"query","message":"Estimate query failed","caller":"x.go:1"}],"since_minutes":60,"timestamp":"t"}`))
	})
	ctx := context.Background()
	for _, bad := range []LogsOptions{{Limit: 0 - 1}, {Limit: 1001}, {Level: "loud"}, {Since: 30 * time.Second}, {Since: 25 * time.Hour}} {
		if _, err := cli.Logs(ctx, bad); err == nil {
			t.Errorf("%+v should be rejected client-side", bad)
		}
	}
	res, err := cli.Logs(ctx, LogsOptions{})
	if err != nil || q != "" || len(res.Logs) != 1 || res.Logs[0].Level != "ERROR" || res.Logs[0].Timestamp.IsZero() {
		t.Errorf("q=%q res=%+v err=%v", q, res, err)
	}
	res, err = cli.Logs(ctx, LogsOptions{Limit: 2, Level: "FATAL", Since: 90 * time.Second})
	if err != nil || res.Logs == nil || len(res.Logs) != 0 {
		t.Errorf("null logs must normalise to []: res=%+v err=%v", res, err)
	}
	if raw := string(res.Raw); !strings.Contains(raw, `"logs":[]`) || !strings.Contains(raw, `"level_filter":"fatal"`) {
		t.Errorf("raw must carry \"logs\":[] and keep the other keys: %s", raw)
	}
	if q != "level=fatal&limit=2&since_minutes=2" {
		t.Errorf("query = %q (since rounds up to whole minutes, level lower-cased)", q)
	}
}

func TestGetImportStats(t *testing.T) {
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/import/stats" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"stats":{"total_errors":1,"total_records":250,"total_requests":3},"status":"success"}`))
	})
	st, raw, err := cli.GetImportStats(context.Background())
	if err != nil || st.TotalRequests != 3 || st.TotalRecords != 250 || st.TotalErrors != 1 || !strings.Contains(string(raw), `"status"`) {
		t.Errorf("st=%+v err=%v", st, err)
	}
}

func TestWriteMsgPack_HeadersAnd400(t *testing.T) {
	var gotCT, gotDB string
	var gotLen int64
	var gotBody []byte
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/write/msgpack" {
			t.Errorf("path = %s", r.URL.Path)
		}
		gotCT, gotDB, gotLen = r.Header.Get("Content-Type"), r.Header.Get(HeaderDatabase), r.ContentLength
		gotBody, _ = io.ReadAll(r.Body)
		if len(gotBody) > 0 && gotBody[0] == 'c' {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"Invalid MessagePack payload: unsupported msgpack payload type: int8"}`))
			return
		}
		w.WriteHeader(204)
	})
	ctx := context.Background()
	payload := []byte{0x81, 0xa1, 'm', 0xa3, 'c', 'p', 'u'} // {"m":"cpu"} (server would reject: no columns — irrelevant here)
	if err := cli.WriteMsgPack(ctx, strings.NewReader(string(payload)), int64(len(payload)), "smoke"); err != nil {
		t.Fatal(err)
	}
	if gotCT != "application/msgpack" || gotDB != "smoke" || gotLen != int64(len(payload)) || string(gotBody) != string(payload) {
		t.Errorf("ct=%q db=%q len=%d body=%x", gotCT, gotDB, gotLen, gotBody)
	}
	err := cli.WriteMsgPack(ctx, strings.NewReader("cpu usage=1\n"), -1, "")
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 400 || !strings.Contains(he.Message, "Invalid MessagePack payload") {
		t.Errorf("err = %v", err)
	}
	if err := cli.WriteMsgPack(ctx, strings.NewReader(""), MsgPackMaxBody+1, ""); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Errorf("oversize must be rejected client-side: %v", err)
	}
}
