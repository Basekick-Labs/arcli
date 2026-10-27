package client

import (
	"context"
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
	cases := []struct {
		name   string
		status int
		body   string
		want   string
		call   func(*Client) error
	}{
		{
			name:   "admin permission",
			status: http.StatusForbidden,
			body:   `{"error":"Permission denied: admin permission required"}`,
			want:   "admin permission required",
			call:   func(c *Client) error { _, err := c.ReloadCQScheduler(context.Background()); return err },
		},
		{
			name:   "disabled CQ scheduler",
			status: http.StatusServiceUnavailable,
			body:   `{"error":"CQ scheduler is not running - enterprise license required"}`,
			want:   "CQ scheduler is not running",
			call:   func(c *Client) error { _, err := c.ReloadCQScheduler(context.Background()); return err },
		},
		{
			name:   "disabled retention scheduler",
			status: http.StatusServiceUnavailable,
			body:   `{"error":"Retention scheduler is not running - enterprise license required"}`,
			want:   "Retention scheduler is not running",
			call:   func(c *Client) error { _, err := c.TriggerRetentionScheduler(context.Background()); return err },
		},
		{
			name:   "retention role gate",
			status: http.StatusInternalServerError,
			body:   `{"error":"Retention trigger failed: retention: node role is not primary writer"}`,
			want:   "not primary writer",
			call:   func(c *Client) error { _, err := c.TriggerRetentionScheduler(context.Background()); return err },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			err := tc.call(cli)
			var he *HTTPError
			if !errors.As(err, &he) || he.Status != tc.status || !strings.Contains(he.Message, tc.want) {
				t.Errorf("error=%v", err)
			}
		})
	}
}
