package commands

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestQueryJSON_ReportsTruncatedResultAfterRendering(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"columns":["n"],"data":[[1]],"row_count":1,"truncated":true,"truncation_reason":"disk read failed"}`))
	}))
	defer srv.Close()
	writeTestConfig(t, srv.URL, "tok")

	out, stderr, err := execCmd(t, newQueryCmd(), "-o", "json", "SELECT 1")
	if err == nil || !strings.Contains(err.Error(), "disk read failed") {
		t.Fatalf("error = %v, want truncation reason", err)
	}
	if !strings.Contains(out, `"truncated": true`) || !strings.Contains(out, `"truncation_reason": "disk read failed"`) {
		t.Errorf("JSON output did not preserve truncation signals: %s", out)
	}
	if !strings.Contains(stderr, "result is incomplete") {
		t.Errorf("stderr = %q, want incomplete-result notice", stderr)
	}
	var result struct {
		Data [][]int `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil || !reflect.DeepEqual(result.Data, [][]int{{1}}) {
		t.Errorf("partial data = %v, decode error = %v", result.Data, err)
	}
}

func TestQueryJSON_ReportsRowCapWithoutFailing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"columns":["n"],"data":[[1]],"row_count":1,"rows_capped":true,"row_cap":100}`))
	}))
	defer srv.Close()
	writeTestConfig(t, srv.URL, "tok")

	out, stderr, err := execCmd(t, newQueryCmd(), "-o", "json", "SELECT 1")
	if err != nil {
		t.Fatalf("capped result should remain successful: %v", err)
	}
	if !strings.Contains(out, `"rows_capped": true`) || !strings.Contains(out, `"row_cap": 100`) {
		t.Errorf("JSON output did not preserve row-cap signals: %s", out)
	}
	if !strings.Contains(stderr, "capped at 100 rows") {
		t.Errorf("stderr = %q, want row-cap notice", stderr)
	}
}

func TestQuerySignals_SanitizeDiagnosticsPreserveJSON(t *testing.T) {
	reason := "disk \x1b[2J\r\n\t\u202eread failed\u2069"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"columns": []string{"n"}, "data": [][]int{{1}}, "row_count": 1,
			"truncated": true, "truncation_reason": reason,
		})
	}))
	defer srv.Close()
	writeTestConfig(t, srv.URL, "")

	out, stderr, err := execCmd(t, newQueryCmd(), "-o", "json", "SELECT 1")
	if err == nil || err.Error() != "query result truncated: disk [2Jread failed" {
		t.Fatalf("error = %q, want sanitized truncation error", err)
	}
	if stderr != "query: result is incomplete: disk [2Jread failed\n" {
		t.Errorf("stderr = %q, want sanitized notice", stderr)
	}
	var result struct {
		Reason string `json:"truncation_reason"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil || result.Reason != reason {
		t.Errorf("JSON reason = %q, decode error = %v; want original metadata", result.Reason, err)
	}
}

func TestQuerySignals_ArrowSanitizesDiagnostics(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(http.TrailerPrefix+"Arc-Rows-Capped", "\u202e250\u2069")
		w.Header().Set(http.TrailerPrefix+"Arc-Stream-Truncated", "\u202ebatch failed\u2069")
		_, _ = io.WriteString(w, "partial-arrow")
	}))
	defer srv.Close()
	writeTestConfig(t, srv.URL, "")
	out, stderr, err := execCmd(t, newQueryCmd(), "-o", "arrow", "SELECT 1")
	if err == nil || err.Error() != "arrow stream truncated: batch failed" {
		t.Fatalf("error = %q, want sanitized truncation error", err)
	}
	if out != "partial-arrow" {
		t.Errorf("stdout = %q, want unchanged stream", out)
	}
	if !strings.Contains(stderr, "capped at 250 rows") || strings.ContainsAny(stderr, "\u202e\u2069") {
		t.Errorf("stderr = %q, want sanitized cap notice", stderr)
	}
	if strings.Contains(stderr, "server execution") {
		t.Errorf("truncated stream printed success timing: %q", stderr)
	}
}

