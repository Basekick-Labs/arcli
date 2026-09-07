package client

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
)

// A minimal MessagePack encoder for the value shapes Arc's write
// endpoint accepts: nil, bool, int64, float64, string, []any, and
// map[string]any. Strings are always encoded as msgpack str (never
// bin, which the server rejects in value columns). Map keys are sorted
// so output is deterministic and byte-testable. Hand-rolled to keep the
// dependency surface of a convenience format at zero.

func mpEncode(buf *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		buf.WriteByte(0xc0)
	case bool:
		if x {
			buf.WriteByte(0xc3)
		} else {
			buf.WriteByte(0xc2)
		}
	case int64:
		mpEncodeInt(buf, x)
	case float64:
		buf.WriteByte(0xcb)
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], math.Float64bits(x))
		buf.Write(b[:])
	case string:
		mpEncodeStr(buf, x)
	case []any:
		n := len(x)
		switch {
		case n < 16:
			buf.WriteByte(0x90 | byte(n))
		case n <= math.MaxUint16:
			buf.WriteByte(0xdc)
			_ = binary.Write(buf, binary.BigEndian, uint16(n))
		default:
			buf.WriteByte(0xdd)
			_ = binary.Write(buf, binary.BigEndian, uint32(n))
		}
		for _, e := range x {
			if err := mpEncode(buf, e); err != nil {
				return err
			}
		}
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		n := len(keys)
		switch {
		case n < 16:
			buf.WriteByte(0x80 | byte(n))
		case n <= math.MaxUint16:
			buf.WriteByte(0xde)
			_ = binary.Write(buf, binary.BigEndian, uint16(n))
		default:
			buf.WriteByte(0xdf)
			_ = binary.Write(buf, binary.BigEndian, uint32(n))
		}
		for _, k := range keys {
			mpEncodeStr(buf, k)
			if err := mpEncode(buf, x[k]); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("msgpack: unsupported value type %T", v)
	}
	return nil
}

func mpEncodeInt(buf *bytes.Buffer, x int64) {
	switch {
	case x >= 0 && x < 128:
		buf.WriteByte(byte(x))
	case x < 0 && x >= -32:
		buf.WriteByte(byte(x))
	case x >= math.MinInt8 && x <= math.MaxInt8:
		buf.WriteByte(0xd0)
		buf.WriteByte(byte(int8(x)))
	case x >= math.MinInt16 && x <= math.MaxInt16:
		buf.WriteByte(0xd1)
		_ = binary.Write(buf, binary.BigEndian, int16(x))
	case x >= math.MinInt32 && x <= math.MaxInt32:
		buf.WriteByte(0xd2)
		_ = binary.Write(buf, binary.BigEndian, int32(x))
	default:
		buf.WriteByte(0xd3)
		_ = binary.Write(buf, binary.BigEndian, x)
	}
}

func mpEncodeStr(buf *bytes.Buffer, s string) {
	n := len(s)
	switch {
	case n < 32:
		buf.WriteByte(0xa0 | byte(n))
	case n <= math.MaxUint8:
		buf.WriteByte(0xd9)
		buf.WriteByte(byte(n))
	case n <= math.MaxUint16:
		buf.WriteByte(0xda)
		_ = binary.Write(buf, binary.BigEndian, uint16(n))
	default:
		buf.WriteByte(0xdb)
		_ = binary.Write(buf, binary.BigEndian, uint32(n))
	}
	buf.WriteString(s)
}

// JSONDocMaxBytes bounds --format json input. The decoder holds the
// text, a boxed tree of it, and a converted copy at once, which is
// roughly 6x the text size for realistic documents and far more for
// pathological ones (an array of tiny integers boxes every element),
// so the cap is deliberately small next to the server's 1 GiB body
// limit; JSON is a convenience, not a bulk path.
const JSONDocMaxBytes = 64 << 20

// MsgPackDocSummary describes what a converted JSON document contains.
type MsgPackDocSummary struct {
	Items    int // measurements (1 for a single document)
	Columnar int // items in columnar shape
	Rows     int // total rows across columnar items + row items
}

