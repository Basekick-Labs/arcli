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

func TestWriteJSON_PreservesNanosecondTimestamp(t *testing.T) {
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
	cmd.SetIn(strings.NewReader(`{"m":"cpu","t":1.700000000000000001e18,"fields":{"x":1}}`))
	out, _, err := execCmd(t, cmd, "--format", "json")
	if err != nil {
		t.Fatal(err)
	}
	body := <-bodies
	// The final int64 is 1700000000000000001, retaining the last nanosecond.
	want, err := hex.DecodeString("83a66669656c647381a17801a16da3637075a174d317979cfe362a0001")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, want) {
		t.Errorf("uploaded MessagePack = %x, want %x", body, want)
	}
	if !strings.Contains(out, "OK: 1 measurement(s), 1 row(s)") {
		t.Errorf("stdout = %q", out)
	}
}
