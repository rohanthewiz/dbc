package sqlsplit

import (
	"strings"
	"testing"
)

// TestDialectSplit is N-238: where a string ends is the engine's rule, so
// the same text splits differently by dialect. Each case is what the
// engine itself would run: on Postgres, SQLite and bytdb 'a\' is a whole
// string (a backslash is plain text there), on MySQL it is an unterminated
// one whose \' is an escaped quote.
func TestDialectSplit(t *testing.T) {
	two := []string{`SELECT 'a\'`, "SELECT 2"}
	one := []string{`SELECT 'a\';` + "\nSELECT 2"}
	cases := []struct {
		name string
		d    Dialect
		sql  string
		want []string
	}{
		// a string ending in a backslash — the designer's `x = 'a\'`
		{"postgres trailing backslash", Postgres, "SELECT 'a\\';\nSELECT 2", two},
		{"sqlite trailing backslash", SQLite, "SELECT 'a\\';\nSELECT 2", two},
		{"bytdb trailing backslash", Bytdb, "SELECT 'a\\';\nSELECT 2", two},
		{"mysql trailing backslash", MySQL, "SELECT 'a\\';\nSELECT 2", one},
		{"generic trailing backslash", Generic, "SELECT 'a\\';\nSELECT 2", one},

		// SQLite's ends-with: its LIKE escape is !, so a value's own
		// backslash is written single, right before the closing quote
		{"sqlite ends-with", SQLite, `SELECT * FROM t WHERE x LIKE '%a\' ESCAPE '!';` + "\nSELECT 2",
			[]string{`SELECT * FROM t WHERE x LIKE '%a\' ESCAPE '!'`, "SELECT 2"}},

		// MySQL's escaped quote, which only MySQL and Generic read as one
		{"mysql escaped quote", MySQL, `SELECT 'it\'s; here';SELECT 2`,
			[]string{`SELECT 'it\'s; here'`, "SELECT 2"}},
		{"generic escaped quote", Generic, `SELECT 'it\'s; here';SELECT 2`,
			[]string{`SELECT 'it\'s; here'`, "SELECT 2"}},

		// an E'…' string escapes on Postgres and bytdb, in either case
		{"postgres E string", Postgres, `SELECT E'it\'s; here';SELECT 2`,
			[]string{`SELECT E'it\'s; here'`, "SELECT 2"}},
		{"postgres e string", Postgres, `SELECT e'it\'s; here';SELECT 2`,
			[]string{`SELECT e'it\'s; here'`, "SELECT 2"}},
		{"bytdb E string", Bytdb, `SELECT E'a\\';SELECT 2`, []string{`SELECT E'a\\'`, "SELECT 2"}},
		{"postgres E string at buffer start", Postgres, `E'a\'b';SELECT 2`, []string{`E'a\'b'`, "SELECT 2"}},
		// ...but the e ending a word is not a prefix: this is a plain
		// string after the word "some", which ends at its first quote
		{"postgres word ending in e", Postgres, `SELECT some'a\';SELECT 2`,
			[]string{`SELECT some'a\'`, "SELECT 2"}},
		// SQLite has no escape strings: E'a\' is the word E, then 'a\'
		{"sqlite has no E string", SQLite, `SELECT E'a\';SELECT 2`, []string{`SELECT E'a\'`, "SELECT 2"}},

		// quoted names double their quote on every engine; a backslash in
		// one is never an escape, Generic and MySQL included
		{"mysql backquoted backslash", MySQL, "SELECT `a\\`;SELECT 2", []string{"SELECT `a\\`", "SELECT 2"}},
		{"generic quoted backslash", Generic, `SELECT "a\";SELECT 2`, []string{`SELECT "a\"`, "SELECT 2"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			eq(t, texts(c.d.Split(c.sql)), c.want)
		})
	}
}

// TestPackageFunctionsAreGeneric pins the package-level functions to the
// rule every scan followed before dialects: they are what a caller with no
// engine to name gets, and changing them would silently change it.
func TestPackageFunctionsAreGeneric(t *testing.T) {
	sql := "SELECT 'a\\';\nSELECT 2"
	eq(t, texts(Split(sql)), texts(Generic.Split(sql)))
	if got := len(Split(sql)); got != 1 {
		t.Errorf("Split(%q) = %d statements, want the 1 a backslash escape makes", sql, got)
	}
	var zero Dialect
	if zero != Generic {
		t.Errorf("the zero Dialect is %v, want Generic", zero)
	}
}

