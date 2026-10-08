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
//
// Comments on their own lines above a statement head it: they are part of
// Text (they run with it, and a caret in them picks it), but CodeStart skips
// them, so an editor's marker can sit beside the code alone.
//
//	SELECT 1; -- note⏎
//	---⏎                 ← Start: Text begins with the header comment
//	SELECT 2;            ← CodeStart: the first byte outside blanks/comments
//
// Only the comments touching the statement head it, though: a blank line
// detaches everything above it. That is how a commented-out statement or a
// note left between queries reads, and running it along with the query
// would put it in the run's log and history as if it belonged there. A
// caret in a detached comment still picks the statement below (IndexAt goes
// by the whole span), as a caret on a blank line does.
//
//	-- CREATE SCHEMA s;⏎  ┐ detached: in the span, not in Text
//	---⏎                  ┘
//	⏎                     ← the blank line that detaches them
//	-- all jobs⏎          ← Start: the comment block touching the code
//	SELECT * FROM jobs;   ← CodeStart
type Stmt struct {
	Text      string // statement text, trimmed, terminating semicolon removed
	Start     int    // byte offset of Text in the buffer
	End       int    // byte offset just past Text in the buffer
	CodeStart int    // byte offset of Text's first code, past any heading comments; Start when there are none

	spanStart int // start of the whole chunk, leading comments/blanks included
	spanEnd   int // end of the whole chunk: its semicolon and that line's trailing remark included
}

// chunk is a raw semicolon-delimited span, before trimming.
//
// end and tail differ only when the semicolon is followed, on its own line,
// by nothing but blanks and comments ("SELECT 1; -- ran at 15:29"). That rest
// of the line is the ended statement's remark: end stays just past the
// semicolon so the statement's text is cut there, while tail runs to the end
// of the line so a caret in the remark still belongs to this statement — and
// the next chunk starts after it rather than claiming it as a header.
//
//	SELECT 1; -- note⏎---⏎SELECT 2;
//	└──────┘                          chunk 1's text
//	└───────┘                         start … end
//	└───────────────┘                 start … tail (what IndexAt matches on)
//	                 └────────────┘   chunk 2, from the newline: "---" heads it
type chunk struct {
	start, end int
	tail       int  // end of the span: end, or past a same-line trailing remark
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
		start := c.start + lead
		end := start + len(text)
		// the chunk has code (hasCode), so codeAt stops inside Text; it
		// starts on a token boundary (only blanks were skipped to reach
		// Start), so it cannot mistake a comment's inside for code
		code := codeAt(sql, start)
		// drop heading comments a blank line cuts off from the code; start
		// only moves forward, onto a comment or the code itself, so Text
		// stays a trimmed slice of the buffer
		start = headStart(sql, start, code)
		out = append(out, Stmt{
			Text:      sql[start:end],
			Start:     start,
			End:       end,
			CodeStart: code,
			spanStart: c.start,
			spanEnd:   c.tail,
		})
	}
	return out
}

// IndexAt returns the index in stmts of the statement the cursor sits in,
// where offset is a byte offset into the same buffer stmts came from. A
// cursor after a statement's semicolon but still on its line, among nothing
// but blanks and comments, belongs to that statement; one in the blank space
// or comments on the lines between two statements belongs to the following
// one; past the last statement it belongs to that last one. It returns -1
// when stmts is empty.
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

// ddlVerbs are the leading keywords of the statements IsDDL counts as data
// definition: the ones that change what the database holds as structure, or
// who may use it, rather than the rows in it.
//
// The engines do not agree on the edges, so this is their union:
//
//	CREATE ALTER DROP RENAME COMMENT   DDL everywhere it exists
//	TRUNCATE                           DDL to MySQL (it commits implicitly
//	                                   and cannot be rolled back there);
//	                                   Postgres logs it as data modification
//	GRANT REVOKE                       strictly DCL, but Postgres's
//	                                   log_statement = 'ddl' logs them too
//
// A wider net suits the one caller, a log of schema changes: a TRUNCATE or
// a REVOKE missing from it is worse than one line too many.
var ddlVerbs = map[string]bool{
	"create": true, "alter": true, "drop": true, "rename": true, "comment": true,
	"truncate": true, "grant": true, "revoke": true,
}

// IsDDL reports whether stmt is a data definition statement, by its leading
// keyword (ddlVerbs). It is lexical like the rest of the package, so DDL
// that runs out of sight of the statement's first word — inside a function
// body, a Postgres DO block, an EXECUTE of a string — is not seen.
func IsDDL(stmt string) bool {
	return ddlVerbs[FirstKeyword(stmt)]
}

