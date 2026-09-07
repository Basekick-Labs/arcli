package client

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// writeTempFile writes content to a fresh temp file and returns its path.
// t.Cleanup handles removal.
func writeTempFile(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return p
}

// parseMultipartForm decodes the file field out of a multipart request.
// Returns (filename, contents).
func parseMultipartForm(t *testing.T, r *http.Request) (string, string) {
	t.Helper()
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		t.Fatalf("ParseMultipartForm: %v", err)
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		t.Fatalf("FormFile: %v", err)
	}
	defer f.Close()
	body, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	return hdr.Filename, string(body)
}

// envelope helper for happy-path test servers.
func writeImportEnvelope(t *testing.T, w http.ResponseWriter, result any) {
	t.Helper()
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	env := importEnvelope{Status: "ok", Result: raw}
	out, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(out)
}

func TestImportCSV_SendsMultipartAndQueryParams(t *testing.T) {
	var (
		gotPath        string
		gotMethod      string
		gotQuery       url.Values
		gotCT          string
		gotDB          string
		gotAuth        string
		gotFilename    string
		gotFileContent string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotQuery = r.URL.Query()
		gotCT = r.Header.Get("Content-Type")
		gotDB = r.Header.Get("x-arc-database")
		gotAuth = r.Header.Get("Authorization")
		gotFilename, gotFileContent = parseMultipartForm(t, r)
		writeImportEnvelope(t, w, ImportResult{
			Database: "metrics", Measurement: "cpu",
			RowsImported: 3, PartitionsCreated: 1,
			Columns: []string{"time", "host", "value"}, DurationMs: 17,
		})
	}))
	defer srv.Close()

	c := freshClient(t, srv, "metrics")
	filePath := writeTempFile(t, "data.csv", "time,host,value\n1,a,1.0\n2,b,2.0\n3,c,3.0\n")

	result, err := c.ImportCSV(context.Background(), filePath, "metrics", "cpu", CSVImportOptions{
		TimeColumn: "time",
		TimeFormat: "epoch_s",
		Delimiter:  ",",
		SkipRows:   1,
	})
	if err != nil {
		t.Fatalf("ImportCSV: %v", err)
	}

	if gotMethod != "POST" {
		t.Errorf("method = %q", gotMethod)
	}
	if gotPath != "/api/v1/import/csv" {
		t.Errorf("path = %q", gotPath)
	}
	if !strings.HasPrefix(gotCT, "multipart/form-data;") {
		t.Errorf("content-type = %q; expected multipart/form-data;...", gotCT)
	}
	if gotDB != "metrics" {
		t.Errorf("x-arc-database = %q", gotDB)
	}
	if gotAuth != "Bearer test-token" {
		t.Errorf("auth = %q", gotAuth)
	}
	if gotQuery.Get("measurement") != "cpu" {
		t.Errorf("measurement query = %q", gotQuery.Get("measurement"))
	}
	if gotQuery.Get("time_column") != "time" {
		t.Errorf("time_column query = %q", gotQuery.Get("time_column"))
	}
	if gotQuery.Get("time_format") != "epoch_s" {
		t.Errorf("time_format query = %q", gotQuery.Get("time_format"))
	}
	if gotQuery.Get("delimiter") != "," {
		t.Errorf("delimiter query = %q", gotQuery.Get("delimiter"))
	}
	if gotQuery.Get("skip_rows") != "1" {
		t.Errorf("skip_rows query = %q", gotQuery.Get("skip_rows"))
	}
	if gotFilename != "data.csv" {
		t.Errorf("uploaded filename = %q", gotFilename)
	}
	if !strings.HasPrefix(gotFileContent, "time,host,value\n") {
		t.Errorf("uploaded body = %q", gotFileContent)
	}
	if result.RowsImported != 3 || result.Measurement != "cpu" {
		t.Errorf("decoded result = %+v", result)
	}
}