func TestQuerySignals_OldServer(t *testing.T) {
	for _, format := range []string{"json", "arrow"} {
		t.Run(format, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if format == "arrow" {
					_, _ = io.WriteString(w, "legacy-arrow")
					return
				}
				_, _ = io.WriteString(w, `{"columns":["n"],"data":[[1]],"row_count":1}`)
			}))
			defer srv.Close()
			writeTestConfig(t, srv.URL, "")
			out, stderr, err := execCmd(t, newQueryCmd(), "-o", format, "SELECT 1")
			if err != nil {
				t.Fatalf("old server: %v", err)
			}
			if strings.Contains(stderr, "capped") || strings.Contains(stderr, "truncat") || strings.Contains(stderr, "incomplete") {
				t.Errorf("old server produced completeness notice: %q", stderr)
			}
			if format == "arrow" {
				if out != "legacy-arrow" {
					t.Errorf("stdout = %q", out)
				}
				return
			}
			var result map[string]any
			if err := json.Unmarshal([]byte(out), &result); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"truncated", "truncation_reason", "rows_capped", "row_cap"} {
				if _, ok := result[key]; ok {
					t.Errorf("old-server output added %q", key)
				}
			}
		})
	}
}

func TestQuerySignals_TruncatedAndCappedWithDisplayLimit(t *testing.T) {
	for _, format := range []string{"table", "csv"} {
		t.Run(format, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, `{"columns":["n"],"data":[["first"],["second"]],"row_count":2,"truncated":true,"truncation_reason":"read failed","rows_capped":true,"row_cap":2}`)
			}))
			defer srv.Close()
			writeTestConfig(t, srv.URL, "")
			out, stderr, err := execCmd(t, newQueryCmd(), "-o", format, "--limit", "1", "SELECT 1")
			if err == nil || !strings.Contains(err.Error(), "read failed") {
				t.Fatalf("error = %v, want truncation even with a cap", err)
			}
			if !strings.Contains(out, "first") || strings.Contains(out, "second") {
				t.Errorf("stdout = %q, want first row only", out)
			}
			if format == "csv" {
				rows, err := csv.NewReader(strings.NewReader(out)).ReadAll()
				if err != nil || !reflect.DeepEqual(rows, [][]string{{"n"}, {"first"}}) {
					t.Errorf("CSV = %v, error = %v", rows, err)
				}
			}
			if !strings.Contains(stderr, "capped at 2 rows") || !strings.Contains(stderr, "result is incomplete") {
				t.Errorf("stderr = %q, want both signals", stderr)
			}
		})
	}
}

func TestQuerySignals_BlankReason(t *testing.T) {
	for _, reason := range []string{"", " \t\r\n ", "\u202e\u2069"} {
		t.Run(reason, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"columns": []string{"n"}, "data": [][]int{{1}}, "row_count": 1,
					"truncated": true, "truncation_reason": reason,
				})
			}))
			defer srv.Close()
			writeTestConfig(t, srv.URL, "")
			_, _, err := execCmd(t, newQueryCmd(), "-o", "json", "SELECT 1")
			if err == nil || err.Error() != "query result truncated: the server could not finish streaming the result" {
				t.Errorf("error = %q, want fallback reason", err)
			}
		})
	}
}

type querySignalWriter func([]byte) (int, error)

func (f querySignalWriter) Write(p []byte) (int, error) { return f(p) }

func TestQuerySignals_OutputBeforeDiagnostic(t *testing.T) {
	for _, format := range []string{"json", "arrow"} {
		t.Run(format, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if format == "arrow" {
					w.Header().Set(http.TrailerPrefix+"Arc-Stream-Truncated", "read failed")
					_, _ = io.WriteString(w, "partial-arrow")
					return
				}
				_, _ = io.WriteString(w, `{"columns":["n"],"data":[[1]],"row_count":1,"truncated":true}`)
			}))
			defer srv.Close()
			writeTestConfig(t, srv.URL, "")
			c := newQueryCmd()
			var out bytes.Buffer
			c.SetOut(&out)
			seen := false
			c.SetErr(querySignalWriter(func(p []byte) (int, error) {
				seen = true
				if format == "arrow" {
					if out.String() != "partial-arrow" {
						t.Errorf("stdout at diagnostic = %q", out.String())
					}
				} else if !json.Valid(out.Bytes()) || !bytes.Contains(out.Bytes(), []byte(`"truncated": true`)) {
					t.Errorf("stdout at diagnostic is missing the complete JSON envelope: %q", out.String())
				}
				return len(p), nil
			}))
			c.SilenceUsage, c.SilenceErrors = true, true
			c.SetArgs([]string{"-o", format, "SELECT 1"})
			if err := c.Execute(); err == nil || !seen {
				t.Errorf("error = %v, saw diagnostic = %t", err, seen)
			}
		})
	}
}

