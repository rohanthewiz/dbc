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
	i := 0
	for i < len(sql) {
		switch {
		case isSpace(sql[i]), sql[i] == '(':
			i++
		case isLineCommentAt(sql, i):
			i = skipLineComment(sql, i)
		case isBlockCommentAt(sql, i):
			i = skipBlockComment(sql, i)
		default:
			start := i
			for i < len(sql) && isWordByte(sql[i]) {
				i++
			}
			return strings.ToLower(sql[start:i])
		}
	}
	return ""
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
