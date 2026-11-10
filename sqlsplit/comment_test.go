package sqlsplit

import (
	"strings"
	"testing"
)

// TestCommentSplit is N-247: what a comment is depends on the engine, and
// a scan that misses one the server skips cuts a statement at a ; inside
// it. The first case is the item's repro — on MySQL the piece before the
// cut, `DELETE FROM t # old`, deleted every row.
func TestCommentSplit(t *testing.T) {
	cases := []struct {
		name string
		d    Dialect
		sql  string
		want []string
	}{
		// # runs to the end of the line on MySQL, and only there
		{"mysql hash hides a semicolon", MySQL, "DELETE FROM t # old; cleanup\nWHERE id = 1;",
			[]string{"DELETE FROM t # old; cleanup\nWHERE id = 1"}},
		{"mysql hash hides an apostrophe", MySQL, "SELECT 1; # don't\nSELECT 2;\nSELECT 3;",
			[]string{"SELECT 1", "SELECT 2", "SELECT 3"}},
		{"mysql hash heads a statement", MySQL, "SELECT 1;\n# the next one\nSELECT 2;",
			[]string{"SELECT 1", "# the next one\nSELECT 2"}},
		{"mysql hash-only chunk is dropped", MySQL, "SELECT 1;\n# SELECT 2;\n",
			[]string{"SELECT 1"}},
		{"mysql hash in a string or name", MySQL, "SELECT '#;', `a#b`; SELECT 2",
			[]string{"SELECT '#;', `a#b`", "SELECT 2"}},
		// ...and is Postgres's XOR and JSON path operator, so it is code
		// on every other engine, Generic included
		{"postgres hash is an operator", Postgres, "SELECT 1 # 2; SELECT j #> '{a}' FROM t",
			[]string{"SELECT 1 # 2", "SELECT j #> '{a}' FROM t"}},
		{"generic hash is an operator", Generic, "SELECT 1 # 2; SELECT 3",
			[]string{"SELECT 1 # 2", "SELECT 3"}},
		{"sqlite hash is not a comment", SQLite, "SELECT 1 # x; SELECT 2",
			[]string{"SELECT 1 # x", "SELECT 2"}},

		// /* */ nests on Postgres (and Generic) alone: elsewhere the first
		// */ closes it, so the DELETE is code — not the rest of the buffer
		// read as a comment left open
		{"mysql does not nest", MySQL, "/* see /* note */ DELETE FROM t WHERE a = 1; SELECT 2;",
			[]string{"/* see /* note */ DELETE FROM t WHERE a = 1", "SELECT 2"}},
		{"sqlite does not nest", SQLite, "/* see /* note */ DELETE FROM t WHERE a = 1; SELECT 2;",
			[]string{"/* see /* note */ DELETE FROM t WHERE a = 1", "SELECT 2"}},
		{"bytdb does not nest", Bytdb, "/* see /* note */ DELETE FROM t WHERE a = 1; SELECT 2;",
			[]string{"/* see /* note */ DELETE FROM t WHERE a = 1", "SELECT 2"}},
		{"postgres nests", Postgres, "/* see /* note */ DELETE FROM t WHERE a = 1; SELECT 2;",
			nil},
		{"postgres nested comment closes", Postgres, "/* a /* b */ c */ SELECT 1; SELECT 2",
			[]string{"/* a /* b */ c */ SELECT 1", "SELECT 2"}},

		// MySQL's executable comments are code: mysqldump's session
		// settings are statements of nothing else
		{"mysql executable comment is a statement", MySQL,
			"/*!40101 SET NAMES utf8mb4 */;\n/*!40014 SET FOREIGN_KEY_CHECKS=0 */;\nSELECT 1;",
			[]string{"/*!40101 SET NAMES utf8mb4 */", "/*!40014 SET FOREIGN_KEY_CHECKS=0 */", "SELECT 1"}},
		{"mysql executable comment with no version", MySQL, "/*! SET @a = 1 */; SELECT 2",
			[]string{"/*! SET @a = 1 */", "SELECT 2"}},
		{"mariadb executable comment", MySQL, "/*M!100616 SET NOTE_VERBOSITY=0 */;\nSELECT 1;",
			[]string{"/*M!100616 SET NOTE_VERBOSITY=0 */", "SELECT 1"}},
		// a ; inside one does not end its statement (dumps put trigger
		// bodies there), nor does a */ inside one of its strings close it
		{"mysql executable comment holds a semicolon", MySQL,
			"/*!50003 CREATE TRIGGER x BEFORE INSERT ON t FOR EACH ROW BEGIN SET NEW.a = '*/;'; END */; SELECT 2",
			[]string{"/*!50003 CREATE TRIGGER x BEFORE INSERT ON t FOR EACH ROW BEGIN SET NEW.a = '*/;'; END */", "SELECT 2"}},
		// on every other engine it is an ordinary comment, so a statement
		// of nothing else is no statement
		{"postgres executable comment is a comment", Postgres, "/*!40101 SET NAMES utf8mb4 */;\nSELECT 1;",
			[]string{"SELECT 1"}},
		{"generic executable comment is a comment", Generic, "/*!40101 SET NAMES utf8mb4 */;\nSELECT 1;",
			[]string{"SELECT 1"}},
		// an optimizer hint is a comment to the scanner: the server reads
		// one only after a statement's verb, where it changes nothing
		{"mysql hint", MySQL, "SELECT /*+ MAX_EXECUTION_TIME(1000) */ * FROM t; SELECT 2",
			[]string{"SELECT /*+ MAX_EXECUTION_TIME(1000) */ * FROM t", "SELECT 2"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			eq(t, texts(c.d.Split(c.sql)), c.want)
		})
	}
}