// JSONToMsgPack reads one JSON document in Arc's write shapes and
// returns the equivalent MessagePack. It validates what the server
// would reject (400), what it would fail on later with a 500 (value
// column types), and what it would silently drop (bad batch items),
// so a mistake is a client error rather than a lost write.
//
// Shapes: {"m", "columns": {name: [values]}} (columnar, no tag
// metadata), {"m", "t"?, "h"?, "fields": {...}, "tags"?: {...}} (row;
// tags become tag columns, h defaults to "unknown" server-side),
// {"batch": [items...]}, or a top-level array of items.
func JSONToMsgPack(r io.Reader) ([]byte, MsgPackDocSummary, error) {
	var sum MsgPackDocSummary
	// LimitReader truncates an oversize document, which the decoder
	// would report as "unexpected EOF"; count what was read so the
	// error names the real cause.
	cr := &countedReader{r: io.LimitReader(r, JSONDocMaxBytes+1)}
	dec := json.NewDecoder(cr)
	dec.UseNumber()
	var root any
	tooBig := func() error {
		return fmt.Errorf("JSON document exceeds %d bytes (%d MiB); use --format msgpack for bulk data", JSONDocMaxBytes, JSONDocMaxBytes>>20)
	}
	if err := dec.Decode(&root); err != nil {
		if cr.n > JSONDocMaxBytes {
			return nil, sum, tooBig()
		}
		return nil, sum, fmt.Errorf("invalid JSON: %w", err)
	}
	if dec.InputOffset() > JSONDocMaxBytes {
		return nil, sum, tooBig()
	}
	// Nothing but whitespace may follow the document.
	if _, err := dec.Token(); err != io.EOF {
		return nil, sum, fmt.Errorf("invalid JSON: trailing content after the document")
	}
	converted, err := convertTop(root, &sum)
	if err != nil {
		return nil, sum, err
	}
	var buf bytes.Buffer
	if err := mpEncode(&buf, converted); err != nil {
		return nil, sum, err
	}
	return buf.Bytes(), sum, nil
}

func convertTop(root any, sum *MsgPackDocSummary) (any, error) {
	switch x := root.(type) {
	case map[string]any:
		if b, ok := x["batch"]; ok {
			items, ok := b.([]any)
			if !ok {
				return nil, fmt.Errorf(`"batch" must be an array of measurements`)
			}
			if len(items) == 0 {
				return nil, fmt.Errorf(`"batch" is empty; nothing to write`)
			}
			out := make([]any, 0, len(items))
			for i, it := range items {
				m, ok := it.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("batch item %d is not an object (the server would silently drop it)", i)
				}
				c, err := convertItem(m, fmt.Sprintf("batch item %d", i), sum)
				if err != nil {
					return nil, err
				}
				out = append(out, c)
			}
			return map[string]any{"batch": out}, nil
		}
		return convertItem(x, "document", sum)
	case []any:
		if len(x) == 0 {
			return nil, fmt.Errorf("top-level array is empty; nothing to write")
		}
		out := make([]any, 0, len(x))
		for i, it := range x {
			m, ok := it.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("array item %d is not an object (the server would silently drop it)", i)
			}
			c, err := convertItem(m, fmt.Sprintf("array item %d", i), sum)
			if err != nil {
				return nil, err
			}
			out = append(out, c)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("the document must be an object or an array of objects")
	}
}

