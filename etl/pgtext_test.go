package etl

import (
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
		{"bytea hex", []byte{0xde, 0xad, '\t'}, true, `\\xdead09`},
		{"bytes into text column", []byte("{\"a\":\t1}"), false, `{"a":\t1}`},
		{"fallback Sprint", []int{1, 2}, false, "[1 2]"},
	}
	for _, c := range cases {
		if got := string(appendPGText(nil, c.v, c.bytea)); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
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
