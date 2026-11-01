package commands

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/basekick-labs/arcli/internal/client"
)

func TestWriteJSON_RejectsOversizeTrailingWhitespaceBeforeUpload(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	writeTestConfig(t, srv.URL, "tok")

	const document = `{"m":"cpu","fields":{"x":1}}`
	cmd := newWriteCmd()
	cmd.SetIn(io.MultiReader(
		strings.NewReader(document),
		strings.NewReader(strings.Repeat(" ", client.JSONDocMaxBytes-len(document)+1)),
	))
	out, _, err := execCmd(t, cmd, "--format", "json")
	if err == nil || !strings.Contains(err.Error(), "JSON document exceeds") {
		t.Errorf("error = %v, want size-limit error", err)
	}
	if out != "" {
		t.Errorf("stdout = %q, want no success output", out)
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("sent %d HTTP requests for rejected input", got)
	}
}
