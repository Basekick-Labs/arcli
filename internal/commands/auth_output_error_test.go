package commands

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func TestAuthSecretOutputErrors(t *testing.T) {
	for _, operation := range []string{"create", "rotate", "rotate with save"} {
		for _, format := range []string{"table", "json"} {
			for _, failure := range []string{"closed file", "partial write"} {
				t.Run(operation+"/"+format+"/"+failure, func(t *testing.T) {
					srv, _ := fakeAuthServer(t)
					path := writeTestConfig(t, srv.URL, "admin-secret")
					before, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					var out io.Writer
					wantErr := io.ErrClosedPipe
					if failure == "closed file" {
						f, err := os.CreateTemp(t.TempDir(), "stdout")
						if err != nil {
							t.Fatal(err)
						}
						if err := f.Close(); err != nil {
							t.Fatal(err)
						}
						out, wantErr = f, os.ErrClosed
					} else {
						out = querySignalWriter(func([]byte) (int, error) { return 1, wantErr })
					}
					cmd := newAuthTokenCreateCmd()
					args := []string{"--name", "ci", "-o", format}
					if strings.HasPrefix(operation, "rotate") {
						cmd = newAuthTokenRotateCmd()
						args = []string{"admin", "--yes", "-o", format}
					}
					if operation == "rotate with save" {
						args = append(args, "--save")
					}
					var stderr bytes.Buffer
					cmd.SetOut(out)
					cmd.SetErr(&stderr)
					cmd.SilenceUsage, cmd.SilenceErrors = true, true
					cmd.SetArgs(args)
					if err := cmd.Execute(); !errors.Is(err, wantErr) {
						t.Errorf("error = %v; want the output error", err)
					}
					if strings.Contains(stderr.String(), "Created token") || strings.Contains(stderr.String(), "Rotated token") {
						t.Errorf("success reported after output failed: %q", stderr.String())
					}
					after, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					if operation == "rotate with save" {
						if bytes.Count(after, []byte("rotated-secret==")) != 2 || bytes.Contains(after, []byte("admin-secret")) {
							t.Error("new secret was not saved after stdout failed")
						}
					} else if !bytes.Equal(before, after) {
						t.Error("config changed despite not requesting --save")
					}
				})
			}
		}
	}
}

func TestAuthRotateReportsOutputAndSaveFailures(t *testing.T) {
	for _, format := range []string{"table", "json"} {
		t.Run(format, func(t *testing.T) {
			srv, _ := fakeAuthServer(t)
			path := writeTestConfig(t, srv.URL, "admin-secret")
			cmd := newAuthTokenRotateCmd()
			cmd.SetOut(querySignalWriter(func([]byte) (int, error) {
				// The writable-directory preflight has already passed, but the
				// config can still become unreadable before the post-rotate load.
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				return 0, io.ErrClosedPipe
			}))
			var stderr bytes.Buffer
			cmd.SetErr(&stderr)
			cmd.SilenceUsage, cmd.SilenceErrors = true, true
			cmd.SetArgs([]string{"admin", "--yes", "--save", "-o", format})
			err := cmd.Execute()
			if !errors.Is(err, io.ErrClosedPipe) {
				t.Errorf("error = %v; want the output failure", err)
			}
			var pathErr *os.PathError
			if !errors.As(err, &pathErr) {
				t.Errorf("error = %v; want the config read failure too", err)
			}
			if strings.Contains(stderr.String(), "printed above") {
				t.Error("claimed the new secret was printed despite the output failure")
			}
		})
	}
}
