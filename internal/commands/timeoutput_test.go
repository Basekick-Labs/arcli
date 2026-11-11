package commands

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// timeOutputCommands is the exact set of commands that render timestamps in
// table, CSV or text and therefore take --time-output.
var timeOutputCommands = []string{
	"arcli audit log",
	"arcli audit stats",
	"arcli auth token list",
	"arcli auth token show",
	"arcli auth whoami",
	"arcli backup create",
	"arcli backup delete",
	"arcli backup list",
	"arcli backup restore",
	"arcli backup show",
	"arcli backup status",
	"arcli cluster files",
	"arcli cluster node show",
	"arcli cluster nodes",
	"arcli cluster status",
	"arcli compaction status",
	"arcli cq create",
	"arcli cq execute",
	"arcli cq executions",
	"arcli cq list",
	"arcli cq show",
	"arcli cq update",
	"arcli license status",
	"arcli logs",
	"arcli retention create",
	"arcli retention execute",
	"arcli retention executions",
	"arcli retention list",
	"arcli retention show",
	"arcli retention update",
	"arcli scheduler status",
	"arcli tiering file",
	"arcli tiering policy list",
	"arcli tiering policy set",
	"arcli tiering policy show",
	"arcli tiering stats",
	"arcli tiering status",
}

func TestTimeOutputFlagTreeWalk(t *testing.T) {
	var got []string
	walkCommands(NewRoot(BuildInfo{Version: "test"}), func(c *cobra.Command) {
		if f := c.Flags().Lookup("time-output"); f != nil {
			got = append(got, c.CommandPath())
			if f.DefValue != "rfc3339" {
				t.Errorf("%s: default %q", c.CommandPath(), f.DefValue)
			}
		}
		if c.PersistentFlags().Lookup("time-output") != nil {
			t.Errorf("%s: --time-output must not be persistent", c.CommandPath())
		}
	})
	sort.Strings(got)
	if strings.Join(got, "\n") != strings.Join(timeOutputCommands, "\n") {
		t.Errorf("commands with --time-output:\n got %q\nwant %q", got, timeOutputCommands)
	}
}

func TestTimeOutputSet(t *testing.T) {
	for in, nano := range map[string]bool{"rfc3339": false, "RFC3339": false, "Rfc3339": false, "rfc3339nano": true, "RFC3339Nano": true, "RFC3339NANO": true} {
		to := timeOutput{nano: !nano}
		if err := to.Set(in); err != nil || to.nano != nano {
			t.Errorf("Set(%q): err=%v nano=%t", in, err, to.nano)
		}
	}
	for _, in := range []string{"", "bogus", "rfc3339 ", "rfc", "unix"} {
		var to timeOutput
		err := to.Set(in)
		if err == nil || err.Error() != "must be rfc3339 or rfc3339nano" {
			t.Errorf("Set(%q) err = %v", in, err)
		}
	}
	var to timeOutput
	if to.String() != "rfc3339" || to.Type() != "string" {
		t.Errorf("zero value: %q %q", to.String(), to.Type())
	}
}

