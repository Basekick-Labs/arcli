package client

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"math/big"
	"strings"
	"testing"
)

func encodeHex(t *testing.T, v any) string {
	t.Helper()
	var buf bytes.Buffer
	if err := mpEncode(&buf, v); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(buf.Bytes())
}

// Byte vectors from the MessagePack spec.
func TestMsgPackEncoder_SpecVectors(t *testing.T) {
	cases := []struct {
		v    any
		want string
	}{
		{nil, "c0"},
		{true, "c3"},
		{false, "c2"},
		{int64(0), "00"},
		{int64(127), "7f"},
		{int64(-1), "ff"},
		{int64(-32), "e0"},
		{int64(-33), "d0df"},
		{int64(128), "d10080"},
		{int64(-129), "d1ff7f"},
		{int64(70000), "d200011170"},
		{int64(1633024800000000), "d30005cd3a371d0800"},
		{float64(1.5), "cb3ff8000000000000"},
		{"", "a0"},
		{"cpu", "a3637075"},
		{strings.Repeat("x", 32), "d920" + strings.Repeat("78", 32)},
		{[]any{}, "90"},
		{[]any{int64(1), "a"}, "9201a161"},
		{map[string]any{}, "80"},
		{map[string]any{"m": "cpu"}, "81a16da3637075"},
		// keys sorted: "a" before "m"
		{map[string]any{"m": "cpu", "a": int64(1)}, "82a16101a16da3637075"},
	}
	for _, c := range cases {
		if got := encodeHex(t, c.v); got != c.want {
			t.Errorf("encode(%v) = %s, want %s", c.v, got, c.want)
		}
	}
	var buf bytes.Buffer
	if err := mpEncode(&buf, []byte("bin")); err == nil {
		t.Error("[]byte must be rejected (the server rejects bin in value columns)")
	}
}

