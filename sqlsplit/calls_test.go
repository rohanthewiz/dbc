package sqlsplit

import (
	"slices"
	"testing"
)

// Calls finds a function by the paren after its name — through blanks, a
// comment and a schema — and nowhere a string, a comment, a quoted name or
// a parameter holds the same text.
func TestCalls(t *testing.T) {
	cases := []struct {
		d    Dialect
		sql  string
		want []string
	}{
		{Postgres, "SELECT set_config('search_path', 'x', false)", []string{"set_config"}},
		{Postgres, "select PG_CATALOG.Set_Config('a', 'b', false)", []string{"set_config"}},
		{Postgres, "SELECT pg_advisory_lock /* id */ (42)", []string{"pg_advisory_lock"}},
		{Postgres, "SELECT pg_advisory_lock\n\t(42)", []string{"pg_advisory_lock"}},
		{Postgres, "SELECT * FROM jobs WHERE pg_try_advisory_lock(id) LIMIT 1", []string{"pg_try_advisory_lock"}},
		{Postgres, "SELECT lower(upper(x)) FROM t", []string{"lower", "upper"}},
		// a keyword before a paren is listed too: a superset to look up in
		{Postgres, "SELECT 1 WHERE x IN (1)", []string{"in"}},
		// not calls
		{Postgres, "SELECT 'set_config(1)'", nil},
		{Postgres, "SELECT $$ set_config(1) $$", nil},
		{Postgres, "SELECT 1 -- set_config(1)", nil},
		{Postgres, "SELECT /* set_config(1) */ 1", nil},
		{Postgres, `SELECT "set_config"('a', 'b', false)`, nil},
		{Postgres, "SELECT set_config", nil},
		{Postgres, "SELECT a$set_config(1)", []string{"a$set_config"}},
		{MySQL, "SELECT @get_lock (1)", nil},
		{MySQL, "SELECT :get_lock (1)", nil},
		{MySQL, "SELECT GET_LOCK('a', 0) # get_lock(", []string{"get_lock"}},
		// the dialect's reading of strings: on Postgres 'a\' ends at its
		// quote, so the call after it is code; on MySQL the string runs on
		{Postgres, `SELECT 'a\', get_lock('b', 0)`, []string{"get_lock"}},
		{MySQL, `SELECT 'a\', get_lock('b', 0)`, nil},
		{SQLite, "PRAGMA main.foreign_keys(1)", []string{"foreign_keys"}},
	}
	for _, c := range cases {
		if got := c.d.Calls(c.sql); !slices.Equal(got, c.want) {
			t.Errorf("%s.Calls(%q) = %q, want %q", c.d, c.sql, got, c.want)
		}
	}
}

// HasOp sees an operator in code only.
func TestHasOp(t *testing.T) {
	cases := []struct {
		d    Dialect
		sql  string
		op   string
		want bool
	}{
		{MySQL, "SELECT @v := 1", ":=", true},
		{MySQL, "SELECT @v:=1", ":=", true},
		{MySQL, "SELECT ':=' AS a", ":=", false},
		{MySQL, "SELECT 1 # :=", ":=", false},
		{MySQL, "SELECT `a:=b` FROM t", ":=", false},
		{SQLite, "PRAGMA foreign_keys = ON", "=", true},
		{SQLite, "PRAGMA foreign_keys", "=", false},
		{SQLite, "PRAGMA table_info('a=b')", "=", false},
		{SQLite, "PRAGMA /* = */ foreign_keys", "=", false},
		{Postgres, "SELECT $x$ := $x$", ":=", false},
	}
	for _, c := range cases {
		if got := c.d.HasOp(c.sql, c.op); got != c.want {
			t.Errorf("%s.HasOp(%q, %q) = %v, want %v", c.d, c.sql, c.op, got, c.want)
		}
	}
}