func TestImportCSV_OmitsEmptyOptionalQueryParams(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		// Drain the body so the upload goroutine completes.
		_, _, _ = r.FormFile("file") // best-effort
		_ = r.ParseMultipartForm(1 << 20)
		writeImportEnvelope(t, w, ImportResult{Database: "metrics", Measurement: "cpu"})
	}))
	defer srv.Close()

	c := freshClient(t, srv, "metrics")
	filePath := writeTempFile(t, "data.csv", "x\n1\n")
	if _, err := c.ImportCSV(context.Background(), filePath, "metrics", "cpu", CSVImportOptions{}); err != nil {
		t.Fatalf("ImportCSV: %v", err)
	}
	// Only `measurement` should be present.
	if _, ok := gotQuery["time_column"]; ok {
		t.Errorf("time_column sent even though empty: %v", gotQuery)
	}
	if _, ok := gotQuery["time_format"]; ok {
		t.Errorf("time_format sent even though empty: %v", gotQuery)
	}
	if _, ok := gotQuery["delimiter"]; ok {
		t.Errorf("delimiter sent even though empty: %v", gotQuery)
	}
	if _, ok := gotQuery["skip_rows"]; ok {
		t.Errorf("skip_rows sent even though 0: %v", gotQuery)
	}
	if gotQuery.Get("measurement") != "cpu" {
		t.Errorf("measurement not sent: %v", gotQuery)
	}
}

func TestImportCSV_MissingFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should not be called when file is missing")
	}))
	defer srv.Close()

	c := freshClient(t, srv, "metrics")
	_, err := c.ImportCSV(context.Background(), "/nonexistent/path.csv", "metrics", "cpu", CSVImportOptions{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "open") {
		t.Errorf("error %q lacks open context", err)
	}
}

func TestImportCSV_MissingDatabase(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should not be called")
	}))
	defer srv.Close()
	c := freshClient(t, srv, "")
	_, err := c.ImportCSV(context.Background(), "/nonexistent", "", "cpu", CSVImportOptions{})
	if err == nil || !strings.Contains(err.Error(), "database is required") {
		t.Errorf("expected database-required error, got %v", err)
	}
}

// TestImportCSV_RejectsEmptyMeasurement and the three TimeFormat/
// Delimiter tests below pin the client-boundary validations added in
// Gemini PR #3 round 5. Each fails before the server is contacted,
// so the test handler's t.Fatal would fire on any regression.

func TestImportCSV_RejectsEmptyMeasurement(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should not be called with empty measurement")
	}))
	defer srv.Close()
	c := freshClient(t, srv, "metrics")
	filePath := writeTempFile(t, "x.csv", "x\n1\n")
	_, err := c.ImportCSV(context.Background(), filePath, "metrics", "", CSVImportOptions{})
	if err == nil || !strings.Contains(err.Error(), "measurement is required") {
		t.Errorf("expected measurement-required error, got %v", err)
	}
}

func TestImportCSV_RejectsMultiCharDelimiter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should not be called with multi-char delimiter")
	}))
	defer srv.Close()
	c := freshClient(t, srv, "metrics")
	filePath := writeTempFile(t, "x.csv", "x\n1\n")
	_, err := c.ImportCSV(context.Background(), filePath, "metrics", "cpu", CSVImportOptions{Delimiter: "||"})
	if err == nil || !strings.Contains(err.Error(), "Delimiter must be a single character") {
		t.Errorf("expected delimiter-single-char error, got %v", err)
	}
}

func TestImportCSV_AcceptsSingleRuneDelimiter(t *testing.T) {
	// A multi-byte UTF-8 single rune (e.g. en-dash, U+2013, 3 bytes)
	// should pass the `len([]rune) == 1` check.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(1 << 20)
		writeImportEnvelope(t, w, ImportResult{Database: "metrics", Measurement: "cpu"})
	}))
	defer srv.Close()
	c := freshClient(t, srv, "metrics")
	filePath := writeTempFile(t, "x.csv", "x\n1\n")
	if _, err := c.ImportCSV(context.Background(), filePath, "metrics", "cpu", CSVImportOptions{Delimiter: "—"}); err != nil {
		t.Errorf("single multi-byte rune should be accepted, got %v", err)
	}
}

func TestImportCSV_RejectsInvalidTimeFormat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should not be called with invalid time_format")
	}))
	defer srv.Close()
	c := freshClient(t, srv, "metrics")
	filePath := writeTempFile(t, "x.csv", "x\n1\n")
	_, err := c.ImportCSV(context.Background(), filePath, "metrics", "cpu", CSVImportOptions{TimeFormat: "epoch_milliseconds"})
	if err == nil || !strings.Contains(err.Error(), "invalid CSVImportOptions.TimeFormat") {
		t.Errorf("expected time-format-enum error, got %v", err)
	}
}

