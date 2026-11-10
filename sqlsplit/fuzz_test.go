package sqlsplit

import (
	"strings"
	"testing"
)

// FuzzSplit holds the scanner to the properties every caller leans on,
// for every dialect (N-302). Run briefly by `go test` (the seeds alone),
// and at length by hand:
//
//	go test -run '^$' -fuzz '^FuzzSplit$' -fuzztime 5m ./sqlsplit
//
// The properties:
//
//   - nothing panics: Split, Lex, Folds, Verbs, HasKeyword, Format
//   - statements are in buffer order and do not overlap; each Text is the
//     trimmed, non-empty slice sql[Start:End], CodeStart inside it
//   - IndexAt maps a caret at a statement's end back to that statement
//   - Lex's tokens are in order, do not overlap, and stay in the buffer;
//     folds stay in the buffer
//   - Lex agrees with Split: no statement's code starts strictly inside a
//     string, quoted name or comment the editor draws — the promise that
//     what runs is what is on screen. (A buffer with a MySQL DELIMITER
//     command is left out of this one: a delimiter may be any text, ' or
//     -- included, and the editor's coloring does not follow it.)
//
// The seeds are the inputs each scanner fix was written for: the
// dialects' strings (N-238), comments (N-247), bodies and DELIMITER
// (N-248), the BOM (N-249) and dollar quotes (N-306).
func FuzzSplit(f *testing.F) {
	for _, s := range []string{
		"SELECT 1; SELECT 2",
		"SELECT 'a\\';\nSELECT 2",
		"SELECT E'it\\'s; here';SELECT 2",
		"DELETE FROM t # old; cleanup\nWHERE id = 1;",
		"SELECT 1; # don't\nSELECT 2;",
		"/* see /* note */ DELETE FROM t WHERE a = 1; SELECT 2;",
		"/*!40101 SET NAMES utf8mb4 */;\n/*M!100616 SET x=0 */;",
		"CREATE TRIGGER t AFTER DELETE ON o BEGIN\n  DELETE FROM i WHERE oid = old.id;\n  UPDATE s SET n = CASE WHEN n > 0 THEN n - 1 END;\nEND;\nSELECT 1;",
		"CREATE PROCEDURE p() BEGIN IF begin > 0 THEN SELECT 1; END IF; CASE n WHEN 1 THEN SELECT 1; END CASE; lbl: BEGIN END lbl; END; SELECT 3",
		"CREATE FUNCTION f(a int) RETURNS int LANGUAGE sql BEGIN ATOMIC INSERT INTO t VALUES (a); END;",
		"CREATE RULE r AS ON INSERT TO t DO ALSO (INSERT INTO a VALUES (1); INSERT INTO b VALUES (2));",
		"DELIMITER //\nCREATE PROCEDURE p()\nBEGIN\n  SELECT 1;\nEND //\nDELIMITER ;\nSELECT 2;",
		"delimiter $$\nSELECT 1$$\ndelimiter ;",
		"\uFEFFSELECT 1;\r\nSELECT 2;",
		"SELECT a$b$c FROM t; SELECT $$x; y$$; SELECT $q$z$q$",
		"-- head\n\nSELECT 1; -- remark\n/* block\n*/ SELECT 2",
		"WITH a AS (DELETE FROM t RETURNING *) SELECT * FROM a",
		"'open", "/* open", "$$open", "\"open", "--", "#", "/*!", ";;;", "",
	} {
		for d := range Bytdb + 1 {
			f.Add(s, uint8(d))
		}
	}
	f.Fuzz(func(t *testing.T, sql string, dn uint8) {
		d := Dialect(dn % uint8(Bytdb+1))
		stmts := d.Split(sql)
		prevEnd := 0
		for i, s := range stmts {
			if s.Start < prevEnd || s.Start > s.CodeStart || s.CodeStart >= s.End || s.End > len(sql) {
				t.Fatalf("%v: statement %d spans Start %d CodeStart %d End %d (previous End %d, len %d)",
					d, i, s.Start, s.CodeStart, s.End, prevEnd, len(sql))
			}
			if s.Text != sql[s.Start:s.End] || s.Text == "" || strings.TrimSpace(s.Text) != s.Text {
				t.Fatalf("%v: statement %d Text %q is not the trimmed slice %q", d, i, s.Text, sql[s.Start:s.End])
			}
			// a caret at End is inside the statement's own span (a caret
			// at its code's first byte may not be: right after a ; with
			// no blank, it belongs to the statement the ; ended)
			if got := IndexAt(stmts, s.End); got != i {
				t.Fatalf("%v: IndexAt(statement %d's End) = %d", d, i, got)
			}
			prevEnd = s.End
		}

		toks := d.Lex(sql)
		prev := 0
		for _, tk := range toks {
			if tk.Start < prev || tk.End <= tk.Start || tk.End > len(sql) {
				t.Fatalf("%v: token %+v out of order or bounds (previous end %d, len %d)", d, tk, prev, len(sql))
			}
			prev = tk.End
		}
		for _, fd := range d.Folds(sql) {
			if fd.Start < 0 || fd.End <= fd.Start || fd.End > len(sql) {
				t.Fatalf("%v: fold %+v out of bounds (len %d)", d, fd, len(sql))
			}
		}

		if !hasDelimiterCmd(sql, d) {
			k := 0
			for i, s := range stmts {
				for k < len(toks) && toks[k].End <= s.CodeStart {
					k++
				}
				if k < len(toks) {
					tk := toks[k]
					opaque := tk.Kind == TokString || tk.Kind == TokIdent || tk.Kind == TokComment
					if opaque && tk.Start < s.CodeStart {
						t.Fatalf("%v: statement %d's code starts at %d, inside the %v token %q drawn at %d…%d",
							d, i, s.CodeStart, tk.Kind, sql[tk.Start:tk.End], tk.Start, tk.End)
					}
				}
			}
		}

		for _, s := range stmts {
			d.Verbs(s.Text)
			d.HasKeyword(s.Text, "returning")
		}
		d.Format(sql)
	})
}

// hasDelimiterCmd reports whether d's scan of sql meets a DELIMITER
// command.
func hasDelimiterCmd(sql string, d Dialect) bool {
	for _, c := range scan(sql, d) {
		if c.cmd {
			return true
		}
	}
	return false
}