// catalogVerbs are the leading keywords of the statements ChangesCatalog
// counts: the ones that may change what a catalog read lists — the
// tables, their columns and comments, the schemas (SQLite's ATTACH and
// DETACH add and remove one).
//
// It is not ddlVerbs, which answers another question (what belongs in a
// log of schema changes), and the two differ on purpose at the edges:
//
//	              IsDDL  ChangesCatalog
//	TRUNCATE        ✓         ·          rows go, the table stays
//	GRANT REVOKE    ✓         ·          who may use it, not what exists
//	ATTACH DETACH   ·         ✓          a whole schema comes or goes
var catalogVerbs = map[string]bool{
	"create": true, "alter": true, "drop": true, "rename": true, "comment": true,
	"attach": true, "detach": true,
}

// ChangesCatalog reports whether stmt may change what the database's
// catalog lists, by its leading keyword (catalogVerbs): what a cached
// schema (completion's, a sidebar's table list) is stale after. It is
// lexical as IsDDL is, with the same blind spots: DDL inside a function
// body, a DO block or an EXECUTE is not seen.
func ChangesCatalog(stmt string) bool {
	return catalogVerbs[FirstKeyword(stmt)]
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

// codeAt returns the offset of the first byte at or after i that is neither
// whitespace nor inside a comment; len(sql) when there is none. Unlike
// keywordAt it stops at an open paren, which is code: a statement such as
// `(SELECT 1) UNION (SELECT 2)` starts there.
func codeAt(sql string, i int) int {
	for i < len(sql) {
		switch {
		case isSpace(sql[i]):
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

// headStart returns where a statement's heading comments begin, given that
// sql[i:code] holds only blanks and comments, i is on a non-blank, and code
// is the statement's first code: the first comment after the last blank line
// in that run, or code when a blank line sits right above it. A blank line
// is one holding only whitespace outside any comment, so an empty line
// inside a block comment does not detach anything.
//
//	-- a⏎        i (on a non-blank, so its line is never blank)
//	⏎            blank: cut
//	-- b⏎        first comment after a cut → head
//	SELECT 1     code
//
// The walk tracks whether the line so far is empty. A line comment eats its
// own newline, so the line after it starts empty; a block comment leaves
// the walk on the line it closes on, which then holds content.
func headStart(sql string, i, code int) int {
	head := i
	empty := false // the current line holds nothing but whitespace so far
	cut := false   // a blank line has passed since the last comment
	for i < code {
		switch {
		case sql[i] == '\n':
			if empty {
				cut = true
			}
			empty = true
			i++
		case isSpace(sql[i]):
			i++
		case isLineCommentAt(sql, i), isBlockCommentAt(sql, i):
			if cut {
				head, cut = i, false
			}
			if isLineCommentAt(sql, i) {
				i, empty = skipLineComment(sql, i), true
			} else {
				i, empty = skipBlockComment(sql, i), false
			}
		default:
			return head // not reached: only blanks and comments precede code
		}
	}
	if cut {
		return code
	}
	return head
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
			tail := remarkEnd(sql, i)
			out = append(out, chunk{start: start, end: i, tail: tail, hasCode: hasCode})
			// the remark holds no semicolon that could end a statement (its
			// comments were skipped whole), so the scan resumes past it
			start, hasCode, i = tail, false, tail
		default:
			if !isSpace(c) {
				hasCode = true
			}
			i++
		}
	}
	if start < len(sql) {
		out = append(out, chunk{start: start, end: len(sql), tail: len(sql), hasCode: hasCode})
	}
	return out
}

// remarkEnd returns where the trailing remark after a semicolon ends, where i
// is just past the semicolon: the end of the line (before its newline, so a
// caret at the start of the next line is not in it) when the rest of the line
// holds only blanks and comments, else i itself — code on the same line
// starts the next statement there, and any comment before that code is the
// next statement's, as it was before.
//
// A block comment that opens on the line counts even when it closes on a
// later one: it started as a remark on this statement, and what decides is
// the line it closes on. The end of the buffer ends the line too, which
// covers an unterminated comment.
func remarkEnd(s string, i int) int {
	j := i
	for j < len(s) {
		switch {
		case s[j] == '\n':
			return j
		case s[j] == ' ', s[j] == '\t', s[j] == '\r', s[j] == '\v', s[j] == '\f':
			j++
		case isLineCommentAt(s, j):
			// a line comment runs to the newline, which ends the remark
			if nl := strings.IndexByte(s[j:], '\n'); nl >= 0 {
				return j + nl
			}
			return len(s)
		case isBlockCommentAt(s, j):
			j = skipBlockComment(s, j)
		default:
			return i // code follows on the same line
		}
	}
	return len(s)
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
