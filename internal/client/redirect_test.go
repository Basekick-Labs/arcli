package client

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRedirectRejectsMethodChanges(t *testing.T) {
	operations := []struct {
		name   string
		method string
		run    func(*Client) error
	}{
		{"line protocol", http.MethodPost, func(c *Client) error {
			return c.WriteLineProtocol(context.Background(), strings.NewReader("cpu value=1"), "metrics", "")
		}},
		{"msgpack", http.MethodPost, func(c *Client) error {
			return c.WriteMsgPack(context.Background(), strings.NewReader("\x80"), 1, "metrics")
		}},
		{"delete database", http.MethodDelete, func(c *Client) error {
			return c.DeleteDatabase(context.Background(), "metrics")
		}},
	}
	for _, status := range []int{301, 302, 303} {
		for _, op := range operations {
			t.Run(fmt.Sprintf("%d/%s", status, op.name), func(t *testing.T) {
				var redirected atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/redirected" {
						redirected.Add(1)
						w.WriteHeader(http.StatusOK)
						return
					}
					_, _ = io.Copy(io.Discard, r.Body)
					http.Redirect(w, r, "/redirected", status)
				}))
				defer srv.Close()
				err := op.run(freshClient(t, srv, ""))
				if err == nil || !strings.Contains(err.Error(), op.method+" to GET") {
					t.Errorf("operation returned %v; want a method-changing redirect error", err)
				}
				if got := redirected.Load(); got != 0 {
					t.Errorf("redirect target received %d requests; want none", got)
				}
			})
		}
	}
}

func TestRedirectPreservesReplayablePost(t *testing.T) {
	for _, status := range []int{307, 308} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var redirected atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				if r.Method != http.MethodPost || string(body) != `{"sql":"SELECT 1"}` {
					t.Errorf("request = %s %s; want POST with the query body", r.Method, body)
				}
				if r.URL.Path != "/redirected" {
					http.Redirect(w, r, "/redirected", status)
					return
				}
				redirected.Add(1)
				_, _ = io.WriteString(w, `{"columns":["n"],"data":[[1]],"row_count":1}`)
			}))
			defer srv.Close()
			qr, err := freshClient(t, srv, "").QueryJSON(context.Background(), "SELECT 1", "")
			if err != nil {
				t.Fatal(err)
			}
			if redirected.Load() != 1 || qr.RowCount != 1 {
				t.Fatalf("redirects=%d, result=%+v", redirected.Load(), qr)
			}
		})
	}
}

func TestRedirectPreservesGetAndLimit(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/health" {
					http.Redirect(w, r, "/redirected", status)
					return
				}
				_, _ = io.WriteString(w, `{"status":"ok","time":"2026-01-01T00:00:00Z","uptime":"1h"}`)
			}))
			defer srv.Close()
			if _, err := freshClient(t, srv, "").Health(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Redirect(w, r, "/health", http.StatusFound)
	}))
	defer srv.Close()
	_, err := freshClient(t, srv, "").Health(context.Background())
	if err == nil || !strings.Contains(err.Error(), "stopped after 10 redirects") {
		t.Errorf("redirect loop error = %v", err)
	}
	if got := requests.Load(); got != 10 {
		t.Errorf("redirect loop sent %d requests; want 10", got)
	}
}
