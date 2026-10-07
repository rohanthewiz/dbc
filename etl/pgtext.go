package etl

import (
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
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

// pgTimeLayoutSecs is pgTimeLayout for a zone whose offset has seconds.
// Local mean time — every zone before standard time reached it, e.g.
// America/Los_Angeles is -07:52:58 until 1883 — has them, and "Z07:00"
// would silently drop them, moving the instant by up to a minute.
const pgTimeLayoutSecs = "2006-01-02 15:04:05.999999Z07:00:00"

// appendPGText appends one field of a COPY text row. bytea says the
// destination column is bytea, where a []byte goes in as hex; elsewhere a
// []byte is the column's text (json, say, from a Transform).
//
// The cases up to time.Time are what a Reader yields, and are written
// without allocating. The rest are what a script's Transform or Writer
// might hand over instead (see pgOtherText): a time.Duration, an
// sql.NullString, a []string for a text[] column, a map for a jsonb one.
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
		return appendPGTime(b, t)
	}
	s, null := pgOtherText(v, bytea)
	if null {
		return append(b, `\N`...)
	}
	return appendPGEscaped(b, s)
}

// pgOtherText is the text — before COPY escaping — of a value none of
// appendPGText's fast cases took, and whether it is NULL. In order:
//
//	time.Duration    "<n> microseconds": its String() ("1.5µs", "2h0m0s")
//	                 is not interval input for every value; this is, exactly
//	                 to Postgres's resolution
//	driver.Valuer    its Value(): sql.NullString and friends, decimal types
//	fmt.Stringer     its String(): a uuid.UUID, pgtype.InfinityModifier
//	nil pointer      NULL; any other pointer, the value it points at
//	[]byte-like      a named byte slice (json.RawMessage) is bytes, as []byte
//	slice, array     an array literal, {…} (pgArrayLiteral) — for a text[] or
//	                 int[] column; a nil slice is NULL
//	map, struct      JSON — for a json or jsonb column; a nil map is NULL
//	anything else    fmt.Sprint (a named int, say)
func pgOtherText(v any, bytea bool) (string, bool) {
	switch t := v.(type) {
	case time.Duration:
		return strconv.FormatInt(t.Microseconds(), 10) + " microseconds", false
	case driver.Valuer:
		val, err := t.Value()
		if err != nil {
			return fmt.Sprint(v), false
		}
		if _, again := val.(driver.Valuer); again {
			// a Value() that returns another Valuer: stop rather than loop
			return fmt.Sprint(val), false
		}
		return pgPlainText(val, bytea)
	case fmt.Stringer:
		return t.String(), false
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			return "", true
		}
		return pgPlainText(rv.Elem().Interface(), bytea)
	case reflect.Slice, reflect.Array:
		if rv.Kind() == reflect.Slice && rv.IsNil() {
			return "", true
		}
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			raw := make([]byte, rv.Len())
			reflect.Copy(reflect.ValueOf(raw), rv)
			return pgPlainText(raw, bytea)
		}
		return string(appendPGArray(nil, rv)), false
	case reflect.Map, reflect.Struct:
		if rv.Kind() == reflect.Map && rv.IsNil() {
			return "", true
		}
		if js, err := json.Marshal(v); err == nil {
			return string(js), false
		}
	}
	return fmt.Sprint(v), false
}

// pgPlainText is any value's text before COPY escaping, and whether it is
// NULL: appendPGText's own output for the types it writes without escapes,
// pgOtherText's for the rest. It serves the values found inside another —
// an array's elements, what a pointer or a Valuer leads to.
func pgPlainText(v any, bytea bool) (string, bool) {
	switch t := v.(type) {
	case nil:
		return "", true
	case string:
		return t, false
	case []byte:
		if bytea {
			return `\x` + hex.EncodeToString(t), false
		}
		return string(t), false
	case bool, int64, int, int32, int16, int8, uint64, uint, uint32, float64, float32, time.Time:
		// none of these writes a character COPY would escape
		return string(appendPGText(nil, v, false)), false
	}
	return pgOtherText(v, bytea)
}

