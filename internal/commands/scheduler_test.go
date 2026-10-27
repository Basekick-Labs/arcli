package commands

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestSchedulerCQReload_TableAndJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/schedulers/cq/reload" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"message":"CQ scheduler reloaded successfully","job_count":3,"extra":"kept"}`))
	}))
	defer srv.Close()
	writeTestConfig(t, srv.URL, "tok")
	out, _, err := execCmd(t, newSchedulerCQReloadCmd())
	if err != nil || !strings.Contains(out, "CQ scheduler reloaded successfully") || !strings.Contains(out, "scheduled jobs: 3") {
		t.Errorf("table=%q err=%v", out, err)
	}
	out, _, err = execCmd(t, newSchedulerCQReloadCmd(), "-o", "json")
	if err != nil || !strings.Contains(out, `"extra": "kept"`) || !strings.Contains(out, `"job_count": 3`) {
		t.Errorf("json=%q err=%v", out, err)
	}
	if _, _, err := execCmd(t, newSchedulerCQReloadCmd(), "-o", "csv"); err == nil {
		t.Error("unsupported output format should fail")
	}
}

func TestSchedulerRetentionTrigger_ConfirmationAndOutputs(t *testing.T) {
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/schedulers/retention/trigger" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		posts++
		_, _ = w.Write([]byte(`{"message":"Retention triggered successfully","extra":"kept"}`))
	}))
	defer srv.Close()
	writeTestConfig(t, srv.URL, "tok")

	cmd := newSchedulerRetentionTriggerCmd()
	cmd.SetIn(strings.NewReader("yes\n"))
	out, stderr, err := execCmd(t, cmd)
	if err != nil || posts != 1 || !strings.Contains(stderr, "permanently delete data") || !strings.Contains(out, "Retention triggered successfully") {
		t.Errorf("table=%q stderr=%q posts=%d err=%v", out, stderr, posts, err)
	}

	cmd = newSchedulerRetentionTriggerCmd()
	cmd.SetIn(strings.NewReader("no\n"))
	out, _, err = execCmd(t, cmd)
	if err != errAborted || posts != 1 || out != "" {
		t.Errorf("declined trigger: out=%q posts=%d err=%v", out, posts, err)
	}

	out, stderr, err = execCmd(t, newSchedulerRetentionTriggerCmd(), "--yes", "-o", "json")
	if err != nil || posts != 2 || stderr != "" || !strings.Contains(out, `"extra": "kept"`) {
		t.Errorf("json=%q stderr=%q posts=%d err=%v", out, stderr, posts, err)
	}
}

func TestSchedulerActionErrorsStayVisibleOnStderr(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		args   []string
		newCmd func() *cobra.Command
		want   string
	}{
		{
			name:   "admin permission",
			status: http.StatusForbidden,
			body:   `{"error":"Permission denied: admin permission required"}`,
			newCmd: newSchedulerCQReloadCmd,
			want:   "HTTP 403",
		},
		{
			name:   "disabled scheduler",
			status: http.StatusServiceUnavailable,
			body:   `{"error":"Retention scheduler is not running - enterprise license required"}`,
			args:   []string{"--yes"},
			newCmd: newSchedulerRetentionTriggerCmd,
			want:   "enterprise license required",
		},
		{
			name:   "role gate",
			status: http.StatusInternalServerError,
			body:   `{"error":"Retention trigger failed: retention: node role is not primary writer"}`,
			args:   []string{"--yes"},
			newCmd: newSchedulerRetentionTriggerCmd,
			want:   "not primary writer",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			writeTestConfig(t, srv.URL, "tok")
			out, _, err := execCmd(t, tc.newCmd(), tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) || out != "" {
				t.Errorf("out=%q err=%v", out, err)
			}
		})
	}
}
