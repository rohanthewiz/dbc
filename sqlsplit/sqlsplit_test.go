package sqlsplit

import "testing"

func texts(stmts []Stmt) []string {
	out := make([]string, len(stmts))
	for i, s := range stmts {
		out[i] = s.Text
	}
	return out
}

func eq(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d statements %q, want %d %q", len(got), got, len(want), want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("statement %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSplit(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want []string
	}{
		{"empty", "   \n\t ", nil},
		{"single", "SELECT 1", []string{"SELECT 1"}},
		{"single terminated", "SELECT 1;", []string{"SELECT 1"}},
		{"trailing blank span", "SELECT 1;\n\n", []string{"SELECT 1"}},
		{"two", "SELECT 1;\nSELECT 2;", []string{"SELECT 1", "SELECT 2"}},
		{"empty spans dropped", "SELECT 1;;;\nSELECT 2", []string{"SELECT 1", "SELECT 2"}},
		{"semicolon in string", "SELECT 'a;b';SELECT 2", []string{"SELECT 'a;b'", "SELECT 2"}},
		{"doubled quote", "SELECT 'it''s; here';SELECT 2", []string{"SELECT 'it''s; here'", "SELECT 2"}},
		{"backslash quote", `SELECT 'it\'s; here';SELECT 2`, []string{`SELECT 'it\'s; here'`, "SELECT 2"}},
		{"quoted ident", `SELECT "a;b" FROM t;SELECT 2`, []string{`SELECT "a;b" FROM t`, "SELECT 2"}},
		{"backquoted ident", "SELECT `a;b` FROM t;SELECT 2", []string{"SELECT `a;b` FROM t", "SELECT 2"}},
		// a comment after the terminator belongs to the following statement
		{"line comment", "SELECT 1; -- drop; this\nSELECT 2", []string{"SELECT 1", "-- drop; this\nSELECT 2"}},
		{"comment-only span kept with stmt", "-- note; here\nSELECT 1", []string{"-- note; here\nSELECT 1"}},
		{"block comment", "SELECT 1; /* a; b */ SELECT 2", []string{"SELECT 1", "/* a; b */ SELECT 2"}},
		{"nested block comment", "SELECT 1 /* a /* b; */ c */ + 1;SELECT 2",
			[]string{"SELECT 1 /* a /* b; */ c */ + 1", "SELECT 2"}},
		{"dollar quoted", "CREATE FUNCTION f() RETURNS int AS $$ BEGIN; RETURN 1; END; $$ LANGUAGE plpgsql;SELECT 2",
			[]string{"CREATE FUNCTION f() RETURNS int AS $$ BEGIN; RETURN 1; END; $$ LANGUAGE plpgsql", "SELECT 2"}},
		{"tagged dollar quote", "DO $body$ SELECT 1; $body$;SELECT 2",
			[]string{"DO $body$ SELECT 1; $body$", "SELECT 2"}},
		{"placeholders are not tags", "SELECT * FROM t WHERE a=$1 AND b=$2;SELECT 2",
			[]string{"SELECT * FROM t WHERE a=$1 AND b=$2", "SELECT 2"}},
		{"unterminated string", "SELECT 'oops", []string{"SELECT 'oops"}},
		{"unterminated block comment", "SELECT 1;/* oops", []string{"SELECT 1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			eq(t, texts(Split(c.sql)), c.want)
		})
	}
}

func TestSplitOffsets(t *testing.T) {
	sql := "\n  SELECT 1;\n\nSELECT 2 ;\n"
	stmts := Split(sql)
	if len(stmts) != 2 {
		t.Fatalf("got %d statements, want 2", len(stmts))
	}
	for _, s := range stmts {
		if got := sql[s.Start:s.End]; got != s.Text {
			t.Errorf("offsets [%d:%d] give %q, want %q", s.Start, s.End, got, s.Text)
		}
	}
}

func TestIndexAt(t *testing.T) {
	//               0         1         2
	//               0123456789012345678901234
	const sql = "SELECT 1;\nSELECT 2;\nSELECT 3"
	stmts := Split(sql)
	if len(stmts) != 3 {
		t.Fatalf("got %d statements, want 3", len(stmts))
	}
	cases := []struct {
		offset int
		want   int
	}{
		{0, 0},              // start of buffer
		{5, 0},              // inside the first
		{9, 0},              // just past the first semicolon
		{10, 1},             // start of the second
		{19, 1},             // just past the second semicolon
		{20, 2},             // start of the third
		{len(sql), 2},       // end of buffer
		{len(sql) + 100, 2}, // beyond the buffer
	}
	for _, c := range cases {
		if got := IndexAt(stmts, c.offset); got != c.want {
			t.Errorf("IndexAt(%d) = %d, want %d", c.offset, got, c.want)
		}
	}
	if got := IndexAt(nil, 0); got != -1 {
		t.Errorf("IndexAt(nil) = %d, want -1", got)
	}
}

func TestIndexAtSkipsCommentOnlySpans(t *testing.T) {
	const sql = "-- just a note;\nSELECT 1"
	stmts := Split(sql)
	if len(stmts) != 1 {
		t.Fatalf("got %d statements, want 1", len(stmts))
	}
	if got := IndexAt(stmts, 3); got != 0 { // cursor inside the comment span
		t.Errorf("IndexAt = %d, want 0", got)
	}
}

func TestFirstKeyword(t *testing.T) {
	cases := map[string]string{
		"SELECT 1":                    "select",
		"  \n\tselect 1":              "select",
		"-- note\nSELECT 1":           "select",
		"/* note */ WITH x AS (...)":  "with",
		"/* a /* b */ c */ explain 1": "explain",
		"(SELECT 1) UNION (SELECT 2)": "select",
		"INSERT INTO t VALUES (1)":    "insert",
		"":                            "",
		"-- only a comment":           "",
	}
	for in, want := range cases {
		if got := FirstKeyword(in); got != want {
			t.Errorf("FirstKeyword(%q) = %q, want %q", in, got, want)
		}
	}
}
