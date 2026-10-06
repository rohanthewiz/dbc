package etl

import (
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"time"
)

// The Postgres Writer loads with COPY … FROM STDIN in the *text* format, not
// binary. Binary COPY needs every value encoded as exactly the destination
// column's type (an int4 column rejects an int8), so a load from MySQL or
// SQLite — or a Transform that returns an int where the column is numeric —
// would fail on types that obviously fit. In text format the server parses
// each field with the column type's own input function, the same as a
// literal in an INSERT, so anything Postgres would accept as a literal
// loads: "42" into an integer, "2024-01-02 03:04:05" into a timestamptz.
// The parse costs the server a little CPU; it is still far faster than
// INSERTs, which pay a round trip and a plan per batch.
//
// Text format, per row: fields separated by a tab, the row ended by a
// newline, NULL written as \N, and backslash, tab, newline and carriage
// return inside a value escaped with a backslash.

// pgTimeLayout is how a time.Time is written for a date, timestamp or
// timestamptz column. The zone offset is always present: a timestamptz
// needs it, and timestamp and date ignore it. Microseconds is Postgres's
// resolution.
const pgTimeLayout = "2006-01-02 15:04:05.999999Z07:00"

// appendPGText appends one field of a COPY text row. bytea says the
// destination column is bytea, where a []byte goes in as hex; elsewhere a
// []byte is the column's text (json, say, from a Transform).
func appendPGText(b []byte, v any, bytea bool) []byte {
	switch t := v.(type) {
	case nil:
		return append(b, `\N`...)
	case string:
		return appendPGEscaped(b, t)
	case []byte:
		if bytea {
			// "\x<hex>" is bytea's input form; its backslash is itself
			// escaped for COPY, hence two.
			b = append(b, `\\x`...)
			return hex.AppendEncode(b, t)
		}
		return appendPGEscaped(b, string(t))
	case bool:
		if t {
			return append(b, 't')
		}
		return append(b, 'f')
	case int64:
		return strconv.AppendInt(b, t, 10)
	case int:
		return strconv.AppendInt(b, int64(t), 10)
	case int32:
		return strconv.AppendInt(b, int64(t), 10)
	case int16:
		return strconv.AppendInt(b, int64(t), 10)
	case int8:
		return strconv.AppendInt(b, int64(t), 10)
	case uint64:
		return strconv.AppendUint(b, t, 10)
	case uint:
		return strconv.AppendUint(b, uint64(t), 10)
	case uint32:
		return strconv.AppendUint(b, uint64(t), 10)
	case float64:
		return appendPGFloat(b, t, 64)
	case float32:
		return appendPGFloat(b, float64(t), 32)
	case time.Time:
		return t.AppendFormat(b, pgTimeLayout)
	case fmt.Stringer:
		return appendPGEscaped(b, t.String())
	}
	return appendPGEscaped(b, fmt.Sprint(v))
}

// appendPGFloat writes the shortest form that round-trips, and the
// non-finite values in the spelling Postgres's float input accepts (Go's
// "+Inf" is not one of them).
func appendPGFloat(b []byte, f float64, bits int) []byte {
	switch {
	case math.IsNaN(f):
		return append(b, "NaN"...)
	case math.IsInf(f, 1):
		return append(b, "Infinity"...)
	case math.IsInf(f, -1):
		return append(b, "-Infinity"...)
	}
	return strconv.AppendFloat(b, f, 'g', -1, bits)
}

// appendPGEscaped appends s with COPY text escapes. The common case — no
// character needing an escape — is one append, no per-byte work beyond the
// scan.
func appendPGEscaped(b []byte, s string) []byte {
	start := 0
	for i := 0; i < len(s); i++ {
		var esc byte
		switch s[i] {
		case '\\':
			esc = '\\'
		case '\n':
			esc = 'n'
		case '\r':
			esc = 'r'
		case '\t':
			esc = 't'
		default:
			continue
		}
		b = append(b, s[start:i]...)
		b = append(b, '\\', esc)
		start = i + 1
	}
	return append(b, s[start:]...)
}

// appendPGRow appends a whole row: fields tab-separated, newline-ended.
func appendPGRow(b []byte, row []any, bytea []bool) []byte {
	for i, v := range row {
		if i > 0 {
			b = append(b, '\t')
		}
		b = appendPGText(b, v, i < len(bytea) && bytea[i])
	}
	return append(b, '\n')
}
