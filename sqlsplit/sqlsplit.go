// Package sqlsplit splits a SQL buffer into individual statements while
// tracking byte offsets, so the editor can run just the statement under the
// cursor.
//
// The scanner understands the lexical constructs that can legally contain a
// semicolon: single-quoted strings, double-quoted and backquoted identifiers,
// line and (nestable) block comments, and PostgreSQL dollar-quoted bodies.
// Anything else is opaque — it is a splitter, not a parser.
package sqlsplit

import "strings"

// Stmt is one statement found in a buffer.
type Stmt struct {
	Text  string // statement text, trimmed, terminating semicolon removed
	Start int    // byte offset of Text in the buffer
	End   int    // byte offset just past Text in the buffer

	spanStart int // start of the whole chunk, leading comments/blanks included
	spanEnd   int // end of the whole chunk, terminating semicolon included
}

// chunk is a raw semicolon-delimited span, before trimming.
type chunk struct {
	start, end int
	hasCode    bool // saw something outside whitespace and comments
}

// Split returns the runnable statements in sql, in buffer order. Spans that
// hold only whitespace or comments are dropped.
func Split(sql string) []Stmt {
	var out []Stmt
	for _, c := range scan(sql) {
		if !c.hasCode {
			continue
		}
		raw := sql[c.start:c.end]
		// the terminator, when present, is the last non-space byte of the span
		body := strings.TrimRight(raw, " \t\r\n")
		body = strings.TrimSuffix(body, ";")
		lead := len(body) - len(strings.TrimLeft(body, " \t\r\n"))
		text := strings.TrimSpace(body)
		if text == "" {
			continue
		}
		out = append(out, Stmt{
			Text:      text,
			Start:     c.start + lead,
			End:       c.start + lead + len(text),
			spanStart: c.start,
			spanEnd:   c.end,
		})
	}
	return out
}

// IndexAt returns the index in stmts of the statement the cursor sits in,
// where offset is a byte offset into the same buffer stmts came from. A
// cursor in the blank space or comments between two statements belongs to the
// following one; past the last statement it belongs to that last one. It
// returns -1 when stmts is empty.
func IndexAt(stmts []Stmt, offset int) int {
	if len(stmts) == 0 {
		return -1
	}
	for i, s := range stmts {
		if offset <= s.spanEnd {
			return i
		}
	}
	return len(stmts) - 1
}

// FirstKeyword returns the leading keyword of a statement, lowercased, with
// any leading comments, whitespace, and open parens skipped. It returns ""
// when the statement holds no keyword.
func FirstKeyword(sql string) string {
	return wordAt(sql, keywordAt(sql, 0))
}

// StmtVerbs is the shape of a statement's verbs: what it does at the top
// level, and — for a WITH — what each of its CTEs does.
type StmtVerbs struct {
	// Main is the leading keyword of the main statement, lowercased. For a
	// WITH it is the verb after the CTE list (`WITH x AS (…) DELETE …` is a
	// "delete"); when that verb cannot be found it stays "with".
	Main string
	// MainAt is the byte offset in sql where Main starts, so a caller can
	// look for a clause (RETURNING, say) in the main statement alone and not
	// pick it up from a CTE body.
	MainAt int
	// CTEs holds the leading keyword of each CTE body, in order. Only
	// Postgres lets a CTE be a write (`WITH d AS (DELETE … RETURNING *)
	// SELECT …`), and that is what this is for: a SELECT that writes.
	CTEs []string
}

// withMainVerbs are the words that can start the statement a CTE list
// introduces. Any of them at the top level of a WITH, outside the CTE bodies,
// is the main statement: none is a word the CTE list itself uses (AS,
// RECURSIVE, [NOT] MATERIALIZED, Postgres's SEARCH … SET and CYCLE … USING),
// and as reserved words none can be an unquoted CTE or column name.
var withMainVerbs = map[string]bool{
	"select": true, "insert": true, "update": true, "delete": true,
	"merge": true, "values": true, "table": true,
}