func TestImportCSV_AcceptsEmptyTimeFormat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(1 << 20)
		writeImportEnvelope(t, w, ImportResult{Database: "metrics", Measurement: "cpu"})
	}))
	defer srv.Close()
	c := freshClient(t, srv, "metrics")
	filePath := writeTempFile(t, "x.csv", "x\n1\n")
	if _, err := c.ImportCSV(context.Background(), filePath, "metrics", "cpu", CSVImportOptions{TimeFormat: ""}); err != nil {
		t.Errorf("empty TimeFormat should be accepted (means infer), got %v", err)
	}
}

func TestImportParquet_RejectsEmptyMeasurement(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should not be called with empty measurement")
	}))
	defer srv.Close()
	c := freshClient(t, srv, "metrics")
	filePath := writeTempFile(t, "x.parquet", "fake")
	_, err := c.ImportParquet(context.Background(), filePath, "metrics", "", ParquetImportOptions{})
	if err == nil || !strings.Contains(err.Error(), "measurement is required") {
		t.Errorf("expected measurement-required error, got %v", err)
	}
}

// TestImportCSV_RejectsNegativeSkipRows pins the client-layer
// validation Gemini added in PR #3 round 4. The cobra layer rejects
// negative --skip-rows at RunE, but the client must ALSO reject so
// any future caller bypassing cobra can't silently drop the value.
func TestImportCSV_RejectsNegativeSkipRows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should not be called when SkipRows is negative")
	}))
	defer srv.Close()

	c := freshClient(t, srv, "metrics")
	filePath := writeTempFile(t, "data.csv", "x\n1\n")
	_, err := c.ImportCSV(context.Background(), filePath, "metrics", "cpu", CSVImportOptions{SkipRows: -3})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "SkipRows must be >= 0") {
		t.Errorf("error %q lacks SkipRows context", err)
	}
}