func TestJSONToMsgPack_ColumnarAndSummary(t *testing.T) {
	in := `{"m":"cpu","columns":{"time":[1633024800000000,1633024801000000],"host":["a","b"],"usage":[1.5,2],"ok":[true,null]}}`
	out, sum, err := JSONToMsgPack(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if sum.Items != 1 || sum.Columnar != 1 || sum.Rows != 2 {
		t.Errorf("summary = %+v", sum)
	}
	h := hex.EncodeToString(out)
	// {"columns":{...},"m":"cpu"} with sorted keys; spot-check the measurement and that "usage" holds a float and an int.
	if !strings.HasPrefix(h, "82a7636f6c756d6e73") || !strings.HasSuffix(h, "a16da3637075") {
		t.Errorf("unexpected framing: %s", h)
	}
	// A mixed int/float column is promoted: 1.5 and 2 both encode as float64.
	if !strings.Contains(h, "cb3ff8000000000000cb4000000000000000") {
		t.Errorf("usage column must be promoted to float64: %s", h)
	}
}

func TestJSONToMsgPack_RowBatchAndTags(t *testing.T) {
	in := `{"batch":[{"m":"cpu","t":1633024800000,"h":"srv1","fields":{"usage":95.5},"tags":{"region":"us","rack":12,"prod":true}},{"m":"mem","fields":{"used":1}}]}`
	_, sum, err := JSONToMsgPack(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if sum.Items != 2 || sum.Columnar != 0 || sum.Rows != 2 {
		t.Errorf("summary = %+v", sum)
	}
	// Top-level array of items is accepted too.
	if _, s2, err := JSONToMsgPack(strings.NewReader(`[{"m":"a","fields":{"x":1}},{"m":"b","fields":{"y":2}}]`)); err != nil || s2.Items != 2 {
		t.Errorf("array: err=%v sum=%+v", err, s2)
	}
}

func TestJSONToMsgPack_TrailingWhitespaceSizeLimit(t *testing.T) {
	const document = `{"m":"cpu","fields":{"x":1}}`
	padding := strings.Repeat(" ", JSONDocMaxBytes-len(document))
	for _, tc := range []struct {
		name, suffix string
		wantError    bool
	}{
		{"at limit", "", false},
		{"one byte over", " ", true},
		{"trailing document beyond limit", " {}", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := io.MultiReader(strings.NewReader(document), strings.NewReader(padding), strings.NewReader(tc.suffix))
			out, sum, err := JSONToMsgPack(in)
			if tc.wantError {
				if err == nil || !strings.Contains(err.Error(), "JSON document exceeds") {
					t.Fatalf("error = %v, want size-limit error", err)
				}
				if out != nil || sum != (MsgPackDocSummary{}) {
					t.Errorf("oversize input produced output: %x, summary: %+v", out, sum)
				}
				return
			}
			if err != nil || len(out) == 0 || sum.Items != 1 || sum.Rows != 1 {
				t.Errorf("input at limit: output=%x summary=%+v error=%v", out, sum, err)
			}
		})
	}
}

func TestJSONToMsgPack_Rejections(t *testing.T) {
	cases := map[string]string{
		`{"columns":{"x":[1]}}`:                                        `"m" (measurement) must be a non-empty string`,
		`{"m":5,"columns":{"x":[1]}}`:                                  `"m" (measurement) must be a non-empty string`,
		`{"m":"1bad","columns":{"x":[1]}}`:                             "invalid measurement name",
		`{"m":"cpu","columns":{}}`:                                     "non-empty object",
		`{"m":"cpu","columns":{"a":[1,2],"b":[1]}}`:                    "all columns must be the same length",
		`{"m":"cpu","columns":{"a":[]}}`:                               "columns are empty",
		`{"m":"cpu","columns":{"a":1}}`:                                "must be an array",
		`{"m":"cpu","columns":{"time":["2026-01-01"]}}`:                "integer epochs",
		`{"m":"cpu","columns":{"time":[1.5]}}`:                         "not an integer epoch",
		`{"m":"cpu","columns":{"time":[null]}}`:                        "must not be null",
		`{"m":"cpu","columns":{"time":[1700000000,1700000000000000]}}`: "not the same unit",
		`{"m":"cpu","columns":{"a":[1,"x"]}}`:                          "mixes",
		`{"m":"cpu","columns":{"a":[1.5,true]}}`:                       "mixes",
		`{"m":"cpu","columns":{"a":[{"n":1}]}}`:                        "nested objects and arrays",
		`{"m":"cpu","columns":{"a":[9223372036854775808]}}`:            "out of int64 range",
		`{"m":"cpu","columns":{"a":[1]},"extra":1}`:                    `unexpected key "extra"`,
		`{"m":"cpu"}`:                                       `needs a non-empty "fields" object`,
		`{"m":"cpu","fields":{}}`:                           `needs a non-empty "fields" object`,
		`{"m":"cpu","fields":{"x":1},"t":"now"}`:            `"t" must be an integer epoch`,
		`{"m":"cpu","fields":{"x":1},"h":7}`:                `"h" (host) must be a non-empty string`,
		`{"m":"cpu","fields":{"x":1},"tags":{"a":null}}`:    "must be a string, number, or boolean",
		`{"m":"cpu","fields":{"x":1},"tags":{"a":{"b":1}}}`: "must be a string, number, or boolean",
		`{"m":"cpu","f":[1]}`:                               `needs a non-empty "fields" object`,
		`{"m":"cpu","fields":{"x":1},"f":[1]}`:              `the compact "f" array is not supported`,
		`{"batch":[]}`:                                      "is empty",
		`{"batch":[1]}`:                                     "not an object",
		`[]`:                                                "top-level array is empty",
		`[1]`:                                               "not an object",
		`null`:                                              "must be an object or an array",
		`42`:                                                "must be an object or an array",
		`{"m":"cpu","fields":{"x":1}} trailing`:             "trailing content",
		`{"m":"cpu","fields":{"x":1}`:                       "invalid JSON",
	}
	for in, want := range cases {
		_, _, err := JSONToMsgPack(strings.NewReader(in))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", in, err, want)
		}
	}
	// An oversize document is reported as such, not as a truncated read.
	big := `{"m":"cpu","columns":{"x":[` + strings.Repeat("1,", JSONDocMaxBytes/2) + `1]}}`
	if _, _, err := JSONToMsgPack(strings.NewReader(big)); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("oversize: err = %v", err)
	}
	// Integral floats in time are converted to int64 (1e15 → us bucket).
	if _, _, err := JSONToMsgPack(strings.NewReader(`{"m":"cpu","columns":{"time":[1e15,1000000000000001]}}`)); err != nil {
		t.Errorf("integral float time should be accepted: %v", err)
	}
}