// Verbs finds the verbs of a statement: the leading keyword, and for a WITH
// the verb of the main statement past the CTE list and the verb of each CTE
// body. Like the rest of the package it is lexical, not a parse: a WITH is
// walked word by word at paren depth 0, skipping strings, quoted identifiers,
// comments and everything inside parentheses.
//
//	WITH [RECURSIVE] name [(cols)] AS [NOT MATERIALIZED] ( body ) , … main
//	                               │                     │              │
//	                  prev word "as" / "materialized"  '(' → CTE verb   │
//	          first withMainVerbs word at depth 0 ─────────────────────┘
//
// A '(' at depth 0 right after a ')' is neither a column list (which follows
// a name) nor a CTE body (which follows AS): it is a parenthesized main
// statement, `WITH x AS (…) (SELECT …)`, and its verb is read inside it.
func Verbs(sql string) StmtVerbs {
	at := keywordAt(sql, 0)
	kw := wordAt(sql, at)
	if kw != "with" {
		return StmtVerbs{Main: kw, MainAt: at}
	}
	v := StmtVerbs{Main: kw, MainAt: at}
	var (
		depth      int
		prevWord   string // the last depth-0 word, "" once any other token follows it
		afterClose bool   // the last depth-0 token was a ')'
	)
	i := at + len(kw)
	for i < len(sql) {
		c := sql[i]
		switch {
		case isSpace(c):
			i++
			continue // whitespace does not change what came before
		case isLineCommentAt(sql, i):
			i = skipLineComment(sql, i)
			continue
		case isBlockCommentAt(sql, i):
			i = skipBlockComment(sql, i)
			continue
		case c == '\'', c == '"', c == '`':
			i = skipQuoted(sql, i, c)
		case c == '$':
			if tag, ok := dollarTag(sql, i); ok {
				i = skipDollarQuoted(sql, i, tag)
			} else {
				i++
			}
		case c == '(':
			if depth == 0 {
				switch {
				case prevWord == "as" || prevWord == "materialized":
					body := keywordAt(sql, i+1)
					v.CTEs = append(v.CTEs, wordAt(sql, body))
				case afterClose:
					// a parenthesized main statement; a nested WITH in it
					// has its own CTE list, so read it the same way
					inner := Verbs(sql[i:])
					v.Main, v.MainAt = inner.Main, i+inner.MainAt
					v.CTEs = append(v.CTEs, inner.CTEs...)
					return v
				}
			}
			depth++
			i++
		case c == ')':
			depth--
			i++
			if depth == 0 {
				prevWord, afterClose = "", true
				continue
			}
		case isWordByte(c):
			start := i
			for i < len(sql) && isWordByte(sql[i]) {
				i++
			}
			if depth == 0 {
				w := strings.ToLower(sql[start:i])
				if withMainVerbs[w] {
					v.Main, v.MainAt = w, start
					return v
				}
				prevWord, afterClose = w, false
				continue
			}
		default:
			i++
		}
		if depth == 0 {
			prevWord, afterClose = "", false
		}
	}
	return v
}

// keywordAt returns the offset of the first keyword at or after i, skipping
// whitespace, comments and open parens as FirstKeyword does; len(sql) when
// there is none.
func keywordAt(sql string, i int) int {
	for i < len(sql) {
		switch {
		case isSpace(sql[i]), sql[i] == '(':
			i++
		case isLineCommentAt(sql, i):
			i = skipLineComment(sql, i)
		case isBlockCommentAt(sql, i):
			i = skipBlockComment(sql, i)
		default:
			return i
		}
	}
	return len(sql)
}

// wordAt returns the word starting at i, lowercased ("" when there is none).
func wordAt(sql string, i int) string {
	end := i
	for end < len(sql) && isWordByte(sql[end]) {
		end++
	}
	return strings.ToLower(sql[i:end])
}