func TestTimeOutputFormatting(t *testing.T) {
	def, nano := timeOutput{}, timeOutput{nano: true}
	frac := "2026-09-07T16:00:00.123456789-03:00"
	for _, c := range []struct{ got, want string }{
		{def.Server(frac, "-"), "2026-09-07T19:00:00Z"},
		{nano.Server(frac, "-"), "2026-09-07T19:00:00.123456789Z"},
		{nano.Server("2026-09-07T19:00:00.500Z", "-"), "2026-09-07T19:00:00.5Z"},
		{nano.Server("2026-09-07T19:00:00Z", "-"), "2026-09-07T19:00:00Z"},
		{nano.Server("", "never"), "never"},
		{nano.Server("0001-01-01T00:00:00Z", "never"), "never"},
		{nano.Server("not\x1b[31ma time", "-"), "not[31ma time"},
		{nano.ServerPtr(nil, "none"), "none"},
		{nano.Val(time.Time{}, "-"), "-"},
		{nano.Ptr(nil, "never"), "never"},
		{nano.Ptr(&time.Time{}, "never"), "never"},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
	if got := def.Server("not\x1b[31ma time", "-"); got != clean("not\x1b[31ma time") {
		t.Errorf("default scrub: %q", got)
	}
}

// An explicit --time-output rfc3339 (any case) is the default: byte-identical
// to the goldens main produced. JSON output ignores the flag entirely.
func TestTimeOutputExplicitDefaultAndJSONMatchGolden(t *testing.T) {
	for _, tc := range timeOutputGoldenCases() {
		want, err := os.ReadFile(filepath.Join("testdata", "timeoutput", tc.name+".golden"))
		if err != nil {
			t.Fatal(err)
		}
		t.Run(tc.name+"/rfc3339", func(t *testing.T) {
			if got, _ := runGoldenCase(t, tc, "--time-output", "Rfc3339"); got != string(want) {
				t.Errorf("--time-output Rfc3339 differs from default\n--- want\n%s\n--- got\n%s", want, got)
			}
		})
		if strings.HasSuffix(tc.name, "_json") {
			t.Run(tc.name+"/nano", func(t *testing.T) {
				if got, _ := runGoldenCase(t, tc, "--time-output", "RFC3339Nano"); got != string(want) {
					t.Errorf("-o json changed with --time-output\n--- want\n%s\n--- got\n%s", want, got)
				}
			})
		}
	}
}

// An invalid value fails at flag parsing on every command that has the
// flag (with -o json too), before any request reaches the server.
func TestTimeOutputInvalidMakesNoRequest(t *testing.T) {
	for _, tc := range timeOutputGoldenCases() {
		t.Run(tc.name, func(t *testing.T) {
			got, g := runGoldenCase(t, tc, "--time-output", "bogus")
			if !strings.Contains(got, `invalid argument "bogus" for "--time-output" flag: must be rfc3339 or rfc3339nano`) {
				t.Errorf("output = %s", got)
			}
			if n := g.requests(); n != 0 {
				t.Errorf("%d requests reached the server", n)
			}
			if strings.Contains(got, "[y/N]") {
				t.Errorf("prompted before rejecting the flag: %s", got)
			}
		})
	}
}

func goldenCaseByName(t *testing.T, name string) goldenCase {
	t.Helper()
	for _, tc := range timeOutputGoldenCases() {
		if tc.name == name {
			return tc
		}
	}
	t.Fatalf("no golden case %q", name)
	return goldenCase{}
}

func TestTimeOutputNano(t *testing.T) {
	for _, c := range []struct {
		name    string
		want    []string
		notWant []string
	}{
		// String-sourced (server RFC3339 strings with an offset).
		{"retention_show", []string{
			"last run:       2026-09-07T19:00:00.123456789Z\n",
			"created:        2026-09-07T19:05:00.5Z\n",
			"updated:        2026-09-07T19:00:00.123456789Z\n",
		}, nil},
		{"retention_execute_prompt", []string{"older than 2026-09-07T19:00:00.123456789Z from database"}, nil},
		{"scheduler_status", []string{"next run 2026-09-07T19:00:00.123456789Z"}, nil},
		{"cq_execute", []string{"window:       2026-09-07T19:00:00.123456789Z → 2026-09-07T19:05:00.5Z\n"}, nil},
		// time.Time-sourced.
		{"auth_token_show", []string{
			"expires:     2027-01-01T06:00:00.123Z\n",
			"last used:   2026-09-07T23:02:03.000001Z\n",
			"created:     2026-09-07T19:00:01.987654321Z\n",
		}, nil},
		{"backup_delete_prompt", []string{"Delete backup backup-20260907-190000-abcdef12 (2026-09-07T19:00:01.987654321Z, 2 files"}, nil},
		{"cluster_node_show", []string{"joined:         2026-09-07T23:02:03.000001Z\n"}, nil},
		{"compaction_status", []string{"2026-09-07T19:00:01.987654321Z"}, nil},
		// CSV.
		{"cq_list_csv", []string{",2026-09-07T19:00:00.123456789Z,completed,5\n"}, nil},
		{"logs_csv", []string{"2026-09-07T19:00:01.987654321Z,info,api,hello,x.go:1\n", "2026-09-07T23:02:03.000001Z,warn,"}, nil},
		// Values that are not timestamps keep their none/scrub rendering.
		{"retention_list", []string{"│ never "}, nil},
		{"cluster_nodes_csv", []string{"n2,node-2,reader,unknown,10.0.0.2:9000,10.0.0.2:8000,26.09.3,,3\n"}, nil},
		// The cq --end warning is fixed RFC3339 whatever --time-output says.
		{"cq_execute_end_earlier", []string{"warning: --end 2026-09-07T18:00:00Z is earlier than the query's last processed time 2026-09-07T19:00:00Z;"}, []string{"19:00:00.75"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, _ := runGoldenCase(t, goldenCaseByName(t, c.name), "--time-output", "rfc3339nano")
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q in\n%s", w, got)
				}
			}
			for _, w := range c.notWant {
				if strings.Contains(got, w) {
					t.Errorf("unexpected %q in\n%s", w, got)
				}
			}
		})
	}
}