func TestDialectOf(t *testing.T) {
	cases := map[string]Dialect{
		// config names and db.Driver's canonical ones
		"postgres": Postgres, "PostgreSQL": Postgres, "pg": Postgres, "pgx": Postgres,
		"mysql": MySQL, "mariadb": MySQL,
		"sqlite": SQLite, "sqlite3": SQLite,
		"bytdb": Bytdb,
		// code fence tags naming an engine
		"psql": Postgres, "pgsql": Postgres, "plpgsql": Postgres,
		// no engine named
		"": Generic, "sql": Generic, "oracle": Generic,
	}
	for name, want := range cases {
		if got := DialectOf(name); got != want {
			t.Errorf("DialectOf(%q) = %v, want %v", name, got, want)
		}
	}
	for _, d := range []Dialect{Postgres, MySQL, SQLite, Bytdb} {
		if back := DialectOf(d.String()); back != d {
			t.Errorf("DialectOf(%v.String()) = %v", d, back)
		}
	}
}

// TestDialectLexAgreesWithSplit is the editor's promise per dialect: what
// it colors as a string is what the run splits as one, so the statement
// after 'a\' is drawn as code on Postgres, and as string on MySQL, where
// it is one.
func TestDialectLexAgreesWithSplit(t *testing.T) {
	sql := "SELECT 'a\\';\nSELECT 2"
	for _, c := range []struct {
		d       Dialect
		strings []string
	}{
		{Postgres, []string{`'a\'`}},
		{SQLite, []string{`'a\'`}},
		{Bytdb, []string{`'a\'`}},
		{MySQL, []string{"'a\\';\nSELECT 2"}},
	} {
		var got []string
		for _, tk := range c.d.Lex(sql) {
			if tk.Kind == TokString {
				got = append(got, sql[tk.Start:tk.End])
			}
		}
		if strings.Join(got, "|") != strings.Join(c.strings, "|") {
			t.Errorf("%v: strings %q, want %q", c.d, got, c.strings)
		}
		// and the keyword after it is a keyword exactly where Split cut
		kw := false
		for _, tk := range c.d.Lex(sql) {
			kw = kw || tk.Kind == TokKeyword && tk.Start == strings.LastIndex(sql, "SELECT")
		}
		if want := len(c.d.Split(sql)) == 2; kw != want {
			t.Errorf("%v: second SELECT colored as keyword = %v, Split found it a statement = %v", c.d, kw, want)
		}
	}
}

// TestDialectHasKeyword is the run's RETURNING check (db.isQuery): on
// Postgres the RETURNING after 'a\' is outside the string, so the INSERT
// returns rows; read by MySQL's rule it would be inside it.
func TestDialectHasKeyword(t *testing.T) {
	sql := `INSERT INTO t VALUES ('a\') RETURNING id`
	for _, d := range []Dialect{Postgres, SQLite, Bytdb} {
		if !d.HasKeyword(sql, "returning") {
			t.Errorf("%v: RETURNING after 'a\\' not found", d)
		}
	}
	if MySQL.HasKeyword(sql, "returning") {
		t.Errorf("mysql: RETURNING found, but 'a\\') … runs on to the end there")
	}
	// Verbs, which finds the main verb past a CTE list, reads alike
	with := `WITH a AS (SELECT 'x\') DELETE FROM t RETURNING id`
	if v := Postgres.Verbs(with); v.Main != "delete" {
		t.Errorf("postgres Verbs(%q).Main = %q, want delete", with, v.Main)
	}
}

// TestDialectFolds: a fold hides the statement the dialect's run would,
// so on Postgres the statement after 'a\' folds on its own.
func TestDialectFolds(t *testing.T) {
	sql := "SELECT 'a\\'\nFROM t;\nSELECT 2\nFROM u;"
	if n := len(Postgres.Folds(sql)); n != 2 {
		t.Errorf("postgres: %d folds, want one per statement (2)", n)
	}
	if n := len(MySQL.Folds(sql)); n != 1 {
		t.Errorf("mysql: %d folds, want 1 (the string runs to the end)", n)
	}
}

// TestDialectFormat: the layout keeps a Postgres string that ends in a
// backslash a string, so the clause after it is laid out as a clause.
func TestDialectFormat(t *testing.T) {
	got := Postgres.Format(`select a from t where x = 'a\' and y = 1`)
	want := "SELECT a\nFROM t\nWHERE x = 'a\\' AND y = 1"
	if got != want {
		t.Errorf("postgres Format:\n%s\nwant\n%s", got, want)
	}
}
