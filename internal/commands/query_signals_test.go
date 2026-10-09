package commands

import (
	"net/http"
	"net/http/httptest"
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
