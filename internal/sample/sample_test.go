package sample

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testUA = "arcli/26.09.4 (darwin/arm64)"

// newTestClient points a Client at a test server. baseURL is unexported,
// so tests construct through NewClient and override it.
func newTestClient(t *testing.T, srvURL, installationID string) *Client {
	t.Helper()
	c := NewClient(testUA, installationID, 5*time.Second)
	c.baseURL = srvURL
	return c
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TestIdentityHeaders is the important one: sample downloads carry the
// same identity as any other arcli request, the opt-out is honoured, and
// no bearer token is ever sent to the sample host.
func TestIdentityHeaders(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		fmt.Fprint(w, `{"schema_version":1,"datasets":{"citibike":{"title":"X"}}}`)
	}))
	defer srv.Close()

	tests := []struct {
		name, id, want string
	}{
		{"sends the installation id", "0f0f0f0f-0f0f-4f0f-8f0f-0f0f0f0f0f0f", "0f0f0f0f-0f0f-4f0f-8f0f-0f0f0f0f0f0f"},
		{"opted out sends none", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := newTestClient(t, srv.URL, tc.id).Manifest(context.Background()); err != nil {
				t.Fatalf("Manifest: %v", err)
			}
			if g := got.Get(HeaderInstallationID); g != tc.want {
				t.Errorf("%s = %q, want %q", HeaderInstallationID, g, tc.want)
			}
			if ua := got.Get("User-Agent"); ua != testUA {
				t.Errorf("User-Agent = %q, want %q", ua, testUA)
			}
			if auth := got.Get("Authorization"); auth != "" {
				t.Errorf("Authorization leaked to the sample host: %q", auth)
			}
		})
	}
}

func TestManifestRejectsUnknownSchemaVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"schema_version":2,"datasets":{"x":{}}}`)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL, "").Manifest(context.Background())
	if err == nil {
		t.Fatal("expected an error for schema_version 2")
	}
	if !strings.Contains(err.Error(), "upgrade arcli") {
		t.Errorf("error should tell the user to upgrade, got: %v", err)
	}
}

func TestManifestStatusErrors(t *testing.T) {
	tests := []struct {
		code int
		want string
	}{
		{http.StatusForbidden, "User-Agent"},
		{http.StatusNotFound, "not found"},
		{http.StatusInternalServerError, "HTTP 500"},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprint(tc.code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.code)
			}))
			defer srv.Close()

			_, err := newTestClient(t, srv.URL, "").Manifest(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestDatasetAndMonthLookup(t *testing.T) {
	m := &Manifest{SchemaVersion: 1, Datasets: map[string]Dataset{
		"citibike": {Months: map[string]Month{
			"2025-11": {Parts: []Part{{Key: "a"}}},
			"2025-12": {Parts: []Part{{Key: "b"}}},
			"2026-01": {Parts: []Part{{Key: "c"}}},
		}},
	}}

	if _, err := m.Dataset("nope"); err == nil || !strings.Contains(err.Error(), "citibike") {
		t.Errorf("unknown dataset error should list what is available, got: %v", err)
	}

	d, err := m.Dataset("citibike")
	if err != nil {
		t.Fatal(err)
	}
	// Empty key means newest, and month keys sort lexically = chronologically.
	key, _, err := d.Month("")
	if err != nil {
		t.Fatal(err)
	}
	if key != "2026-01" {
		t.Errorf("default month = %q, want the newest (2026-01)", key)
	}
	if _, _, err := d.Month("1999-01"); err == nil {
		t.Error("expected an error for an unpublished month")
	}
}

// TestFetchDownloadsAndVerifies covers the whole download path: files
// land, checksums are enforced, and an already-verified file is reused
// instead of re-fetched.
func TestFetchDownloadsAndVerifies(t *testing.T) {
	body := []byte("parquet-ish bytes")
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write(body)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, "")
	dir := t.TempDir()
	parts := []Part{{
		Key:    "citibike/2025-12/v3/part-001.parquet",
		Bytes:  int64(len(body)),
		SHA256: sha256Hex(body),
	}}

	paths, err := c.Fetch(context.Background(), dir, parts, 2, nil)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(paths) != 1 {
		t.Fatalf("got %d paths, want 1", len(paths))
	}
	if got, _ := os.ReadFile(paths[0]); string(got) != string(body) {
		t.Errorf("downloaded content = %q, want %q", got, body)
	}
	if hits != 1 {
		t.Errorf("server hits = %d, want 1", hits)
	}

	// Second run: the file verifies, so nothing is re-downloaded.
	var cachedReported bool
	if _, err := c.Fetch(context.Background(), dir, parts, 2, func(_, _ int, _ Part, cached bool) {
		cachedReported = cached
	}); err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	if hits != 1 {
		t.Errorf("server hits after cached run = %d, want still 1", hits)
	}
	if !cachedReported {
		t.Error("progress should report the file as cached")
	}
}

// TestFetchRejectsBadChecksum proves a corrupt download never reaches the
// importer. The server always returns the wrong bytes, so the one retry
// also fails and the command errors rather than importing.
func TestFetchRejectsBadChecksum(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("wrong bytes"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	parts := []Part{{Key: "a/part-001.parquet", SHA256: sha256Hex([]byte("right bytes"))}}

	_, err := newTestClient(t, srv.URL, "").Fetch(context.Background(), dir, parts, 1, nil)
	if err == nil {
		t.Fatal("a checksum mismatch must fail the command")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("error = %v, want it to name the checksum mismatch", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "part-001.parquet")); !os.IsNotExist(statErr) {
		t.Error("the bad file must not be left on disk where a later run would trust it")
	}
}

// TestFetchReplacesStaleFile covers the resume case: a file left over
// from an interrupted run has the wrong checksum and is re-downloaded.
func TestFetchReplacesStaleFile(t *testing.T) {
	body := []byte("good")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	dir := t.TempDir()
	stale := filepath.Join(dir, "part-001.parquet")
	if err := os.WriteFile(stale, []byte("truncated"), 0o600); err != nil {
		t.Fatal(err)
	}
	parts := []Part{{Key: "a/part-001.parquet", Bytes: int64(len(body)), SHA256: sha256Hex(body)}}

	if _, err := newTestClient(t, srv.URL, "").Fetch(context.Background(), dir, parts, 1, nil); err != nil {
		t.Fatalf("Fetch should replace a stale file, got: %v", err)
	}
	if got, _ := os.ReadFile(stale); string(got) != string(body) {
		t.Errorf("stale file not replaced: %q", got)
	}
}

// TestFetchRejectsWrongSize guards the manifest-declared size bound.
func TestFetchRejectsWrongSize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("way more bytes than declared"))
	}))
	defer srv.Close()

	parts := []Part{{Key: "a/p.parquet", Bytes: 4, SHA256: sha256Hex([]byte("four"))}}
	_, err := newTestClient(t, srv.URL, "").Fetch(context.Background(), t.TempDir(), parts, 1, nil)
	if err == nil {
		t.Fatal("a response larger than the manifest declares must fail")
	}
}

func TestChecksumMatches(t *testing.T) {
	dir := t.TempDir()
	body := []byte("content")
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, err := checksumMatches(p, sha256Hex(body)); err != nil || !ok {
		t.Errorf("matching file: ok=%v err=%v", ok, err)
	}
	if ok, err := checksumMatches(p, sha256Hex([]byte("other"))); err != nil || ok {
		t.Errorf("mismatched file: ok=%v err=%v", ok, err)
	}
	// A missing file is "not there yet", not an error.
	if ok, err := checksumMatches(filepath.Join(dir, "absent"), sha256Hex(body)); err != nil || ok {
		t.Errorf("absent file should be (false, nil): ok=%v err=%v", ok, err)
	}
}

func TestDownloadDirIsDeterministic(t *testing.T) {
	a := DownloadDir("", "citibike", "2025-12")
	b := DownloadDir("", "citibike", "2025-12")
	if a != b {
		t.Errorf("DownloadDir must be stable across calls so an interrupted run resumes: %q vs %q", a, b)
	}
	if !strings.Contains(a, "citibike") || !strings.Contains(a, "2025-12") {
		t.Errorf("DownloadDir should namespace by dataset and month, got %q", a)
	}
	if got := DownloadDir("/custom", "citibike", "2025-12"); got != filepath.Join("/custom", "citibike", "2025-12") {
		t.Errorf("explicit root not honoured: %q", got)
	}
}

func TestNoRedirectOffHost(t *testing.T) {
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"schema_version":1,"datasets":{"x":{}}}`)
	}))
	defer elsewhere.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/manifest.json", http.StatusFound)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL, "").Manifest(context.Background())
	if err == nil {
		t.Fatal("a redirect off the sample host must fail, not be followed")
	}
}

