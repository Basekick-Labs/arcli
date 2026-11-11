package commands

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"testing/iotest"
)

func TestTokenStdinReadErrorPreservesConfig(t *testing.T) {
	for _, operation := range []string{"create", "update"} {
		for _, tc := range []struct{ name, prefix string }{{"empty", ""}, {"partial", "partial-token"}} {
			t.Run(operation+"/"+tc.name, func(t *testing.T) {
				prefix := tc.prefix
				path := writeTestConfig(t, "http://unused", "existing-token")
				before, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				readErr := errors.New("stdin read failed")
				cmd := newConfigCreateCmd()
				args := []string{"--name", "new", "--endpoint", "http://unused", "--token-stdin"}
				if operation == "update" {
					cmd = newConfigUpdateCmd()
					args = []string{"local", "--token-stdin"}
				}
				cmd.SetIn(io.MultiReader(strings.NewReader(prefix), iotest.ErrReader(readErr)))
				out, stderr, err := execCmd(t, cmd, args...)
				if !errors.Is(err, readErr) {
					t.Errorf("error = %v; want the original read error", err)
				}
				if out != "" {
					t.Errorf("failed command printed success: %q", out)
				}
				if prefix != "" && strings.Contains(out+stderr+fmt.Sprint(err), prefix) {
					t.Error("partial token appeared in command output")
				}
				after, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(before, after) {
					t.Error("config changed after stdin failed")
				}
			})
		}
	}
}

func TestTokenStdinAcceptsCompleteInput(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		end         error
	}{
		{"EOF without newline", "complete-token", io.EOF},
		{"newline before error", "complete-token\n", errors.New("after first line")},
		{"CRLF before error", "complete-token\r\nignored", errors.New("after first line")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newConfigCreateCmd()
			// Only the first line is the token. A later read failure must not
			// invalidate a complete line that has already been received.
			cmd.SetIn(io.MultiReader(strings.NewReader(tc.input), iotest.ErrReader(tc.end)))
			got, err := readTokenFromStdin(cmd)
			if err != nil || got != "complete-token" {
				t.Errorf("token=%q err=%v", got, err)
			}
		})
	}
}
