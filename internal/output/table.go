// Package output renders command output in operator-friendly forms.
//
// PR1 only needs a table renderer good enough for `arcli config list`
// (a 5-column connection table). Later PRs add JSON / CSV / Arrow
// renderers that share the same column abstraction.
package output

import (
	"io"
	"strings"

	"github.com/olekukonko/tablewriter"
)

// Table writes a simple bordered table of `rows` under the given
// `headers` to `w`. Terminal controls and bidi overrides are removed,
// preserving newlines and tabs for layout. Input slices are not modified.
// Returns any I/O error from the writer.
func Table(w io.Writer, headers []string, rows [][]string) error {
	t := tablewriter.NewWriter(w)
	t.Header(sanitizeTableRow(headers))
	for _, row := range rows {
		if err := t.Append(sanitizeTableRow(row)); err != nil {
			return err
		}
	}
	return t.Render()
}

func sanitizeTableRow(row []string) []string {
	if row == nil {
		return nil
	}
	cleaned := make([]string, len(row))
	for i, cell := range row {
		cleaned[i] = strings.Map(func(r rune) rune {
			switch {
			case r < 0x20 && r != '\n' && r != '\t', r == 0x7f, r >= 0x80 && r <= 0x9f:
				return -1
			case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
				return -1
			}
			return r
		}, cell)
	}
	return cleaned
}