func TestMsgPackFirstByteOK(t *testing.T) {
	for _, ok := range []byte{0x80, 0x8f, 0x90, 0x9f, 0xdc, 0xdd, 0xde, 0xdf, 0x1f, 0x28} {
		if !MsgPackFirstByteOK(ok) {
			t.Errorf("0x%02x should be accepted", ok)
		}
	}
	for _, bad := range []byte{'c', '{', '[', ' ', '\n', 0x00, 0xa3, 0xc0, 0x7f} {
		if MsgPackFirstByteOK(bad) {
			t.Errorf("0x%02x should be rejected", bad)
		}
	}
}

func TestJSONToMsgPack_ExactTimestamps(t *testing.T) {
	for _, tc := range []struct {
		number  string
		want    int64
		invalid bool
	}{
		{"1700000000000000001.0", 1700000000000000001, false},
		{"1.700000000000000001e18", 1700000000000000001, false},
		{"17000000000000000010e-1", 1700000000000000001, false},
		{"9007199254740993.0", 9007199254740993, false},
		{"9223372036854775807.0", math.MaxInt64, false},
		{"9.223372036854775807e18", math.MaxInt64, false},
		{"-9223372036854775808.0", math.MinInt64, false},
		{"-9.223372036854775808E+18", math.MinInt64, false},
		{"0.001e3", 1, false},
		{"1.00e2", 100, false},
		{"-0.0e99999999999999999999", 0, false},
		{"1" + strings.Repeat("0", 1000) + "e-1000", 1, false},
		{"1700000000000000001.5", 0, true},
		{"1700000000000000001.0000000000000001", 0, true},
		{"9223372036854775808.0", 0, true},
		{"-9223372036854775809.0", 0, true},
		{"1e-400", 0, true},
		{"1e-1000000000", 0, true},
		{"1e1000000000", 0, true},
		{"1e-99999999999999999999", 0, true},
	} {
		for _, shape := range []string{"row", "columnar"} {
			t.Run(fmt.Sprintf("%s/%s", shape, tc.number), func(t *testing.T) {
				in := fmt.Sprintf(`{"m":"cpu","t":%s,"fields":{"x":1}}`, tc.number)
				want := map[string]any{"m": "cpu", "t": tc.want, "fields": map[string]any{"x": int64(1)}}
				if shape == "columnar" {
					in = fmt.Sprintf(`{"m":"cpu","columns":{"time":[%s]}}`, tc.number)
					want = map[string]any{"m": "cpu", "columns": map[string]any{"time": []any{tc.want}}}
				}
				out, sum, err := JSONToMsgPack(strings.NewReader(in))
				if tc.invalid {
					if err == nil || out != nil {
						t.Errorf("non-integral or out-of-range timestamp accepted: output=%x error=%v", out, err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if got, expected := hex.EncodeToString(out), encodeHex(t, want); got != expected {
					t.Errorf("MessagePack = %s, want %s (timestamp %d)", got, expected, tc.want)
				}
				if sum.Items != 1 || sum.Rows != 1 {
					t.Errorf("summary = %+v", sum)
				}
			})
		}
	}
}

func TestParseIntegerEpoch_DecimalScales(t *testing.T) {
	for _, mantissa := range []string{
		"0", "-0", "1", "-1", "1.5", "0.0001", "1700000000000000001",
		"9223372036854775808", "-9223372036854775808", "100000000000000000000",
	} {
		for exponent := -25; exponent <= 25; exponent++ {
			number := fmt.Sprintf("%se%d", mantissa, exponent)
			exact, ok := new(big.Rat).SetString(number)
			if !ok {
				t.Fatalf("invalid test number %q", number)
			}
			wantOK := exact.IsInt() && exact.Num().IsInt64()
			got, gotOK := parseIntegerEpoch(number)
			if gotOK != wantOK || (wantOK && got != exact.Num().Int64()) {
				t.Errorf("parseIntegerEpoch(%q) = (%d, %t), exact value = %s, fits int64 = %t", number, got, gotOK, exact, wantOK)
			}
		}
	}
}