func convertItem(m map[string]any, where string, sum *MsgPackDocSummary) (any, error) {
	sum.Items++
	name, ok := m["m"].(string)
	if !ok || name == "" {
		return nil, fmt.Errorf(`%s: "m" (measurement) must be a non-empty string`, where)
	}
	if err := ValidateMeasurementName(name); err != nil {
		return nil, fmt.Errorf("%s: %w", where, err)
	}
	out := map[string]any{"m": name}
	if cols, ok := m["columns"]; ok {
		sum.Columnar++
		cm, ok := cols.(map[string]any)
		if !ok || len(cm) == 0 {
			return nil, fmt.Errorf(`%s: "columns" must be a non-empty object of column: [values]`, where)
		}
		outCols := make(map[string]any, len(cm))
		n := -1
		for col, v := range cm {
			arr, ok := v.([]any)
			if !ok {
				return nil, fmt.Errorf("%s: column %q must be an array (the server would drop it)", where, col)
			}
			if n == -1 {
				n = len(arr)
			} else if len(arr) != n {
				return nil, fmt.Errorf("%s: column %q has %d values, expected %d (all columns must be the same length)", where, col, len(arr), n)
			}
			conv, err := convertColumn(col, arr, where)
			if err != nil {
				return nil, err
			}
			outCols[col] = conv
		}
		if n == 0 {
			return nil, fmt.Errorf("%s: columns are empty; nothing to write", where)
		}
		sum.Rows += n
		out["columns"] = outCols
		for k := range m {
			if k != "m" && k != "columns" {
				return nil, fmt.Errorf("%s: unexpected key %q in columnar shape (only m and columns)", where, k)
			}
		}
		return out, nil
	}
	// Row shape.
	fields, ok := m["fields"].(map[string]any)
	if !ok || len(fields) == 0 {
		return nil, fmt.Errorf(`%s: row shape needs a non-empty "fields" object (or use "columns")`, where)
	}
	outFields := make(map[string]any, len(fields))
	for k, v := range fields {
		cv, err := convertScalar(v, fmt.Sprintf("%s: field %q", where, k), false)
		if err != nil {
			return nil, err
		}
		outFields[k] = cv
	}
	out["fields"] = outFields
	if t, ok := m["t"]; ok {
		tv, err := convertScalar(t, where+`: "t"`, true)
		if err != nil {
			return nil, err
		}
		if _, isInt := tv.(int64); !isInt {
			return nil, fmt.Errorf(`%s: "t" must be an integer epoch timestamp`, where)
		}
		out["t"] = tv
	}
	if h, ok := m["h"]; ok {
		hs, ok := h.(string)
		if !ok || hs == "" {
			return nil, fmt.Errorf(`%s: "h" (host) must be a non-empty string`, where)
		}
		out["h"] = hs
	}
	if tags, ok := m["tags"]; ok {
		tm, ok := tags.(map[string]any)
		if !ok {
			return nil, fmt.Errorf(`%s: "tags" must be an object`, where)
		}
		outTags := make(map[string]any, len(tm))
		for k, v := range tm {
			switch tv := v.(type) {
			case string:
				outTags[k] = tv
			case json.Number:
				outTags[k] = tv.String()
			case bool:
				if tv {
					outTags[k] = "true"
				} else {
					outTags[k] = "false"
				}
			default:
				return nil, fmt.Errorf("%s: tag %q must be a string, number, or boolean", where, k)
			}
		}
		out["tags"] = outTags
	}
	for k := range m {
		switch k {
		case "m", "t", "h", "fields", "tags":
		case "f":
			return nil, fmt.Errorf(`%s: the compact "f" array is not supported; use "fields"`, where)
		default:
			return nil, fmt.Errorf("%s: unexpected key %q in row shape", where, k)
		}
	}
	sum.Rows++
	return out, nil
}

