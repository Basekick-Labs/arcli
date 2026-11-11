package client

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
)

func TestJSONToMsgPack_MixedNumericColumnExactIntegers(t *testing.T) {
	for _, tc := range []struct {
		value int64
		exact bool
	}{
		{1 << 53, true},
		{(1 << 53) + 2, true},
		{-(1 << 53) - 2, true},
		{1 << 54, true},
		{math.MaxInt64 - 1023, true},
		{math.MinInt64, true},
		{(1 << 53) + 1, false},
		{-(1 << 53) - 1, false},
		{math.MaxInt64, false},
		{math.MinInt64 + 1, false},
	} {
		for _, floatFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/float-first=%t", tc.value, floatFirst), func(t *testing.T) {
				values := []any{nil, json.Number(strconv.FormatInt(tc.value, 10)), json.Number("1.5")}
				wantValues := []any{nil, float64(tc.value), float64(1.5)}
				if floatFirst {
					values[1], values[2] = values[2], values[1]
					wantValues[1], wantValues[2] = wantValues[2], wantValues[1]
				}
				in, err := json.Marshal(map[string]any{"m": "cpu", "columns": map[string]any{"value": values}})
				if err != nil {
					t.Fatal(err)
				}
				out, sum, err := JSONToMsgPack(bytes.NewReader(in))
				if !tc.exact {
					if err == nil || !strings.Contains(err.Error(), "cannot be represented exactly") || out != nil {
						t.Errorf("inexact integer: output=%x error=%v", out, err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				want := encodeHex(t, map[string]any{"m": "cpu", "columns": map[string]any{"value": wantValues}})
				if got := hex.EncodeToString(out); got != want {
					t.Errorf("MessagePack = %s, want %s", got, want)
				}
				if sum.Items != 1 || sum.Columnar != 1 || sum.Rows != 3 {
					t.Errorf("summary = %+v", sum)
				}
			})
		}
	}
}

func TestJSONToMsgPack_IntegerColumnRetainsInt64(t *testing.T) {
	const in = `{"m":"cpu","columns":{"value":[9007199254740993,9223372036854775807,-9223372036854775808]}}`
	out, _, err := JSONToMsgPack(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := encodeHex(t, map[string]any{
		"m": "cpu", "columns": map[string]any{"value": []any{int64(9007199254740993), int64(math.MaxInt64), int64(math.MinInt64)}},
	})
	if got := hex.EncodeToString(out); got != want {
		t.Errorf("integer column = %s, want %s", got, want)
	}
}
