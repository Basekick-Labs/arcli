package commands

import (
	"bytes"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteJSON_ExactLargeIntegerInMixedColumn(t *testing.T) {
	bodies := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/write/msgpack" {
			t.Errorf("path = %q", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		bodies <- body
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	writeTestConfig(t, srv.URL, "tok")
	cmd := newWriteCmd()
	cmd.SetIn(strings.NewReader(`{"m":"cpu","columns":{"value":[18014398509481984,1.5]}}`))
	out, _, err := execCmd(t, cmd, "--format", "json")
	if err != nil {
		t.Fatal(err)
	}
	body := <-bodies
	// Both values use float64 tags; 2^54 is exactly representable.
	want, err := hex.DecodeString("82a7636f6c756d6e7381a576616c756592cb4350000000000000cb3ff8000000000000a16da3637075")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, want) {
		t.Errorf("uploaded MessagePack = %x, want %x", body, want)
	}
	if !strings.Contains(out, "OK: 1 measurement(s), 2 row(s)") {
		t.Errorf("stdout = %q", out)
	}
}
