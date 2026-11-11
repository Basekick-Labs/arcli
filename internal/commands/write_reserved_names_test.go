package commands

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestWriteJSONRejectsReservedNamesBeforeRequest(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"partial write: 1 points dropped; the other points were written"}`)
	}))
	defer srv.Close()
	writeTestConfig(t, srv.URL, "tok")
	for _, bad := range []string{
		`{"m":"cpu","fields":{"time":1}}`,
		`{"m":"cpu","fields":{"value":1},"tags":{"TIME":"label"}}`,
		`{"m":"cpu","columns":{"Time":[1700000000],"value":[1]}}`,
	} {
		cmd := newWriteCmd()
		cmd.SetIn(strings.NewReader(`{"batch":[{"m":"cpu","fields":{"value":1}},` + bad + `]}`))
		out, _, err := execCmd(t, cmd, "--format", "json")
		if err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Errorf("error = %v; want local reserved-name validation", err)
		}
		if out != "" {
			t.Errorf("invalid document printed success: %q", out)
		}
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("invalid batches reached the write endpoint %d times", got)
	}
}
