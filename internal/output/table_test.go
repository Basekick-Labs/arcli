package output

import (
	"bytes"
	"strings"
	"testing"
)

func TestTable_StripsTerminalControls(t *testing.T) {
	const header = "column\x1b[2J\u202e\u2069"
	const cell = "value\x00\x07\r\x7f\u0085\u009b\u202e\u2069日本語"
	headers := []string{header}
	rows := [][]string{{cell}}
	var out bytes.Buffer
	if err := Table(&out, headers, rows); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(out.String(), "\x00\x07\x1b\r\x7f\u0085\u009b\u202e\u2069") {
		t.Errorf("table contains terminal controls: %q", out.String())
	}
	if !strings.Contains(out.String(), "value日本語") || !strings.Contains(out.String(), "COLUMN") {
		t.Errorf("table lost printable content: %q", out.String())
	}
	if headers[0] != header || rows[0][0] != cell {
		t.Error("rendering modified the caller's headers or rows")
	}
}

func TestTable_PreservesMultilineCells(t *testing.T) {
	var out bytes.Buffer
	if err := Table(&out, []string{"column"}, [][]string{{"first\nsecond"}}); err != nil {
		t.Fatal(err)
	}
	firstLine, secondLine := -1, -1
	for i, line := range strings.Split(out.String(), "\n") {
		if strings.Contains(line, "first") {
			firstLine = i
		}
		if strings.Contains(line, "second") {
			secondLine = i
		}
	}
	if firstLine < 0 || secondLine <= firstLine {
		t.Errorf("multiline cell lost its line break: %q", out.String())
	}
}