// TestFetchFailsFast: a failure must stop the remaining downloads rather
// than pull the whole dataset before reporting a problem present from the
// first byte.
//
// Every part fails, which is the real-world shape this guards (a 403
// because the gate did not recognise this build fails for every part).
// Making them all fail also keeps the test deterministic: singling out
// one part would depend on the Go scheduler running that goroutine in the
// first concurrency-sized batch, which it does not guarantee.
func TestFetchFailsFast(t *testing.T) {
	var started int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&started, 1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	const total = 24
	parts := make([]Part, 0, total)
	for i := 0; i < total; i++ {
		parts = append(parts, Part{
			Key:    fmt.Sprintf("p/part-%03d.parquet", i),
			Bytes:  1,
			SHA256: sha256Hex([]byte("x")),
		})
	}

	if _, err := newTestClient(t, srv.URL, "").Fetch(context.Background(), t.TempDir(), parts, 4, nil); err == nil {
		t.Fatal("expected an error")
	}
	// With cancellation working, only the first concurrency-sized batch
	// (plus at most a straggler already past the check) ever reaches the
	// server. Without it, all 24 would.
	if n := atomic.LoadInt64(&started); n > 8 {
		t.Errorf("fail-fast not working: %d of %d parts reached the server after the first failure", n, total)
	}
}

// TestFetchAll403StopsEarly is the concrete regression: an unrecognised
// build 403s on every part and must not make 31 requests to find out.
func TestFetchAll403StopsEarly(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	parts := make([]Part, 0, 31)
	for i := 0; i < 31; i++ {
		parts = append(parts, Part{Key: fmt.Sprintf("p/part-%03d.parquet", i), Bytes: 1, SHA256: "aa"})
	}
	if _, err := newTestClient(t, srv.URL, "").Fetch(context.Background(), t.TempDir(), parts, 4, nil); err == nil {
		t.Fatal("expected an error")
	}
	if n := atomic.LoadInt64(&hits); n > 10 {
		t.Errorf("made %d requests for a dataset that 403s on every part", n)
	}
}

// TestProgressCountsOnlySuccesses: the progress line must never claim
// more files than actually landed, since the run may still fail.
func TestProgressCountsOnlySuccesses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "part-005.parquet") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Write([]byte("x"))
	}))
	defer srv.Close()

	parts := make([]Part, 0, 12)
	for i := 0; i < 12; i++ {
		parts = append(parts, Part{
			Key: fmt.Sprintf("p/part-%03d.parquet", i), Bytes: 1, SHA256: sha256Hex([]byte("x")),
		})
	}
	var maxReported int
	_, err := newTestClient(t, srv.URL, "").Fetch(context.Background(), t.TempDir(), parts, 2,
		func(done, total int, _ Part, _ bool) {
			if done > maxReported {
				maxReported = done
			}
			if done > total {
				t.Errorf("progress reported %d/%d", done, total)
			}
		})
	if err == nil {
		t.Fatal("expected an error")
	}
	if maxReported >= len(parts) {
		t.Errorf("progress reported %d/%d despite a failed file", maxReported, len(parts))
	}
}

// TestManifestValidation rejects the shapes that would silently corrupt a
// download rather than fail loudly.
func TestManifestValidation(t *testing.T) {
	tests := []struct {
		name, body, want string
	}{
		{
			"empty part key",
			`{"schema_version":1,"datasets":{"d":{"months":{"2025-12":{"parts":[{"key":"","bytes":1}]}}}}}`,
			"empty key",
		},
		{
			"negative size",
			`{"schema_version":1,"datasets":{"d":{"months":{"2025-12":{"parts":[{"key":"a/p.parquet","bytes":-1}]}}}}}`,
			"negative size",
		},
		{
			"duplicate basename",
			`{"schema_version":1,"datasets":{"d":{"months":{"2025-12":{"parts":[{"key":"a/p.parquet"},{"key":"b/p.parquet"}]}}}}}`,
			"share the filename",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()

			_, err := newTestClient(t, srv.URL, "").Manifest(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}