// appendPGArray writes a Go slice or array as a Postgres array literal, the
// form a text[], int[] or any other array column takes as input:
//
//	[]string{"a b", `q"uote`, "NULL"}  ─► {"a b","q\"uote","NULL"}
//	[]any{1, nil, 2.5}                 ─► {1,NULL,2.5}
//	[][]int{{1, 2}, {3, 4}}            ─► {{1,2},{3,4}}   (int[][])
//	[][]byte{{0xde, 0xad}}             ─► {"\\xdead"}     (bytea[])
//
// Numbers and booleans go in bare; every other element is double-quoted,
// with backslash and quote escaped, so a value with a comma, a brace,
// spaces at either end or the word NULL comes through as itself. A nested
// slice is a sub-array (Postgres wants them all the same length). The
// result is still COPY-escaped by the caller, like any other field.
func appendPGArray(b []byte, rv reflect.Value) []byte {
	b = append(b, '{')
	for i := 0; i < rv.Len(); i++ {
		if i > 0 {
			b = append(b, ',')
		}
		e := rv.Index(i)
		for (e.Kind() == reflect.Interface || e.Kind() == reflect.Pointer) && !e.IsNil() {
			e = e.Elem()
		}
		switch e.Kind() {
		case reflect.Interface, reflect.Pointer: // nil
			b = append(b, "NULL"...)
			continue
		case reflect.Slice, reflect.Array:
			if e.Type().Elem().Kind() != reflect.Uint8 {
				if e.Kind() == reflect.Slice && e.IsNil() {
					b = append(b, "NULL"...)
				} else {
					b = appendPGArray(b, e)
				}
				continue
			}
		}
		s, null := pgPlainText(e.Interface(), true)
		if null {
			b = append(b, "NULL"...)
			continue
		}
		if k := e.Kind(); k == reflect.Bool || (k >= reflect.Int && k <= reflect.Float64) {
			b = append(b, s...)
			continue
		}
		b = append(b, '"')
		for j := 0; j < len(s); j++ {
			if s[j] == '\\' || s[j] == '"' {
				b = append(b, '\\')
			}
			b = append(b, s[j])
		}
		b = append(b, '"')
	}
	return append(b, '}')
}

// appendPGTime writes t in a form Postgres's date, timestamp and timestamptz
// input all accept, for every year either side can hold.
//
// Go counts years astronomically — year 0 is 1 BC, -43 is 44 BC — and
// formats them with a minus sign ("-0043-03-15"), which Postgres reads as
// a malformed zone ("time zone displacement out of range"). Postgres wants
// the BC year and a trailing "BC" instead:
//
//	Go year  y ≥ 1  ─► "yyyy-mm-dd hh:mm:ss…±zz"        (unchanged)
//	Go year  y ≤ 0  ─► "%04d(1-y)-mm-dd hh:mm:ss…±zz BC"
//
// Only the year is rewritten. Month, day, clock and zone come from Go's own
// formatting of t itself, never from a time rebuilt with the BC year: the
// two numberings differ in which years are leap years (1 BC is one, 1 AD is
// not), so a rebuilt Feb 29 would roll over into March.
func appendPGTime(b []byte, t time.Time) []byte {
	layout := pgTimeLayout
	if _, off := t.Zone(); off%60 != 0 {
		layout = pgTimeLayoutSecs
	}
	y := t.Year()
	if y > 0 {
		return t.AppendFormat(b, layout)
	}
	// layout[4:] is "-01-02 15:04:05…": the year's digits are written here,
	// the rest — starting with the literal "-" — by AppendFormat.
	b = fmt.Appendf(b, "%04d", 1-y)
	b = t.AppendFormat(b, layout[4:])
	return append(b, " BC"...)
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
