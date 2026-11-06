package commands

import (
	"bytes"
	"encoding/json"
	"github.com/basekick-labs/arcli/internal/client"
	"strings"
	"testing"
)

func TestGovernanceTimestampPrecision(t *testing.T) {
	const original = "2026-10-27T12:34:56.123456789+02:30"
	const utc = "2026-10-27T10:04:56.123456789Z"
	p := client.GovernancePolicy{TokenID: 1, CreatedAt: original, UpdatedAt: original}
	u := client.GovernanceUsage{TokenID: 1, HourResetAt: original, DayResetAt: original}
	for _, tc := range []struct {
		name   string
		render func(*bytes.Buffer)
	}{
		{"policy table", func(b *bytes.Buffer) { writeGovernancePolicyInfo(b, &p) }},
		{"policy csv", func(b *bytes.Buffer) {
			if err := renderGovernancePolicyList(b, []client.GovernancePolicy{p}, false, true); err != nil {
				t.Fatal(err)
			}
		}},
		{"usage table", func(b *bytes.Buffer) { writeGovernanceUsageInfo(b, &u, &p) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			tc.render(&b)
			if strings.Count(b.String(), utc) != 2 {
				t.Fatalf("timestamp precision/UTC lost: %s", b.String())
			}
		})
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
