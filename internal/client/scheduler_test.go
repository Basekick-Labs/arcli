package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestSchedulerActions_Success(t *testing.T) {
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertAuthHeaders(t, r)
		if r.Method != http.MethodPost || r.ContentLength != 0 {
			t.Errorf("request method=%s body length=%d", r.Method, r.ContentLength)
		}
		switch r.URL.Path {
		case "/api/v1/schedulers/cq/reload":
			_, _ = w.Write([]byte(`{"message":"CQ scheduler reloaded successfully","job_count":3,"extra":"kept"}`))
		case "/api/v1/schedulers/retention/trigger":
			_, _ = w.Write([]byte(`{"message":"Retention triggered successfully","extra":"kept"}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	ctx := context.Background()
	reload, err := cli.ReloadCQScheduler(ctx)
	if err != nil || reload.Message != "CQ scheduler reloaded successfully" || reload.JobCount == nil || *reload.JobCount != 3 || !strings.Contains(string(reload.Raw), `"extra":"kept"`) {
		t.Errorf("reload=%+v err=%v", reload, err)
	}
	trigger, err := cli.TriggerRetentionScheduler(ctx)
	if err != nil || trigger.Message != "Retention triggered successfully" || trigger.JobCount != nil || !strings.Contains(string(trigger.Raw), `"extra":"kept"`) {
		t.Errorf("trigger=%+v err=%v", trigger, err)
	}
}

func TestSchedulerActions_PreserveDistinctArcErrors(t *testing.T) {
	actions := []struct {
		name string
		call func(*Client) error
	}{
		{"CQ reload", func(c *Client) error { _, err := c.ReloadCQScheduler(context.Background()); return err }},
		{"retention trigger", func(c *Client) error { _, err := c.TriggerRetentionScheduler(context.Background()); return err }},
	}
	cases := []struct {
		name    string
		status  int
		message string
	}{
		{"admin permission", http.StatusForbidden, "Permission denied: admin required"},
		{"missing entitlement", http.StatusForbidden, "CQ scheduler is not running: Enterprise license required"},
		{"scheduler not running (Arc 26.09)", http.StatusServiceUnavailable, "Retention scheduler is not running - enterprise license required"},
		{"retention role restriction", http.StatusConflict, "Retention trigger refused: retention: node role is not primary writer"},
		{"retention role restriction (Arc 26.09)", http.StatusInternalServerError, "Retention trigger failed: retention: node role is not primary writer"},
		{"already running", http.StatusConflict, "Retention trigger refused: retention: a cycle is already running"},
	}
	for _, action := range actions {
		for _, tc := range cases {
			t.Run(action.name+"/"+tc.name, func(t *testing.T) {
				cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(tc.status)
					_ = json.NewEncoder(w).Encode(map[string]string{"error": tc.message})
				})
				err := action.call(cli)
				var he *HTTPError
				if !errors.As(err, &he) {
					t.Fatalf("error type = %T, want *HTTPError: %v", err, err)
				}
				if he.Status != tc.status || he.Message != tc.message {
					t.Errorf("HTTPError = %+v, want status %d and message %q", he, tc.status, tc.message)
				}
			})
		}
	}
}
