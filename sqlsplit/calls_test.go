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

// Words gives code words with their paren depth, and passes over what
// Calls passes over: strings, comments, quoted names, parameters.
func TestWords(t *testing.T) {
	str := func(ws []Word) string {
		var b []byte
		for i, w := range ws {
			if i > 0 {
				b = append(b, ' ')
			}
			b = append(b, w.Text...)
			b = append(b, ':')
			b = append(b, byte('0'+w.Depth))
		}
		return string(b)
	}
	cases := []struct {
		d         Dialect
		sql, want string
	}{
		{Postgres, "SELECT a FROM (SELECT b FROM t LIMIT 5) s FOR UPDATE",
			"select:0 a:0 from:0 select:1 b:1 from:1 t:1 limit:1 5:1 s:0 for:0 update:0"},
		{Postgres, "select X from T Limit 10 Offset 2", "select:0 x:0 from:0 t:0 limit:0 10:0 offset:0 2:0"},
		// strings, comments, quoted names, dollar bodies and parameters are
		// not words, and their parens do not count
		{Postgres, "SELECT '(' , \"for\" /* ( */ -- (\nFROM t WHERE a = $1 AND $$ ) $$ = b",
			"select:0 from:0 t:0 where:0 a:0 and:0 b:0"},
		{MySQL, "SELECT @for, :update FROM t", "select:0 from:0 t:0"},
		// a stray ')' does not go below 0
		{Postgres, "x) y (z", "x:0 y:0 z:1"},
		// a$b is one word
		{Postgres, "SELECT a$b", "select:0 a$b:0"},
	}
	for _, c := range cases {
		if got := str(c.d.Words(c.sql)); got != c.want {
			t.Errorf("%s.Words(%q)\n got %s\nwant %s", c.d, c.sql, got, c.want)
		}
	}
}
