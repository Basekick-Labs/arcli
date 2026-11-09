package commands

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// timeOutput is the value of --time-output: how timestamps are displayed
// in table, CSV and human-readable text. The zero value is rfc3339 (UTC,
// whole seconds), which is what every command printed before the flag
// existed; rfc3339nano keeps the fractional seconds the server sent.
// JSON output never goes through it.
//
// It is a per-command value, not package state: a renderer that shows a
// timestamp takes a timeOutput parameter, and the old free helpers are
// gone, so a renderer cannot format a timestamp without one. Registering
// the flag (to.add) on each command is enforced by
// TestTimeOutputFlagTreeWalk, not by the compiler.
type timeOutput struct{ nano bool }

const (
	timeOutputRFC3339     = "rfc3339"
	timeOutputRFC3339Nano = "rfc3339nano"
)

// Set implements pflag.Value. Values are case-insensitive.
func (t *timeOutput) Set(s string) error {
	switch strings.ToLower(s) {
	case timeOutputRFC3339:
		t.nano = false
	case timeOutputRFC3339Nano:
		t.nano = true
	default:
		return fmt.Errorf("must be %s or %s", timeOutputRFC3339, timeOutputRFC3339Nano)
	}
	return nil
}

// String implements pflag.Value.
func (t *timeOutput) String() string {
	if t.nano {
		return timeOutputRFC3339Nano
	}
	return timeOutputRFC3339
}

// Type implements pflag.Value.
func (t *timeOutput) Type() string { return "string" }

// add registers --time-output on c. The usage's `a|b` token is what shell
// completion offers (see enumFromUsage).
func (t *timeOutput) add(c *cobra.Command) {
	c.Flags().Var(t, "time-output", "timestamp display in table/CSV/text: rfc3339|rfc3339nano (JSON unaffected)")
}

func (t timeOutput) layout() string {
	if t.nano {
		return time.RFC3339Nano
	}
	return time.RFC3339
}

// Time renders t in UTC with the chosen layout.
func (t timeOutput) Time(v time.Time) string {
	return v.UTC().Format(t.layout())
}

// Val renders a time.Time, or none for the zero value (Arc serialises
// unset times as 0001-01-01T00:00:00Z).
func (t timeOutput) Val(v time.Time, none string) string {
	if v.IsZero() {
		return none
	}
	return t.Time(v)
}

// Ptr renders a nullable time.Time, or none when nil or zero.
func (t timeOutput) Ptr(v *time.Time, none string) string {
	if v == nil || v.IsZero() {
		return none
	}
	return t.Time(*v)
}

// Server renders one of Arc's RFC3339 timestamp strings in UTC. Empty or
// zero values (the SQLite driver emits 0001-01-01T00:00:00Z when it cannot
// parse a column) render as none; a string that is not a timestamp at all
// is shown as-is (scrubbed).
func (t timeOutput) Server(s, none string) string {
	if s == "" {
		return none
	}
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return clean(s)
	}
	if v.IsZero() {
		return none
	}
	return t.Time(v)
}

// ServerPtr is Server for a nullable string.
func (t timeOutput) ServerPtr(p *string, none string) string {
	if p == nil {
		return none
	}
	return t.Server(*p, none)
}