func TestTimeOutputLicenseStatus(t *testing.T) {
	block := `{"tier":"enterprise","status":"active","source":"file","expires_at":"2027-01-01T03:00:00.123-03:00"}`
	for _, c := range []struct {
		args       []string
		want, gone string
	}{
		{nil, "│ 2027-01-01T06:00:00Z ", ".123"},
		{[]string{"--time-output", "RFC3339Nano"}, "│ 2027-01-01T06:00:00.123Z ", ""},
	} {
		g := newGoldenServer(t, map[string][]string{"GET /health": {`{"status":"ok","time":"t","uptime":"1s","license":` + block + `}`}})
		writeTestConfig(t, g.srv.URL, "tok")
		out, _, err := execCmd(t, NewRoot(BuildInfo{Version: "test"}), append([]string{"license", "status"}, c.args...)...)
		if err != nil || !strings.Contains(out, c.want) || (c.gone != "" && strings.Contains(out, c.gone)) {
			t.Errorf("%v: err=%v out=%s", c.args, err, out)
		}
	}
}

// cq execute compares the --end value actually sent (fractional seconds
// dropped) with last_processed_time.
func TestCQExecuteEndComparesSentValue(t *testing.T) {
	const warnPrefix = "warning: --end "
	cq := func(lp string) map[string][]string {
		return map[string][]string{"GET /api/v1/continuous_queries/1": {strings.Replace(goldenCQ1, `"last_processed_time":"2026-09-07T19:00:00.75Z"`, `"last_processed_time":"`+lp+`"`, 1)}}
	}
	for _, c := range []struct {
		name, lp, end string
		warn          string // "" = no warning
	}{
		// New: fractional --end later than last processed within the same
		// second; 19:00:00 is sent, which IS earlier. Main stayed silent.
		{"fractional end same second", "2026-09-07T19:00:00.4Z", "2026-09-07T19:00:00.5Z",
			"warning: --end 2026-09-07T19:00:00.5Z is sent as 2026-09-07T19:00:00Z (whole seconds), which is earlier than the query's last processed time 2026-09-07T19:00:00.4Z; the server will rewind the watermark and the next run will reprocess everything since\n"},
		{"fractional end, offset input", "2026-09-07T19:00:00.4Z", "2026-09-07T16:00:00.9-03:00",
			"warning: --end 2026-09-07T19:00:00.9Z is sent as 2026-09-07T19:00:00Z (whole seconds), which is earlier than the query's last processed time 2026-09-07T19:00:00.4Z; the server will rewind the watermark and the next run will reprocess everything since\n"},
		// Fractional input that main already warned about keeps main's text.
		{"fractional end earlier", "2026-09-07T19:00:00Z", "2026-09-07T18:59:58.5Z",
			"warning: --end 2026-09-07T18:59:58Z is earlier than the query's last processed time 2026-09-07T19:00:00Z; the server will rewind the watermark and the next run will reprocess everything since\n"},
		// Unchanged: fractional end whose truncation equals last processed.
		{"fractional end equals truncated", "2026-09-07T19:00:00Z", "2026-09-07T19:00:00.5Z", ""},
		{"fractional end next second", "2026-09-07T19:00:00.4Z", "2026-09-07T19:00:01.1Z", ""},
		// Unchanged whole-second cases (same text as main).
		{"whole end earlier", "2026-09-07T19:00:00Z", "2026-09-07T18:59:59Z",
			"warning: --end 2026-09-07T18:59:59Z is earlier than the query's last processed time 2026-09-07T19:00:00Z; the server will rewind the watermark and the next run will reprocess everything since\n"},
		{"whole end equal", "2026-09-07T19:00:00Z", "2026-09-07T19:00:00Z", ""},
		{"whole end later", "2026-09-07T19:00:00Z", "2026-09-07T19:00:01Z", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, extra := range [][]string{nil, {"--time-output", "rfc3339nano"}} {
				tc := goldenCase{name: c.name, args: []string{"cq", "execute", "1", "--start", "2026-09-07T17:00:00Z", "--end", c.end}, overrides: cq(c.lp)}
				got, _ := runGoldenCase(t, tc, extra...)
				stderr := got[strings.Index(got, "--- stderr\n")+len("--- stderr\n") : strings.Index(got, "--- err\n")]
				if c.warn == "" && strings.Contains(stderr, warnPrefix) {
					t.Errorf("%v: unexpected warning %q", extra, stderr)
				}
				if c.warn != "" && stderr != c.warn {
					t.Errorf("%v: stderr = %q, want %q", extra, stderr, c.warn)
				}
			}
		})
	}
}

func TestTimeOutputCompletion(t *testing.T) {
	writeTestConfig(t, "http://a", "tok")
	out := completeArgs(t, "retention", "list", "--time-output", "")
	if !strings.Contains(out, "rfc3339\n") || !strings.Contains(out, "rfc3339nano\n") || !strings.HasSuffix(out, ":4\n") {
		t.Errorf("completion = %q", out)
	}
}