func TestQuerySignals_OutputFailure(t *testing.T) {
	for _, format := range []string{"json", "arrow"} {
		t.Run(format, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, `{"columns":["n"],"data":[[1]],"row_count":1,"rows_capped":true,"row_cap":1}`)
			}))
			defer srv.Close()
			writeTestConfig(t, srv.URL, "")
			c := newQueryCmd()
			c.SetOut(querySignalWriter(func([]byte) (int, error) { return 0, io.ErrClosedPipe }))
			c.SetErr(io.Discard)
			c.SilenceUsage, c.SilenceErrors = true, true
			c.SetArgs([]string{"-o", format, "SELECT 1"})
			if err := c.Execute(); !errors.Is(err, io.ErrClosedPipe) {
				t.Errorf("error = %v, want output failure", err)
			}
		})
	}
}

func TestQuerySignals_ArrowTransportTruncation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "partial-arrow")
		w.(http.Flusher).Flush()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close() // Drop the connection without the terminating HTTP chunk.
	}))
	defer srv.Close()
	writeTestConfig(t, srv.URL, "")
	out, stderr, err := execCmd(t, newQueryCmd(), "-o", "arrow", "SELECT 1")
	if !errors.Is(err, io.ErrUnexpectedEOF) || out != "partial-arrow" {
		t.Errorf("error = %v, stdout = %q; want transport failure with received bytes", err, out)
	}
	if !strings.Contains(stderr, "stream interrupted") {
		t.Errorf("stderr = %q, want interrupted-stream notice", stderr)
	}
}

func TestQuerySignals_DiagnosticFailureDoesNotFailCappedResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"columns":["n"],"data":[[1]],"row_count":1,"rows_capped":true,"row_cap":1}`)
	}))
	defer srv.Close()
	writeTestConfig(t, srv.URL, "")
	c := newQueryCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(querySignalWriter(func([]byte) (int, error) { return 0, io.ErrClosedPipe }))
	c.SilenceUsage, c.SilenceErrors = true, true
	c.SetArgs([]string{"-o", "json", "SELECT 1"})
	if err := c.Execute(); err != nil || !json.Valid(out.Bytes()) {
		t.Errorf("error = %v, stdout = %q; diagnostic failure must not fail capped result", err, out.String())
	}
}

func TestQueryArrow_ReportsCompletenessTrailers(t *testing.T) {
	t.Run("truncated", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(http.TrailerPrefix+"Arc-Stream-Truncated", "batch write failed")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("partial-arrow"))
		}))
		defer srv.Close()
		writeTestConfig(t, srv.URL, "tok")

		out, stderr, err := execCmd(t, newQueryCmd(), "-o", "arrow", "SELECT 1")
		if err == nil || !strings.Contains(err.Error(), "batch write failed") {
			t.Fatalf("error = %v, want truncation reason", err)
		}
		if out != "partial-arrow" {
			t.Errorf("stdout = %q, want streamed payload", out)
		}
		if !strings.Contains(stderr, "server reported a truncated result") {
			t.Errorf("stderr = %q, want truncation notice", stderr)
		}
	})

	t.Run("row capped", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(http.TrailerPrefix+"Arc-Rows-Capped", "250")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("arrow"))
		}))
		defer srv.Close()
		writeTestConfig(t, srv.URL, "tok")

		out, stderr, err := execCmd(t, newQueryCmd(), "-o", "arrow", "SELECT 1")
		if err != nil {
			t.Fatalf("capped Arrow result should remain successful: %v", err)
		}
		if out != "arrow" {
			t.Errorf("stdout = %q, want streamed payload", out)
		}
		if !strings.Contains(stderr, "capped at 250 rows") {
			t.Errorf("stderr = %q, want row-cap notice", stderr)
		}
	})
}