// HasKeyword reports whether word (lowercase) appears as a bare keyword in
// sql: a whole word, in any case, outside strings, quoted identifiers,
// comments, and dollar-quoted bodies. So `RETURNING id` counts, while
// 'returning' in a string literal, "returning" as a quoted column name, or
// -- returning in a comment does not.
//
// Words that are plainly not keywords are skipped too: one right after a
// '.' is part of a qualified name (t.returning), and one right after ':' or
// '@' is a named parameter or variable (:returning, @returning). Past that it
// is lexical like the rest of the package — a keyword used unquoted as an
// identifier would still count, which the dialects dbc speaks mostly forbid
// for reserved words anyway.
func HasKeyword(sql, word string) bool {
	i := 0
	for i < len(sql) {
		c := sql[i]
		switch {
		case isLineCommentAt(sql, i):
			i = skipLineComment(sql, i)
		case isBlockCommentAt(sql, i):
			i = skipBlockComment(sql, i)
		case c == '\'', c == '"', c == '`':
			i = skipQuoted(sql, i, c)
		case c == '$':
			if tag, ok := dollarTag(sql, i); ok {
				i = skipDollarQuoted(sql, i, tag)
				continue
			}
			// a placeholder such as $1: step past its digits so they are
			// not read as the start of a word
			i++
			for i < len(sql) && isDigit(sql[i]) {
				i++
			}
		case isWordByte(c):
			start := i
			for i < len(sql) && isWordByte(sql[i]) {
				i++
			}
			if start > 0 {
				if p := sql[start-1]; p == '.' || p == ':' || p == '@' {
					continue
				}
			}
			if i-start == len(word) && strings.EqualFold(sql[start:i], word) {
				return true
			}
		default:
			i++
		}
	}
	return false
}

func scan(sql string) []chunk {
	var (
		out     []chunk
		start   int
		hasCode bool
		i       int
	)
	for i < len(sql) {
		c := sql[i]
		switch {
		case isLineCommentAt(sql, i):
			i = skipLineComment(sql, i)
		case isBlockCommentAt(sql, i):
			i = skipBlockComment(sql, i)
		case c == '\'', c == '"', c == '`':
			i = skipQuoted(sql, i, c)
			hasCode = true
		case c == '$':
			if tag, ok := dollarTag(sql, i); ok {
				i = skipDollarQuoted(sql, i, tag)
			} else {
				i++ // a placeholder such as $1
			}
			hasCode = true
		case c == ';':
			i++
			out = append(out, chunk{start: start, end: i, hasCode: hasCode})
			start, hasCode = i, false
		default:
			if !isSpace(c) {
				hasCode = true
			}
			i++
		}
	}
	if start < len(sql) {
		out = append(out, chunk{start: start, end: len(sql), hasCode: hasCode})
	}
	return out
}

func isLineCommentAt(s string, i int) bool {
	return s[i] == '-' && i+1 < len(s) && s[i+1] == '-'
}

func isBlockCommentAt(s string, i int) bool {
	return s[i] == '/' && i+1 < len(s) && s[i+1] == '*'
}

func skipLineComment(s string, i int) int {
	if nl := strings.IndexByte(s[i:], '\n'); nl >= 0 {
		return i + nl + 1
	}
	return len(s)
}

// skipBlockComment handles nesting, which PostgreSQL allows.
func skipBlockComment(s string, i int) int {
	depth := 0
	for i < len(s) {
		switch {
		case isBlockCommentAt(s, i):
			depth++
			i += 2
		case s[i] == '*' && i+1 < len(s) && s[i+1] == '/':
			i += 2
			if depth--; depth == 0 {
				return i
			}
		default:
			i++
		}
	}
	return len(s) // unterminated
}

// skipQuoted consumes a quoted run starting at the opening quote. A doubled
// quote is an embedded quote in every dialect we speak; a backslash escape is
// MySQL's default and is honored inside single quotes, which costs us only
// PostgreSQL strings that end in a literal backslash.
func skipQuoted(s string, i int, q byte) int {
	for i++; i < len(s); i++ {
		switch {
		case s[i] == '\\' && q == '\'' && i+1 < len(s):
			i++
		case s[i] == q:
			if i+1 < len(s) && s[i+1] == q {
				i++
				continue
			}
			return i + 1
		}
	}
	return len(s) // unterminated
}

// dollarTag reports whether a PostgreSQL dollar-quote tag ($$ or $tag$) opens
// at i, as opposed to a positional parameter such as $1.
func dollarTag(s string, i int) (string, bool) {
	j := i + 1
	if j < len(s) && isDigit(s[j]) {
		return "", false // $1, $2, …
	}
	for j < len(s) && isWordByte(s[j]) {
		j++
	}
	if j < len(s) && s[j] == '$' {
		return s[i : j+1], true
	}
	return "", false
}

func skipDollarQuoted(s string, i int, tag string) int {
	body := i + len(tag)
	if end := strings.Index(s[body:], tag); end >= 0 {
		return body + end + len(tag)
	}
	return len(s) // unterminated
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\v' || c == '\f'
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isWordByte(c byte) bool {
	return c == '_' || isDigit(c) ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
