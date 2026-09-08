package client

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// freshClient returns a Client pointed at the given test server.
func freshClient(t *testing.T, srv *httptest.Server, db string) *Client {
	t.Helper()
	c, err := New(Config{
		Endpoint: srv.URL,
		Token:    "test-token",
		Database: db,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestNew_RequiresEndpointOnly(t *testing.T) {
	if _, err := New(Config{Token: "x"}); err == nil {
		t.Fatal("no endpoint: expected error, got nil")
	}
	// A token is optional: servers with auth.enabled = false take none.
	c, err := New(Config{Endpoint: "http://x"})
	if err != nil || c.HasToken() {
		t.Fatalf("no token: err=%v hasToken=%v", err, c != nil && c.HasToken())
	}
}

func TestNew_TrimsTrailingSlash(t *testing.T) {
	c, err := New(Config{Endpoint: "http://localhost:8000/", Token: "t"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := c.Endpoint(); got != "http://localhost:8000" {
		t.Fatalf("trailing slash not trimmed: got %q", got)
	}
}

func TestQueryJSON_SetsHeaders(t *testing.T) {
	var (
		gotAuth   string
		gotDB     string
		gotPath   string
		gotMethod string
		gotCT     string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotDB = r.Header.Get("x-arc-database")
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotCT = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"columns":["n"],"data":[[1]],"row_count":1,"execution_time_ms":1.5}`)
	}))
	defer srv.Close()

	c := freshClient(t, srv, "metrics")
	qr, err := c.QueryJSON(context.Background(), "SELECT 1", "")
	if err != nil {
		t.Fatalf("QueryJSON: %v", err)
	}
	if gotAuth != "Bearer test-token" {
		t.Errorf("Authorization = %q, want Bearer test-token", gotAuth)
	}
	if gotDB != "metrics" {
		t.Errorf("x-arc-database = %q, want metrics", gotDB)
	}
	if gotPath != "/api/v1/query" {
		t.Errorf("path = %q, want /api/v1/query", gotPath)
	}
	if gotMethod != "POST" {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotCT)
	}
	if qr.RowCount != 1 || len(qr.Columns) != 1 || qr.Columns[0] != "n" {
		t.Errorf("decoded result wrong: %+v", qr)
	}
}

func TestQueryJSON_DatabaseOverride(t *testing.T) {
	// Client default = "metrics", per-call override = "logs".
	// Per-call wins.
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("x-arc-database")
		_, _ = io.WriteString(w, `{"columns":[],"data":[],"row_count":0}`)
	}))
	defer srv.Close()

	c := freshClient(t, srv, "metrics")
	if _, err := c.QueryJSON(context.Background(), "SELECT 1", "logs"); err != nil {
		t.Fatalf("QueryJSON: %v", err)
	}
	if got != "logs" {
		t.Errorf("override ignored: got %q, want logs", got)
	}
}

func TestQueryJSON_FallsBackToClientDefault(t *testing.T) {
	// Pinning the contract: when per-call override is empty AND the
	// client has a configured default DB, the request DOES send the
	// x-arc-database header with that default. Cross-DB callers MUST
	// use setCrossDBHeaders instead — see database.go.
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("x-arc-database")
		_, _ = io.WriteString(w, `{"columns":[],"data":[],"row_count":0}`)
	}))
	defer srv.Close()

	c := freshClient(t, srv, "metrics") // client default = "metrics"
	if _, err := c.QueryJSON(context.Background(), "SELECT 1", ""); err != nil {
		t.Fatalf("QueryJSON: %v", err)
	}
	if got != "metrics" {
		t.Errorf("expected x-arc-database=metrics (client default), got %q", got)
	}
}

func TestQueryJSON_NoDatabaseHeaderWhenBothEmpty(t *testing.T) {
	// Empty client default + empty per-call -> header should be absent
	// so Arc applies its server-side default ("default").
	gotSet := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, gotSet = r.Header[http.CanonicalHeaderKey("x-arc-database")]
		_, _ = io.WriteString(w, `{"columns":[],"data":[],"row_count":0}`)
	}))
	defer srv.Close()

	c := freshClient(t, srv, "")
	if _, err := c.QueryJSON(context.Background(), "SELECT 1", ""); err != nil {
		t.Fatalf("QueryJSON: %v", err)
	}
	if gotSet {
		t.Error("x-arc-database header sent even though both client + per-call were empty")
	}
}

func TestQueryJSON_DecodesArcErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"success":false,"error":"table not found"}`)
	}))
	defer srv.Close()

	c := freshClient(t, srv, "")
	_, err := c.QueryJSON(context.Background(), "SELECT 1", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "table not found") {
		t.Errorf("error %q does not contain server message", err)
	}
	if !strings.Contains(err.Error(), "HTTP 400") {
		t.Errorf("error %q does not include HTTP status", err)
	}
}

func TestQueryJSON_DecodesNonJSONErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `<html><body>nginx upstream gone</body></html>`)
	}))
	defer srv.Close()

	c := freshClient(t, srv, "")
	_, err := c.QueryJSON(context.Background(), "SELECT 1", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "HTTP 502") {
		t.Errorf("error %q lacks status", err)
	}
	if !strings.Contains(err.Error(), "nginx") {
		t.Errorf("error %q does not include raw body", err)
	}
}

