package commands

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteRejectsMethodChangingRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirected" {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		http.Redirect(w, r, "/redirected", http.StatusMovedPermanently)
	}))
	defer srv.Close()
	writeTestConfig(t, srv.URL, "")
	for _, tc := range []struct {
		format string
		body   string
	}{
		{"lp", "cpu value=1"},
		{"json", `{"m":"cpu","fields":{"value":1}}`},
		{"msgpack", "\x80"},
	} {
		t.Run(tc.format, func(t *testing.T) {
			cmd := newWriteCmd()
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(io.Discard)
			cmd.SetIn(strings.NewReader(tc.body))
			cmd.SetArgs([]string{"--format", tc.format})
			if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "POST to GET") {
				t.Errorf("write error = %v; want a method-changing redirect error", err)
			}
			if out.Len() != 0 {
				t.Errorf("failed write printed success: %q", out.String())
			}
		})
	}
}