// TestCommentStatementParts: a # comment is a heading comment and a
// trailing remark on MySQL as -- is everywhere, and an executable comment
// is code — the statement's CodeStart, never its remark or its header.
func TestCommentStatementParts(t *testing.T) {
	sql := "# header\nSELECT 1; # remark\nSELECT 2;"
	stmts := MySQL.Split(sql)
	if len(stmts) != 2 {
		t.Fatalf("MySQL.Split(%q) = %d statements, want 2", sql, len(stmts))
	}
	if got := sql[stmts[0].CodeStart:stmts[0].End]; got != "SELECT 1" {
		t.Errorf("first statement's code = %q, want SELECT 1 (the # line heads it)", got)
	}
	// a caret in the remark belongs to the statement it follows
	if i := IndexAt(stmts, strings.Index(sql, "remark")); i != 0 {
		t.Errorf("caret in the # remark picks statement %d, want 0", i)
	}

	sql = "SELECT 1; /*!40101 SET NAMES utf8mb4 */;"
	stmts = MySQL.Split(sql)
	eq(t, texts(stmts), []string{"SELECT 1", "/*!40101 SET NAMES utf8mb4 */"})
	if len(stmts) == 2 && stmts[1].CodeStart != stmts[1].Start {
		t.Errorf("executable comment: CodeStart %d, Start %d — it is code, not a heading comment",
			stmts[1].CodeStart, stmts[1].Start)
	}
}

// TestCommentVerbs is the run's classification (db.isQuery reads Verbs): a
// SELECT under a # comment is a query on MySQL, so its rows are shown, not
// dropped by an Exec.
func TestCommentVerbs(t *testing.T) {
	for _, c := range []struct {
		d    Dialect
		sql  string
		main string
	}{
		{MySQL, "# note\nSELECT 1", "select"},
		{MySQL, "WITH a AS (SELECT 1) # note\nDELETE FROM t", "delete"},
		{SQLite, "/* a /* b */ SELECT 1", "select"},
		{Postgres, "/* a /* b */ c */ SELECT 1", "select"},
		{MySQL, "/*!40101 SET NAMES utf8mb4 */", ""}, // code, but no word
	} {
		if got := c.d.Verbs(c.sql).Main; got != c.main {
			t.Errorf("%v.Verbs(%q).Main = %q, want %q", c.d, c.sql, got, c.main)
		}
	}
	if MySQL.HasKeyword("INSERT INTO t VALUES (1) # returning", "returning") {
		t.Errorf("mysql: RETURNING found in a # comment")
	}
	if !Postgres.HasKeyword("DELETE FROM t WHERE a # 1 = 0 RETURNING id", "returning") {
		t.Errorf("postgres: RETURNING after the # operator not found")
	}
}

// TestCommentLexAgreesWithSplit is the editor's promise for comments: what
// it draws as a comment is what the run skips as one, per engine.
func TestCommentLexAgreesWithSplit(t *testing.T) {
	comments := func(d Dialect, sql string) []string {
		var out []string
		for _, tk := range d.Lex(sql) {
			if tk.Kind == TokComment {
				out = append(out, sql[tk.Start:tk.End])
			}
		}
		return out
	}
	hash := "SELECT 1 # x;\nSELECT 2"
	eq(t, comments(MySQL, hash), []string{"# x;"})
	eq(t, comments(Postgres, hash), nil)

	nested := "/* a /* b */ SELECT 1"
	eq(t, comments(MySQL, nested), []string{"/* a /* b */"})
	eq(t, comments(Postgres, nested), []string{nested})

	// one span, as Split takes it whole
	exec := "/*!50003 SET a = 1; SET b = 2 */; SELECT 3"
	eq(t, comments(MySQL, exec), []string{"/*!50003 SET a = 1; SET b = 2 */"})
}

// TestCommentFolds: a statement after a # comment folds on its own on
// MySQL, as Split runs it on its own.
func TestCommentFolds(t *testing.T) {
	sql := "SELECT a # one;\ntwo\nFROM t;\nSELECT b\nFROM u;"
	var starts []string
	for _, f := range MySQL.Folds(sql) {
		starts = append(starts, sql[f.Start:f.Start+8])
	}
	// cut at the ; in the comment, "SELECT a # one" would be a statement
	// of one line, with no fold, and the next would fold from "two"
	eq(t, starts, []string{"SELECT a", "SELECT b"})
}

// TestCommentFormat: the formatter keeps a # comment ending its line on
// MySQL (joining FROM onto it would comment the FROM out), and lays out an
// executable comment as the code it is, not as the remark of the
// statement before it.
func TestCommentFormat(t *testing.T) {
	for _, c := range []struct {
		d         Dialect
		sql, want string
	}{
		{MySQL, "select a # note\nfrom t", "SELECT a # note\nFROM t"},
		// # is an operator on Postgres, spaced as one
		{Postgres, "select a#b from t", "SELECT a # b\nFROM t"},
		{MySQL, "select 1; /*!40101 SET NAMES utf8mb4 */;", "SELECT 1;\n\n/*!40101 SET NAMES utf8mb4 */;"},
		{SQLite, "select /* a /* b */ 1", "SELECT /* a /* b */ 1"},
	} {
		if got := c.d.Format(c.sql); got != c.want {
			t.Errorf("%v.Format(%q):\n%s\nwant\n%s", c.d, c.sql, got, c.want)
		}
	}
}
