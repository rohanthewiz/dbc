package sqlsplit

import "strings"

// Two questions about a statement's code that its verbs cannot answer:
// which functions it calls (Calls), and whether an operator appears in it
// (HasOp). "Code" is the text left once comments, strings, quoted
// identifiers and dollar-quoted bodies are passed over, as the dialect
// reads them. db asks both to tell a SELECT that leaves state behind —
// SELECT set_config(…), MySQL's SELECT @v := … — from a plain read
// (N-272).

// codeEnd returns the offset past the comment, string, quoted identifier
// or dollar-quoted body that starts at s[i], as d reads them; i when what
// starts there is code. A MySQL executable comment (/*!…*/) is passed over
// whole, as HasKeyword passes it: its body is code to the server, but
// nothing asked here is looked for in one.
func (d Dialect) codeEnd(s string, i int) int {
	if k, end := d.commentAt(s, i); k != noComment {
		return end
	}
	switch c := s[i]; c {
	case '\'', '"', '`':
		return skipQuoted(s, i, c, d)
	case '$':
		if tag, ok := d.dollarTag(s, i); ok {
			return skipDollarQuoted(s, i, tag)
		}
	}
	return i
}

// Calls returns the names of the functions sql calls, lowercased, in the
// order they appear: every bare word followed by an open paren, blanks and
// comments allowed between. A schema-qualified call is named by its last
// part, so pg_catalog.set_config(…) is set_config. A word right after ':',
// '@' or '$' is a parameter or a variable (:name, @v, $1), never a
// function, and is passed over.
//
// It is lexical, like the rest of the package, so what it lists is a
// superset to look names up in, not an inventory:
//
//	SELECT x FROM t WHERE id IN (1, 2)   → ["in"]   a keyword before a paren
//	PRAGMA table_info(t)                 → ["table_info"]
//	SELECT "set_config"('a', 'b', false) → []       a quoted name is not read
//
// and a function called out of sight — inside a view, a trigger, another
// function's body — is not seen at all.
func (d Dialect) Calls(sql string) []string {
	var out []string
	i := 0
	for i < len(sql) {
		if end := d.codeEnd(sql, i); end > i {
			i = end
			continue
		}
		if !isWordByte(sql[i]) {
			i++
			continue
		}
		// A whole word at once, a $ past its first byte included (a$b is
		// one name on Postgres and MySQL), so its inside is never read as
		// the start of another.
		start := i
		for i < len(sql) && (isWordByte(sql[i]) || sql[i] == '$') {
			i++
		}
		if start > 0 {
			if p := sql[start-1]; p == ':' || p == '@' || p == '$' {
				continue
			}
		}
		if j := codeAt(sql, i, d); j < len(sql) && sql[j] == '(' {
			out = append(out, strings.ToLower(sql[start:i]))
		}
	}
	return out
}

// Word is one bare word of a statement's code, as Words reads it.
type Word struct {
	Text  string // lowercased: a keyword, a name or a number's digits
	Depth int    // the parentheses open around it; 0 is the statement's own level
}

// Words returns the bare words of sql's code in order, each with the depth
// of parentheses it sits at. Comments, strings, quoted identifiers and
// dollar-quoted bodies are passed over as d reads them, as are parameters
// and variables (a word right after ':', '@' or '$': $1, :name, @v). A
// number is a word of its digits, so LIMIT 10 reads as "limit", "10"; what
// stands between two words (an operator, a comma, a dot) is not given.
//
// db asks it about a statement's clauses where a verb is not enough: a
// row-locking FOR UPDATE at any depth, a LIMIT at depth 0 (N-324). Like
// Calls, it is a lexical superset to look things up in, not a parse:
//
//	SELECT a FROM (SELECT b FROM t LIMIT 5) s FOR UPDATE
//	  select:0 a:0 from:0 select:1 b:1 from:1 t:1 limit:1 5:1 s:0 for:0 update:0
//
// A parenthesis inside a string or a comment does not change the depth,
// and a stray ')' never takes it below 0.
func (d Dialect) Words(sql string) []Word {
	var out []Word
	depth := 0
	i := 0
	for i < len(sql) {
		if end := d.codeEnd(sql, i); end > i {
			i = end
			continue
		}
		switch c := sql[i]; {
		case c == '(':
			depth++
			i++
			continue
		case c == ')':
			depth = max(depth-1, 0)
			i++
			continue
		case !isWordByte(c):
			i++
			continue
		}
		// A whole word at once, a $ past its first byte included (a$b is
		// one name on Postgres and MySQL), as Calls reads it.
		start := i
		for i < len(sql) && (isWordByte(sql[i]) || sql[i] == '$') {
			i++
		}
		if start > 0 {
			if p := sql[start-1]; p == ':' || p == '@' || p == '$' {
				continue
			}
		}
		out = append(out, Word{Text: strings.ToLower(sql[start:i]), Depth: depth})
	}
	return out
}

// HasOp reports whether the operator op (":=", "=") appears in sql's code:
// outside strings, quoted identifiers, comments and dollar-quoted bodies,
// as d reads them. It matches text, not tokens — "=" is found inside "<="
// and ":=" too — so a caller asks for the form it means, in a statement
// where the others cannot occur (a PRAGMA has no comparisons).
func (d Dialect) HasOp(sql, op string) bool {
	i := 0
	for i < len(sql) {
		if end := d.codeEnd(sql, i); end > i {
			i = end
			continue
		}
		if strings.HasPrefix(sql[i:], op) {
			return true
		}
		i++
	}
	return false
}
