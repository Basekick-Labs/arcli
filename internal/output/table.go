// Package output renders command output in operator-friendly forms.
//
// PR1 only needs a table renderer good enough for `arcli config list`
// (a 5-column connection table). Later PRs add JSON / CSV / Arrow
// renderers that share the same column abstraction.
package output

import (
	"io"

	"github.com/olekukonko/tablewriter"
)

// Table writes a simple bordered table of `rows` under the given
// `headers` to `w`. Returns any I/O error from the writer.
func Table(w io.Writer, headers []string, rows [][]string) error {
	t := tablewriter.NewWriter(w)
	t.Header(headers)
	for _, row := range rows {
		if err := t.Append(row); err != nil {
			return err
		}
	}
	return t.Render()
}