func TestQueryArrow_StreamsBody(t *testing.T) {
	// Verifies body streaming AND the trailer-after-EOF contract. We
	// use the http.TrailerPrefix sentinel (the documented way for an
	// http.Handler to emit a trailer that wasn't pre-declared in the
	// response head); that's the same surface area Arc's production
	// handler uses via fasthttp's respHeader.AddTrailer +
	// respHeader.Set after SetBodyStreamWriter completes.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query/arrow" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/vnd.apache.arrow.stream")
		w.Header().Set(http.TrailerPrefix+ArrowExecutionTimeTrailer, "42")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ARROW-IPC-PRETEND"))
	}))
	defer srv.Close()

	c := freshClient(t, srv, "")
	resp, err := c.QueryArrow(context.Background(), "SELECT 1", "")
	if err != nil {
		t.Fatalf("QueryArrow: %v", err)
	}
	defer resp.Close()

	// Before EOF: trailer is not yet readable on net/http response.
	// We don't assert this — production callers always io.Copy first.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "ARROW-IPC-PRETEND" {
		t.Errorf("body = %q", body)
	}
	ms, ok := resp.ExecutionTimeMs()
	if !ok {
		t.Fatal("trailer not read after body EOF")
	}
	if ms != 42 {
		t.Errorf("trailer = %d, want 42", ms)
	}
}

func TestQueryArrow_DecodesErrorBeforeStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"success":false,"error":"forbidden by RBAC"}`)
	}))
	defer srv.Close()

	c := freshClient(t, srv, "")
	resp, err := c.QueryArrow(context.Background(), "SELECT 1", "")
	if err == nil {
		_ = resp.Close()
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "forbidden by RBAC") {
		t.Errorf("error %q lacks server message", err)
	}
}

func TestWriteLineProtocol_Success(t *testing.T) {
	var (
		gotPath    string
		gotCT      string
		gotDB      string
		gotPrec    string
		gotBodyStr string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		gotDB = r.Header.Get("x-arc-database")
		gotPrec = r.URL.Query().Get("precision")
		b, _ := io.ReadAll(r.Body)
		gotBodyStr = string(b)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := freshClient(t, srv, "metrics")
	err := c.WriteLineProtocol(
		context.Background(),
		strings.NewReader("cpu,host=a v=1 1234"),
		"",
		PrecisionMS,
	)
	if err != nil {
		t.Fatalf("WriteLineProtocol: %v", err)
	}
	if gotPath != "/api/v1/write/line-protocol" {
		t.Errorf("path = %q", gotPath)
	}
	if gotCT != "text/plain" {
		t.Errorf("content-type = %q", gotCT)
	}
	if gotDB != "metrics" {
		t.Errorf("x-arc-database = %q", gotDB)
	}
	if gotPrec != "ms" {
		t.Errorf("precision query = %q", gotPrec)
	}
	if gotBodyStr != "cpu,host=a v=1 1234" {
		t.Errorf("body = %q", gotBodyStr)
	}
}

func TestWriteLineProtocol_PrecisionValidation(t *testing.T) {
	// No server needed — validation runs before the request.
	c := freshClient(t, httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})), "")
	err := c.WriteLineProtocol(context.Background(), strings.NewReader(""), "", Precision("furlong"))
	if err == nil || !strings.Contains(err.Error(), "invalid precision") {
		t.Errorf("expected invalid precision error, got %v", err)
	}
}

func TestWriteLineProtocol_NoPrecisionQueryWhenEmpty(t *testing.T) {
	gotRawQuery := "sentinel"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRawQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := freshClient(t, srv, "metrics")
	if err := c.WriteLineProtocol(context.Background(), strings.NewReader("x v=1"), "", ""); err != nil {
		t.Fatalf("WriteLineProtocol: %v", err)
	}
	if gotRawQuery != "" {
		t.Errorf("expected empty query string when precision is empty, got %q", gotRawQuery)
	}
}

func TestWriteLineProtocol_DecodesErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"malformed line at offset 17"}`)
	}))
	defer srv.Close()

	c := freshClient(t, srv, "")
	err := c.WriteLineProtocol(context.Background(), strings.NewReader("garbage"), "", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "malformed line") {
		t.Errorf("error %q lacks server message", err)
	}
}

func TestValidPrecision(t *testing.T) {
	for _, p := range []string{"", "ns", "us", "ms", "s"} {
		if !ValidPrecision(p) {
			t.Errorf("ValidPrecision(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"furlong", "NS", "nanoseconds", " ns"} {
		if ValidPrecision(p) {
			t.Errorf("ValidPrecision(%q) = true, want false", p)
		}
	}
}

func TestQueryResult_RowAt(t *testing.T) {
	// Row-major: Data[i] is the i-th row.
	qr := &QueryResult{
		Columns: []string{"a", "b"},
		Data: [][]any{
			{1.0, "x"},
			{2.0, "y"},
			{3.0, "z"},
		},
		RowCount: 3,
	}
	if got := qr.RowAt(1); got[0] != 2.0 || got[1] != "y" {
		t.Errorf("RowAt(1) = %v", got)
	}
	if got := qr.RowAt(-1); got != nil {
		t.Errorf("RowAt(-1) = %v, want nil", got)
	}
	if got := qr.RowAt(3); got != nil {
		t.Errorf("RowAt(3) = %v, want nil", got)
	}
}
