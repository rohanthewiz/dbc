package etl

import (
	"database/sql"
	"encoding/json"
	"math"
	"testing"
	"time"
)

func TestAppendPGText(t *testing.T) {
	ts := time.Date(2024, 1, 2, 3, 4, 5, 123456000, time.FixedZone("x", -5*3600))
	cases := []struct {
		name  string
		v     any
		bytea bool
		want  string
	}{
		{"nil", nil, false, `\N`},
		{"plain", "hello", false, "hello"},
		{"escapes", "a\tb\nc\rd\\e", false, `a\tb\nc\rd\\e`},
		{"literal backslash-N is not NULL", `\N`, false, `\\N`},
		{"bool", true, false, "t"},
		{"int64", int64(-42), false, "-42"},
		{"int", 7, false, "7"},
		{"uint64", uint64(18446744073709551615), false, "18446744073709551615"},
		{"float", 0.1, false, "0.1"},
		{"nan", math.NaN(), false, "NaN"},
		{"inf", math.Inf(1), false, "Infinity"},
		{"-inf", math.Inf(-1), false, "-Infinity"},
		{"time keeps zone and micros", ts, false, "2024-01-02 03:04:05.123456-05:00"},
		{"utc time", time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC), false, "2024-01-02 00:00:00Z"},
		// Go's year -43 is 44 BC; Postgres rejects "-0043-…" outright
		{"bc date", time.Date(-43, 3, 15, 0, 0, 0, 0, time.UTC), false, "0044-03-15 00:00:00Z BC"},
		// Go's year 0 is 1 BC, a leap year in both numberings' terms
		{"1 bc leap day", time.Date(0, 2, 29, 12, 0, 0, 0, time.UTC), false, "0001-02-29 12:00:00Z BC"},
		{"five-digit year", time.Date(12345, 6, 7, 1, 2, 3, 0, time.UTC), false, "12345-06-07 01:02:03Z"},
		// local mean time: the offset's seconds are part of the instant
		{"lmt offset keeps seconds", time.Date(1850, 1, 1, 0, 0, 0, 0,
			time.FixedZone("LMT", -(7*3600+52*60+58))), false, "1850-01-01 00:00:00-07:52:58"},
		{"bytea hex", []byte{0xde, 0xad, '\t'}, true, `\\xdead09`},
		{"bytes into text column", []byte("{\"a\":\t1}"), false, `{"a":\t1}`},
		// what a Transform may hand back instead of what it was given
		{"int slice is an array", []int{1, 2}, false, "{1,2}"},
		{"string slice quotes and escapes", []string{"a b", `q"uote`, "NULL", `back\slash`}, false,
			`{"a b","q\\"uote","NULL","back\\\\slash"}`},
		{"any slice with a nil", []any{1, nil, 2.5}, false, "{1,NULL,2.5}"},
		{"nested slice is a 2-d array", [][]int{{1, 2}, {3, 4}}, false, "{{1,2},{3,4}}"},
		{"bytes slice is a bytea array", [][]byte{{0xde, 0xad}}, false, `{"\\\\xdead"}`},
		{"nil slice is NULL", []string(nil), false, `\N`},
		{"map is json", map[string]any{"a": 1}, false, `{"a":1}`},
		{"nil map is NULL", map[string]any(nil), false, `\N`},
		{"json.RawMessage is its bytes", json.RawMessage(`{"x":1}`), false, `{"x":1}`},
		{"duration in microseconds", 90*time.Minute + 1500*time.Millisecond, false, "5401500000 microseconds"},
		{"null Valuer", sql.NullString{}, false, `\N`},
		{"Valuer", sql.NullInt64{Int64: 5, Valid: true}, false, "5"},
		{"nil pointer", (*int)(nil), false, `\N`},
		{"pointer", func() *string { s := "p\tq"; return &s }(), false, `p\tq`},
		{"named int", time.Month(5), false, "May"}, // a Stringer: its String()
		{"struct is json", struct{ A int }{3}, false, `{"A":3}`},
	}
	for _, c := range cases {
		if got := string(appendPGText(nil, c.v, c.bytea)); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestPGTypeName(t *testing.T) {
	cases := map[string]string{
		"INT4": "int4", "_TEXT": "text[]", "TIMESTAMPTZ": "timestamptz", "_BPCHAR": "bpchar[]",
		// what pgx's database/sql driver reports for a type it does not
		// register: the OID — never a type name DDL can use
		"": "text", "790": "text", "16385": "text",
		// pseudo-types: no column can have them
		"RECORD": "text", "_RECORD": "text", "UNKNOWN": "text",
		// bare, these name a different type than the one reported
		"BIT": "varbit", "_BIT": "varbit[]", "CHAR": `"char"`,
	}
	for in, want := range cases {
		if got := pgTypeName(in); got != want {
			t.Errorf("pgTypeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAppendPGRow(t *testing.T) {
	got := string(appendPGRow(nil, []any{int64(1), nil, "x\ty", []byte{1}}, []bool{false, false, false, true}))
	if want := "1\t\\N\tx\\ty\t\\\\x01\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestQuoteTable(t *testing.T) {
	cases := []struct {
		e    Engine
		in   string
		want string
	}{
		{Postgres, "public.users", `"public"."users"`},
		{Postgres, `we"ird`, `we"ird`}, // already quoted by the caller: verbatim
		{MySQL, "db.t", "`db`.`t`"},
		{SQLite, "t", `"t"`},
	}
	for _, c := range cases {
		if got := c.e.QuoteTable(c.in); got != c.want {
			t.Errorf("%v %q: got %s, want %s", c.e, c.in, got, c.want)
		}
	}
	if got := MySQL.QuoteIdent("a`b"); got != "`a``b`" {
		t.Errorf("mysql embedded backtick: %s", got)
	}
}

func TestFamilyOf(t *testing.T) {
	cases := map[string]typeFamily{
		"INT4": famInt, "UNSIGNED BIGINT": famInt, "INTERVAL": famText, "POINT": famText,
		"FLOAT8": famFloat, "DOUBLE": famFloat, "NUMERIC": famNumeric, "DECIMAL": famNumeric,
		"BOOL": famBool, "DATE": famDate, "DATETIME": famTimestamp, "TIMESTAMP": famTimestamp,
		"TIMESTAMPTZ": famTimestampTZ, "BYTEA": famBytes, "LONGBLOB": famBytes,
		"VARCHAR": famText, "": famText, "_INT4": famText, "INT4RANGE": famText,
		"UNSIGNED INT": famInt, "TINYINT": famInt,
		// SQLite's declared types keep their modifiers.
		"DECIMAL(10,2)": famNumeric, "NUMERIC(5)": famNumeric, "NUMERICAL": famText,
	}
	for in, want := range cases {
		if got := familyOf(in); got != want {
			t.Errorf("familyOf(%q) = %d, want %d", in, got, want)
		}
	}
}