// convertColumn converts one columnar array, enforcing the server's
// rules that would otherwise fail with a 500 (mixed types, nested
// values, integers out of range) and, for "time", the rules that make
// the magnitude-based unit inference safe.
func convertColumn(col string, arr []any, where string) ([]any, error) {
	isTime := col == "time"
	out := make([]any, len(arr))
	var kind string
	var bucket int
	hasInt, hasFloat := false, false
	for i, v := range arr {
		cv, err := convertScalar(v, fmt.Sprintf("%s: column %q[%d]", where, col, i), isTime)
		if err != nil {
			return nil, err
		}
		if isTime {
			iv, ok := cv.(int64)
			if !ok {
				return nil, fmt.Errorf(`%s: "time" values must be integer epochs (seconds, ms, us, or ns), got %v at index %d`, where, v, i)
			}
			b := timeBucket(iv)
			if i == 0 {
				bucket = b
			} else if b != bucket {
				return nil, fmt.Errorf(`%s: "time"[%d]=%d is not the same unit as time[0]; the server infers the unit from the first value only`, where, i, iv)
			}
		} else if cv != nil {
			k := "number"
			switch cv.(type) {
			case int64:
				hasInt = true
			case float64:
				hasFloat = true
			default:
				k = fmt.Sprintf("%T", cv)
			}
			if kind == "" {
				kind = k
			} else if k != kind {
				return nil, fmt.Errorf("%s: column %q mixes %s and %s values (the server types a column from its first non-null value)", where, col, kind, k)
			}
		}
		out[i] = cv
	}
	// A numeric column with both integers and floats is promoted to
	// float64 so the server sees one type (it types from the first
	// non-null value and converts the rest, which can fail with a 500).
	if !isTime && hasInt && hasFloat {
		for i, v := range out {
			if iv, ok := v.(int64); ok {
				if iv > 1<<53 || iv < -(1<<53) {
					return nil, fmt.Errorf("%s: column %q mixes integers and floats and %d cannot be represented exactly as a float", where, col, iv)
				}
				out[i] = float64(iv)
			}
		}
	}
	return out, nil
}

// timeBucket mirrors the server's magnitude thresholds
// (arc/internal/ingest/msgpack.go): <1e10 s, <1e13 ms, <1e16 us, else ns.
func timeBucket(v int64) int {
	switch {
	case v < 0:
		return -1
	case v < 1e10:
		return 0
	case v < 1e13:
		return 1
	case v < 1e16:
		return 2
	}
	return 3
}

// convertScalar turns a decoded JSON value into an encodable scalar.
// Numbers: integral → int64 (range-checked), otherwise float64 (or, for
// timestamps, an error unless integral). Nested values are rejected.
func convertScalar(v any, where string, isTime bool) (any, error) {
	switch x := v.(type) {
	case nil:
		if isTime {
			return nil, fmt.Errorf("%s: time values must not be null", where)
		}
		return nil, nil
	case bool, string:
		return x, nil
	case json.Number:
		s := x.String()
		if !strings.ContainsAny(s, ".eE") {
			i, err := x.Int64()
			if err != nil {
				return nil, fmt.Errorf("%s: integer %s is out of int64 range", where, s)
			}
			return i, nil
		}
		f, err := x.Float64()
		if err != nil {
			return nil, fmt.Errorf("%s: invalid number %s", where, s)
		}
		if isTime {
			if f != math.Trunc(f) || math.Abs(f) >= 1<<63 {
				return nil, fmt.Errorf("%s: timestamp %s is not an integer epoch", where, s)
			}
			return int64(f), nil
		}
		return f, nil
	default:
		return nil, fmt.Errorf("%s: nested objects and arrays are not valid values", where)
	}
}

// MsgPackFirstByteOK reports whether b can begin a document the server
// accepts: a msgpack map or array (fixmap, map16/32, fixarray,
// array16/32) or a gzip / zstd magic byte. Text formats (line protocol,
// CSV, JSON) start with a letter, digit, brace or bracket and fail this
// check; it is a UX guard, not a boundary (a text file starting with
// "(" passes as the zstd magic and is rejected by the server instead).
func MsgPackFirstByteOK(b byte) bool {
	switch {
	case b >= 0x80 && b <= 0x9f: // fixmap, fixarray
		return true
	case b == 0xdc || b == 0xdd || b == 0xde || b == 0xdf:
		return true
	case b == 0x1f: // gzip
		return true
	case b == 0x28: // zstd
		return true
	}
	return false
}

// countedReader counts bytes handed to the JSON decoder.
type countedReader struct {
	r io.Reader
	n int64
}

func (c *countedReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