func TestImportCSV_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"measurement query parameter is required"}`)
	}))
	defer srv.Close()

	c := freshClient(t, srv, "metrics")
	filePath := writeTempFile(t, "data.csv", "x\n1\n")
	_, err := c.ImportCSV(context.Background(), filePath, "metrics", "cpu", CSVImportOptions{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "measurement query parameter is required") {
		t.Errorf("error %q lacks server message", err)
	}
	if !strings.Contains(err.Error(), "HTTP 400") {
		t.Errorf("error %q lacks status", err)
	}
}

func TestImportParquet_BasicShape(t *testing.T) {
	var (
		gotPath  string
		gotQuery url.Values
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query()
		_ = r.ParseMultipartForm(1 << 20)
		writeImportEnvelope(t, w, ImportResult{
			Database: "metrics", Measurement: "cpu", RowsImported: 100,
		})
	}))
	defer srv.Close()

	c := freshClient(t, srv, "metrics")
	filePath := writeTempFile(t, "data.parquet", "fake parquet bytes")
	if _, err := c.ImportParquet(context.Background(), filePath, "metrics", "cpu", ParquetImportOptions{TimeColumn: "ts"}); err != nil {
		t.Fatalf("ImportParquet: %v", err)
	}
	if gotPath != "/api/v1/import/parquet" {
		t.Errorf("path = %q", gotPath)
	}
	if gotQuery.Get("measurement") != "cpu" {
		t.Errorf("measurement query = %q", gotQuery.Get("measurement"))
	}
	if gotQuery.Get("time_column") != "ts" {
		t.Errorf("time_column query = %q", gotQuery.Get("time_column"))
	}
}

func TestImportLP_PrecisionAndMeasurementFilter(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		_ = r.ParseMultipartForm(1 << 20)
		writeImportEnvelope(t, w, LPImportResult{
			Database: "metrics", Measurements: []string{"cpu"},
			RowsImported: 42, Precision: "ms", DurationMs: 5,
		})
	}))
	defer srv.Close()

	c := freshClient(t, srv, "metrics")
	filePath := writeTempFile(t, "data.lp", "cpu v=1 1700000000000\n")
	result, err := c.ImportLP(context.Background(), filePath, "metrics", LPImportOptions{
		Precision:         PrecisionMS,
		MeasurementFilter: "cpu",
	})
	if err != nil {
		t.Fatalf("ImportLP: %v", err)
	}
	if gotQuery.Get("precision") != "ms" {
		t.Errorf("precision query = %q", gotQuery.Get("precision"))
	}
	if gotQuery.Get("measurement") != "cpu" {
		t.Errorf("measurement filter query = %q", gotQuery.Get("measurement"))
	}
	if result.RowsImported != 42 || result.Measurements[0] != "cpu" {
		t.Errorf("decoded result = %+v", result)
	}
}

func TestImportLP_InvalidPrecision(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should not be called")
	}))
	defer srv.Close()

	c := freshClient(t, srv, "metrics")
	filePath := writeTempFile(t, "data.lp", "cpu v=1\n")
	_, err := c.ImportLP(context.Background(), filePath, "metrics", LPImportOptions{Precision: Precision("furlong")})
	if err == nil || !strings.Contains(err.Error(), "invalid precision") {
		t.Errorf("expected invalid-precision error, got %v", err)
	}
}

func TestImportLP_NoOptionalQueryParamsWhenEmpty(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		_ = r.ParseMultipartForm(1 << 20)
		writeImportEnvelope(t, w, LPImportResult{Database: "metrics"})
	}))
	defer srv.Close()

	c := freshClient(t, srv, "metrics")
	filePath := writeTempFile(t, "data.lp", "cpu v=1\n")
	if _, err := c.ImportLP(context.Background(), filePath, "metrics", LPImportOptions{}); err != nil {
		t.Fatalf("ImportLP: %v", err)
	}
	if _, ok := gotQuery["precision"]; ok {
		t.Errorf("precision sent even though empty: %v", gotQuery)
	}
	if _, ok := gotQuery["measurement"]; ok {
		t.Errorf("measurement sent even though empty: %v", gotQuery)
	}
}

func TestImportTLE_MeasurementGoesViaHeader(t *testing.T) {
	var (
		gotPath      string
		gotMeasHdr   string
		gotMeasQuery string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMeasHdr = r.Header.Get("x-arc-measurement")
		gotMeasQuery = r.URL.Query().Get("measurement")
		_ = r.ParseMultipartForm(1 << 20)
		writeImportEnvelope(t, w, TLEImportResult{
			Database: "satellites", Measurement: "starlink",
			SatelliteCount: 12, RowsImported: 12, DurationMs: 3,
		})
	}))
	defer srv.Close()

	c := freshClient(t, srv, "satellites")
	filePath := writeTempFile(t, "starlink.tle", "STARLINK-1\n1 11111U\n2 22222\n")
	if _, err := c.ImportTLE(context.Background(), filePath, "satellites", TLEImportOptions{Measurement: "starlink"}); err != nil {
		t.Fatalf("ImportTLE: %v", err)
	}
	if gotPath != "/api/v1/import/tle" {
		t.Errorf("path = %q", gotPath)
	}
	// Server uses HEADER not query param for TLE measurement override.
	if gotMeasHdr != "starlink" {
		t.Errorf("x-arc-measurement header = %q; expected starlink", gotMeasHdr)
	}
	if gotMeasQuery != "" {
		t.Errorf("measurement query param = %q; expected empty (TLE uses header)", gotMeasQuery)
	}
}

func TestImportTLE_NoMeasurementHeaderWhenOmitted(t *testing.T) {
	// When user doesn't pass --measurement, arcli must NOT send the
	// header so the server can apply its default ("satellite_tle").
	hdrPresent := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hdrPresent = r.Header[http.CanonicalHeaderKey("x-arc-measurement")]
		_ = r.ParseMultipartForm(1 << 20)
		writeImportEnvelope(t, w, TLEImportResult{Database: "sats", Measurement: "satellite_tle"})
	}))
	defer srv.Close()

	c := freshClient(t, srv, "sats")
	filePath := writeTempFile(t, "f.tle", "X\n1\n2\n")
	if _, err := c.ImportTLE(context.Background(), filePath, "sats", TLEImportOptions{}); err != nil {
		t.Fatalf("ImportTLE: %v", err)
	}
	if hdrPresent {
		t.Errorf("x-arc-measurement header was sent despite empty override")
	}
}

func TestUploadMultipart_StreamsLargeFileWithoutBuffering(t *testing.T) {
	// Smoke test: write a 5 MiB file and confirm the server receives
	// exactly the same bytes. This isn't a real memory-pressure test
	// (Go heap behavior under test conditions is noisy), but it does
	// confirm the io.Pipe + goroutine plumbing works correctly under
	// non-trivial payload sizes.
	const size = 5 * 1024 * 1024
	var sentBytes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(1 << 30)
		_, _, sent := drainMultipartFile(t, r)
		sentBytes = sent
		writeImportEnvelope(t, w, ImportResult{Database: "x", Measurement: "y", RowsImported: 0})
	}))
	defer srv.Close()

	dir := t.TempDir()
	p := filepath.Join(dir, "big.csv")
	buf := make([]byte, size)
	for i := range buf {
		buf[i] = byte('a' + (i % 26))
	}
	if err := os.WriteFile(p, buf, 0o600); err != nil {
		t.Fatalf("write big file: %v", err)
	}

	c := freshClient(t, srv, "x")
	if _, err := c.ImportCSV(context.Background(), p, "x", "y", CSVImportOptions{}); err != nil {
		t.Fatalf("ImportCSV: %v", err)
	}
	if sentBytes != size {
		t.Errorf("server received %d bytes, want %d", sentBytes, size)
	}
}

// drainMultipartFile is a helper for the streaming test — counts the
// bytes the server actually received in the file field.
func drainMultipartFile(t *testing.T, r *http.Request) (string, string, int) {
	t.Helper()
	f, hdr, err := r.FormFile("file")
	if err != nil {
		t.Fatalf("FormFile: %v", err)
	}
	defer f.Close()
	n, err := io.Copy(io.Discard, f)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	return hdr.Filename, "", int(n)
}

// TestUploadMultipart_NoGoroutineLeakOnEarlyDoFailure runs the import
// path against a refused-connection port and verifies that no goroutine
// is leaked across many iterations.
//
// Context: Gemini PR #3 round-3 raised the concern that if
// c.http.Do returns an error before the transport begins reading the
// request body, the io.Pipe writer goroutine could block on pw.Write
// forever and leak both the goroutine and the underlying file handle.
// The fix is `defer pr.Close()` immediately after io.Pipe creation, so
// the pipe is closed on every return path regardless of transport
// behavior.
//
// Empirically (Go 1.25), net/http's transport DOES close req.Body on
// the connection-refused path, so the leak isn't reproducible by simply
// refusing the TCP connection — `delta=0` even without the fix. This
// test therefore exists as a guard rather than a strict reproducer: if
// a future Go change (or a different early-failure path like
// pre-send ctx-cancel) starts leaking, the linear growth would surface
// here. We accept that the mutation test passes today — the defense-in-
// depth value of the fix isn't reproducible in current Go, but the
// `defer pr.Close()` is still correct hygiene.
func TestUploadMultipart_NoGoroutineLeakOnEarlyDoFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadAddr := ln.Addr().String()
	_ = ln.Close()

	c, err := New(Config{
		Endpoint: "http://" + deadAddr,
		Token:    "t",
		Timeout:  2 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	filePath := writeTempFile(t, "x.csv", "a,b\n1,2\n")

	runtime.GC()
	runtime.Gosched()
	time.Sleep(50 * time.Millisecond)
	before := runtime.NumGoroutine()

	const iterations = 8
	for i := 0; i < iterations; i++ {
		_, err := c.ImportCSV(context.Background(), filePath, "metrics", "cpu", CSVImportOptions{})
		if err == nil {
			t.Fatal("expected error from refused connection, got nil")
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	var after int
	for time.Now().Before(deadline) {
		runtime.GC()
		after = runtime.NumGoroutine()
		if after-before < iterations {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if after-before >= iterations {
		t.Errorf("goroutine leak: before=%d after=%d delta=%d iterations=%d", before, after, after-before, iterations)
	}
}

// TestContentTypeIsValidMultipart verifies the Content-Type header the
// client sends parses as real multipart/form-data with a boundary
// parameter, rather than relying on a brittle prefix check.
func TestContentTypeIsValidMultipart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ct := r.Header.Get("Content-Type")
		mediaType, params, err := mime.ParseMediaType(ct)
		if err != nil {
			t.Errorf("Content-Type %q invalid: %v", ct, err)
		}
		if mediaType != "multipart/form-data" {
			t.Errorf("media type = %q, want multipart/form-data", mediaType)
		}
		if params["boundary"] == "" {
			t.Errorf("Content-Type missing boundary param: %q", ct)
		}
		_ = r.ParseMultipartForm(1 << 20)
		writeImportEnvelope(t, w, ImportResult{})
	}))
	defer srv.Close()

	c := freshClient(t, srv, "metrics")
	filePath := writeTempFile(t, "x.csv", "a\n1\n")
	if _, err := c.ImportCSV(context.Background(), filePath, "metrics", "y", CSVImportOptions{}); err != nil {
		t.Fatalf("ImportCSV: %v", err)
	}
}
