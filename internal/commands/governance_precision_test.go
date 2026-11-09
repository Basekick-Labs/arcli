package commands

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/basekick-labs/arcli/internal/client"
)

func TestGovernanceTimestampOutput(t *testing.T) {
	const original = "2026-10-27T12:34:56.123456789+02:30"
	p := client.GovernancePolicy{TokenID: 1, CreatedAt: original, UpdatedAt: original}
	u := client.GovernanceUsage{TokenID: 1, HourResetAt: original, DayResetAt: original}
	for _, mode := range []struct {
		name string
		to   timeOutput
		want string
	}{
		{"default", timeOutput{}, "2026-10-27T10:04:56Z"},
		{"rfc3339nano", timeOutput{nano: true}, "2026-10-27T10:04:56.123456789Z"},
	} {
		for _, tc := range []struct {
			name   string
			render func(*bytes.Buffer)
		}{
			{"policy table", func(b *bytes.Buffer) { writeGovernancePolicyInfo(b, &p, mode.to) }},
			{"policy csv", func(b *bytes.Buffer) {
				if err := renderGovernancePolicyList(b, []client.GovernancePolicy{p}, false, true, mode.to); err != nil {
					t.Fatal(err)
				}
			}},
			{"usage table", func(b *bytes.Buffer) { writeGovernanceUsageInfo(b, &u, &p, mode.to) }},
		} {
			t.Run(mode.name+"/"+tc.name, func(t *testing.T) {
				var b bytes.Buffer
				tc.render(&b)
				if strings.Count(b.String(), mode.want) != 2 {
					t.Fatalf("want %s twice in: %s", mode.want, b.String())
				}
				if !mode.to.nano && strings.Contains(b.String(), ".123") {
					t.Fatalf("default output kept fractional seconds: %s", b.String())
				}
			})
		}
	}
	for _, obj := range []any{p, u} {
		var b bytes.Buffer
		if err := writeJSON(&b, obj); err != nil {
			t.Fatal(err)
		}
		if !json.Valid(b.Bytes()) || strings.Count(b.String(), original) != 2 {
			t.Fatalf("JSON timestamp changed: %s", b.String())
		}
	}
}

// End-to-end through the command: the flag is parsed, defaults to whole
// seconds, and an unset timestamp is empty in CSV.
func TestGovernancePolicyListTimeOutputFlag(t *testing.T) {
	governanceCommandServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"success":true,"policies":[{"id":1,"token_id":12,"token_name":"batch","created_at":"2026-10-27T10:00:00.5Z","updated_at":""}],"count":1}`))
	})
	out, _, err := execCmd(t, newGovernancePolicyListCmd(), "-o", "csv")
	if err != nil || !strings.Contains(out, ",2026-10-27T10:00:00Z,\n") {
		t.Fatalf("default: err=%v out=%q", err, out)
	}
	out, _, err = execCmd(t, newGovernancePolicyListCmd(), "-o", "csv", "--time-output", "rfc3339nano")
	if err != nil || !strings.Contains(out, ",2026-10-27T10:00:00.5Z,\n") {
		t.Fatalf("nano: err=%v out=%q", err, out)
	}
	if _, _, err := execCmd(t, newGovernancePolicyListCmd(), "--time-output", "bogus"); err == nil {
		t.Fatal("bogus --time-output accepted")
	}
}
