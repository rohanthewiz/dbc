package sqlsplit

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// Format cleans up and lays out a SQL buffer: one clause per line, nested
// queries indented in blocks, keywords upper-cased, spacing normalized.
//
// It first undoes the damage a query picks up on its way out of a program
// (Unwrap): a log line's prose in front of it, and the string-literal
// quoting a log or a JSON field puts around it, with its newlines as \n.
// So a query copied straight from a log line formats in one go:
//
//	"SQL query after template replacement:\nupdate t set a=1;\n\nwith …"
//	                     ↓
//	UPDATE t
//	SET a = 1;
//
//	WITH …
//
// HOW. It is a layout pass over tokens, not a parser — like the splitter,
// and on the splitter's own scanner (skipQuoted, skipBlockComment,
// dollarTag…), so a string or a comment ends where the editor draws it
// ending. The tokens are folded into a tree on their brackets ( … ) and on
// CASE … END, and each statement's top level is cut into clauses on a fixed
// list of clause words (SELECT, FROM, JOIN, WHERE, …). Layout then follows
// one rule throughout: a piece goes on the current line if it fits in
// fmtWidth columns, and breaks the way its kind breaks if it does not —
//
//	a list clause (SELECT, SET, GROUP BY…)   one item per line, indented
//	a condition (WHERE, HAVING, ON)          one AND / OR per line, indented
//	a ( … ) holding a query                  a block: the query indented
//	                                         under it, ")" back at the left
//	any other ( … ) with commas              one item per line
//	CASE … END                               one WHEN / ELSE per line
//
// A query with more than two clauses is always a block, even when short:
// the clause-per-line shape is what makes a subquery readable.
//
// Nothing is reordered or dropped: the output holds the input's tokens,
// comments included, in the input's order. A word that is not on the
// keyword list keeps its case, so identifiers (which Postgres folds and
// MySQL may not) are never rewritten; nor is a keyword written as part of a
// dotted name (t.key).
func Format(sql string) string {
	sql = Unwrap(sql)
	nodes := fmtParse(fmtLex(sql))
	w := &fmtWriter{}
	for i, st := range fmtStatements(nodes) {
		if i > 0 {
			w.nl(0)
			w.blank()
		}
		w.seq(st.nodes, 0)
		for _, t := range st.tail {
			w.tok(t)
		}
	}
	return strings.TrimRight(string(w.buf), " \n")
}

// FormatSpan is Format for a piece of an editor's buffer — a selection, or
// the whole text: the blanks around it are kept, so a selection of whole
// lines keeps its last newline and the text after it stays on its own line.
// Blank or not, a span with no code comes back as it was.
func FormatSpan(s string) string {
	body := strings.TrimSpace(s)
	if body == "" {
		return s
	}
	lead := s[:strings.Index(s, body)]
	return lead + Format(body) + s[len(lead)+len(body):]
}

const (
	fmtIndent = "  " // one level of indent
	fmtWidth  = 80   // the column a line breaks before, when it can
)

// ---------------------------------------------------------------------------
// Unwrap: from a log line or a string literal back to SQL
// ---------------------------------------------------------------------------

// Unwrap recovers the SQL from text copied out of a log or a program:
//
//   - the whole text quoted as a string literal ("…" or '…', as a log line,
//     a JSON field, or a debugger prints it) with escapes inside it: the
//     quotes come off and \n, \t, \" … become what they stand for;
//   - no quotes, but the newlines still escaped: one long line with \n
//     outside any SQL string literal (a '…\n…' inside one is SQL's own and
//     left alone);
//   - prose in front of the query ("SQL query after template replacement:",
//     "executing: "): dropped, up to the first statement word that ends it —
//     one after a colon, or one starting a line.
//
// Each is decided conservatively — text that is already plain SQL comes back
// unchanged (trimmed), so Format can always run it first.
func Unwrap(s string) string {
	t := strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
	if n := len(t); n >= 2 && (t[0] == '"' || t[0] == '\'') && t[n-1] == t[0] && hasEscape(t[1:n-1]) {
		// a statement is never just a string literal, so a quoted text
		// with escapes in it is a quoted query. strconv covers Go and
		// most JSON; unescape covers the rest (\/, '…' quoting).
		if u, err := strconv.Unquote(t); err == nil && t[0] == '"' {
			t = u
		} else {
			t = unescape(t[1 : n-1])
		}
	} else if !strings.Contains(t, "\n") && escapedNewlineInCode(t) {
		t = unescape(t)
	}
	return strings.TrimSpace(stripLead(strings.TrimSpace(t)))
}

// hasEscape reports whether s holds one of the escapes that mark it as the
// inside of a quoted string: a line break, a tab or a quote.
func hasEscape(s string) bool {
	return strings.Contains(s, `\n`) || strings.Contains(s, `\t`) ||
		strings.Contains(s, `\"`) || strings.Contains(s, `\r`)
}

// escapedNewlineInCode reports whether s has a \n escape outside a
// single-quoted SQL string. Comments are not skipped: with its newlines
// escaped, a -- comment would seem to run to the end of the text and hide
// the very escapes being looked for.
func escapedNewlineInCode(s string) bool {
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '\'':
			i = skipQuoted(s, i, '\'') - 1
		case s[i] == '\\' && i+1 < len(s) && s[i+1] == 'n':
			return true
		}
	}
	return false
}

// unescape undoes backslash escapes the way a JSON or C-family string
// would have written them. An unknown escape keeps its backslash: it may be
// the SQL's own (a regex, a LIKE pattern).
func unescape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch c := s[i]; c {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			// \r\n written out: the \n that follows makes the break
		case '"', '\'', '\\', '/':
			b.WriteByte(c)
		case 'u':
			if i+4 < len(s) {
				if r, err := strconv.ParseUint(s[i+1:i+5], 16, 32); err == nil {
					b.WriteRune(rune(r))
					i += 4
					continue
				}
			}
			b.WriteString(`\u`)
		default:
			b.WriteByte('\\')
			b.WriteByte(c)
		}
	}
	return b.String()
}

// stmtWords are the words a statement starts with — where the prose in front
// of a query ends.
var stmtWords = map[string]bool{
	"select": true, "with": true, "insert": true, "update": true, "delete": true,
	"create": true, "alter": true, "drop": true, "explain": true, "truncate": true,
	"grant": true, "revoke": true, "merge": true, "call": true, "values": true,
	"show": true, "describe": true, "pragma": true, "vacuum": true, "analyze": true,
	"copy": true, "replace": true, "upsert": true,
}

// stripLead drops prose in front of the query.
//
// Text that starts with code (a statement word, a comment, a bracket) is
// left alone, unless its first line reads "<words>: <statement word>" —
// "Update query: update t …", where the label happens to begin with one.
// Text that starts with anything else is cut at the first statement word
// that follows a colon or begins a line; with none, it is left alone, as it
// may be a statement Format has no word for.
func stripLead(s string) string {
	toks := fmtLex(s)
	if len(toks) == 0 {
		return s
	}
	colonKw := func(i int) bool {
		return i+1 < len(toks) && toks[i].kind == ftOp && toks[i].text == ":" &&
			stmtWords[toks[i+1].low]
	}
	first := toks[0]
	if stmtWords[first.low] || first.kind == ftOpen ||
		first.kind == ftLineComment || first.kind == ftBlockComment {
		// "<words>: <statement word>" on the first line only
		for i := 1; i+1 < len(toks) && !toks[i].nl; i++ {
			if colonKw(i) {
				return s[toks[i+1].start:]
			}
			if toks[i].kind != ftWord && toks[i].kind != ftKeyword {
				return s
			}
		}
		return s
	}
	for i := 1; i < len(toks); i++ {
		if toks[i].kind == ftSemi {
			return s
		}
		if colonKw(i-1) || (toks[i].nl && stmtWords[toks[i].low]) {
			return s[toks[i].start:]
		}
		// "label:update …" with no space lexes as the parameter :update
		if t := toks[i]; t.kind == ftParam && t.text[0] == ':' && stmtWords[strings.ToLower(t.text[1:])] {
			return s[t.start+1:]
		}
	}
	return s
}

// ---------------------------------------------------------------------------
// Tokens
// ---------------------------------------------------------------------------

type ftKind int

const (
	ftWord         ftKind = iota // an identifier, or a word off the keyword list
	ftKeyword                    // a word on the keyword list, written upper-case
	ftString                     // '…', E'…', a dollar-quoted body
	ftIdent                      // "…" or `…`
	ftNumber                     // 1, 2.5, 1e-3
	ftParam                      // $1, ?, :name
	ftOp                         // =, <>, ||, ::, -> …
	ftComma                      // ,
	ftSemi                       // ;
	ftDot                        // .
	ftOpen                       // (
	ftClose                      // )
	ftOpenBr                     // [
	ftCloseBr                    // ]
	ftLineComment                // -- … (without its newline)
	ftBlockComment               // /* … */
)

// ftok is one token as the formatter sees it.
type ftok struct {
	kind  ftKind
	text  string // as it is written out: a keyword's upper-cased
	low   string // lower-case text of a word or keyword, for matching
	start int    // byte offset in the source
	space bool   // whitespace preceded it in the source
	nl    bool   // a line break preceded it in the source
}

// fmtKeywords are the words written upper-case and the ones clauses are
// found by: the highlighter's vocabulary (so a formatted keyword is a
// colored one) and the few clause words it leaves out.
var fmtKeywords = func() map[string]bool {
	m := map[string]bool{}
	for w := range keywords {
		m[w] = true
	}
	for w := range strings.FieldsSeq(`do conflict nothing lateral filter for of
		interval array within ordinality similar`) {
		m[w] = true
	}
	return m
}()

// fmtOps are the operators longer than one character, longest first so a
// prefix never wins ("->>" before "->").
var fmtOps = []string{
	"->>", "#>>", "!~*", "!~~", "::", "->", "#>", "@>", "<@", "<>", "!=", "<=",
	">=", "||", "&&", "~*", "!~", "~~", ":=", "=>", "<<", ">>",
}

// fmtLex cuts sql into tokens, dropping whitespace but noting it on the
// token it preceded.
func fmtLex(sql string) []ftok {
	var out []ftok
	space, nl := false, false
	add := func(k ftKind, start, end int) {
		t := ftok{kind: k, text: sql[start:end], start: start, space: space, nl: nl}
		if k == ftWord {
			t.low = strings.ToLower(t.text)
			if fmtKeywords[t.low] {
				t.kind = ftKeyword
			}
		}
		out = append(out, t)
		space, nl = false, false
	}
	i := 0
	for i < len(sql) {
		c := sql[i]
		switch {
		case isSpace(c):
			space = true
			if c == '\n' {
				nl = true
			}
			i++
		case isLineCommentAt(sql, i):
			j := skipLineComment(sql, i)
			end := j
			if end > i && sql[end-1] == '\n' {
				end-- // the newline is whitespace, seen on the next token
			}
			add(ftLineComment, i, end)
			i = end
		case isBlockCommentAt(sql, i):
			j := skipBlockComment(sql, i)
			add(ftBlockComment, i, j)
			i = j
		case c == '\'':
			j := skipQuoted(sql, i, c)
			add(ftString, i, j)
			i = j
		case c == '"', c == '`':
			j := skipQuoted(sql, i, c)
			add(ftIdent, i, j)
			i = j
		case c == '$':
			if tag, ok := dollarTag(sql, i); ok {
				j := skipDollarQuoted(sql, i, tag)
				add(ftString, i, j)
				i = j
				continue
			}
			j := i + 1
			for j < len(sql) && isDigit(sql[j]) {
				j++
			}
			add(ftParam, i, j)
			i = j
		case c == '?':
			add(ftParam, i, i+1)
			i++
		case c == ':' && i+1 < len(sql) && fmtWordByte(sql[i+1]) && !isDigit(sql[i+1]) &&
			(i == 0 || sql[i-1] != ':'):
			j := i + 1
			for j < len(sql) && fmtWordByte(sql[j]) {
				j++
			}
			add(ftParam, i, j)
			i = j
		case isDigit(c) || (c == '.' && i+1 < len(sql) && isDigit(sql[i+1]) &&
			(len(out) == 0 || out[len(out)-1].kind != ftWord && out[len(out)-1].kind != ftIdent)):
			j := i
			for j < len(sql) {
				d := sql[j]
				if isDigit(d) || d == '.' {
					j++
				} else if (d == 'e' || d == 'E') && j+1 < len(sql) {
					j++
					if sql[j] == '+' || sql[j] == '-' {
						j++
					}
				} else {
					break
				}
			}
			// 1abc is a word in MySQL (an identifier may start with a digit)
			if j < len(sql) && fmtWordByte(sql[j]) {
				for j < len(sql) && fmtWordByte(sql[j]) {
					j++
				}
				add(ftWord, i, j)
			} else {
				add(ftNumber, i, j)
			}
			i = j
		case fmtWordByte(c) || (c == '@' && i+1 < len(sql) && (fmtWordByte(sql[i+1]) || sql[i+1] == '@')):
			// a word, or a MySQL @var / @@var
			j := i + 1
			for j < len(sql) && (fmtWordByte(sql[j]) || c == '@' && j == i+1 && sql[j] == '@') {
				j++
			}
			// E'…', N'…', X'…', B'…': a prefixed string is one token
			if j-i == 1 && j < len(sql) && sql[j] == '\'' && strings.ContainsRune("eEnNxXbB", rune(c)) {
				k := skipQuoted(sql, j, '\'')
				add(ftString, i, k)
				i = k
				continue
			}
			add(ftWord, i, j)
			i = j
		case c == '(':
			add(ftOpen, i, i+1)
			i++
		case c == ')':
			add(ftClose, i, i+1)
			i++
		case c == '[':
			add(ftOpenBr, i, i+1)
			i++
		case c == ']':
			add(ftCloseBr, i, i+1)
			i++
		case c == ',':
			add(ftComma, i, i+1)
			i++
		case c == ';':
			add(ftSemi, i, i+1)
			i++
		case c == '.':
			add(ftDot, i, i+1)
			i++
		default:
			j := i + 1
			for _, op := range fmtOps {
				if strings.HasPrefix(sql[i:], op) {
					j = i + len(op)
					break
				}
			}
			if j == i+1 && c >= utf8.RuneSelf {
				_, n := utf8.DecodeRuneInString(sql[i:])
				j = i + n
			}
			add(ftOp, i, j)
			i = j
		}
	}
	// A keyword in a dotted name is a name (t.key, key.x), and is left as
	// written. Upper-casing comes after, so this sees the whole stream.
	for i := range out {
		t := &out[i]
		if t.kind != ftKeyword {
			continue
		}
		if (i > 0 && out[i-1].kind == ftDot) || (i+1 < len(out) && out[i+1].kind == ftDot) {
			t.kind = ftWord
			continue
		}
		t.text = strings.ToUpper(t.text)
	}
	return out
}

// fmtWordByte extends the splitter's word bytes with non-ASCII ones, so a
// name like café is one word rather than three tokens.
func fmtWordByte(c byte) bool { return isWordByte(c) || c >= utf8.RuneSelf }

// ---------------------------------------------------------------------------
// Tree
// ---------------------------------------------------------------------------

// fnode is a token, or a group: ( … ) or CASE … END with what is inside.
// A group whose closer never came has close nil and is written without one.
type fnode struct {
	tok         *ftok
	open, close *ftok
	kids        []fnode
}

func (n fnode) group() bool { return n.open != nil }

// kw returns the lower-case keyword n is, or "".
func (n fnode) kw() string {
	if n.tok != nil && n.tok.kind == ftKeyword {
		return n.tok.low
	}
	return ""
}

// fmtParse folds the tokens into groups. A stray ")" or END is kept as a
// plain token: the formatter lays out what it is given, it does not judge.
func fmtParse(toks []ftok) []fnode {
	type frame struct {
		open   *ftok
		kids   []fnode
		isCase bool
	}
	stack := []frame{{}}
	push := func(n fnode) {
		top := &stack[len(stack)-1]
		top.kids = append(top.kids, n)
	}
	pop := func(close *ftok) {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		push(fnode{open: f.open, close: close, kids: f.kids})
	}
	for i := range toks {
		t := &toks[i]
		switch {
		case t.kind == ftOpen:
			stack = append(stack, frame{open: t})
		case t.kind == ftKeyword && t.low == "case":
			stack = append(stack, frame{open: t, isCase: true})
		case t.kind == ftClose && len(stack) > 1 && !stack[len(stack)-1].isCase:
			pop(t)
		case t.kind == ftKeyword && t.low == "end" && len(stack) > 1 && stack[len(stack)-1].isCase:
			pop(t)
		case t.kind == ftClose && len(stack) > 1:
			// a ")" while inside a CASE: the CASE never got its END —
			// close it unended, then let the ")" close its bracket
			for len(stack) > 1 && stack[len(stack)-1].isCase {
				pop(nil)
			}
			if len(stack) > 1 {
				pop(t)
			} else {
				push(fnode{tok: t})
			}
		default:
			push(fnode{tok: t})
		}
	}
	for len(stack) > 1 {
		pop(nil)
	}
	return stack[0].kids
}

// fmtStmt is one statement: its nodes, then its ";" and any comment on the
// same line after it, which belong to it rather than to the next one.
type fmtStmt struct {
	nodes []fnode
	tail  []*ftok
}

func fmtStatements(nodes []fnode) []fmtStmt {
	var out []fmtStmt
	var cur fmtStmt
	for i := 0; i < len(nodes); i++ {
		n := nodes[i]
		if n.tok == nil || n.tok.kind != ftSemi {
			cur.nodes = append(cur.nodes, n)
			continue
		}
		cur.tail = append(cur.tail, n.tok)
		for i+1 < len(nodes) && nodes[i+1].tok != nil && !nodes[i+1].tok.nl &&
			(nodes[i+1].tok.kind == ftLineComment || nodes[i+1].tok.kind == ftBlockComment) {
			i++
			cur.tail = append(cur.tail, nodes[i].tok)
		}
		if len(cur.nodes) > 0 || len(cur.tail) > 1 {
			out = append(out, cur)
		}
		cur = fmtStmt{}
	}
	if len(cur.nodes) > 0 {
		out = append(out, cur)
	}
	return out
}

// ---------------------------------------------------------------------------
// Clauses
// ---------------------------------------------------------------------------

type clKind int

const (
	clPlain  clKind = iota // FROM, UPDATE, LIMIT…: the body as one run
	clList                 // SELECT, SET, GROUP BY…: an item per line if long
	clCond                 // WHERE, HAVING: an AND / OR per line if long
	clJoin                 // … JOIN: ON to the next line if long
	clCreate               // CREATE TABLE: the column list one per line
)

// clause is a clause word or words (head) and what follows up to the next.
// The first clause of a statement may have no head: what comes before the
// first clause word (a comment, or a statement Format has no words for).
type clause struct {
	head []fnode
	body []fnode
	kind clKind
}

// clauses cuts one statement's (or subquery's) top level at clause words.
func clauses(nodes []fnode) []clause {
	var out []clause
	cur := clause{}
	for i := 0; i < len(nodes); {
		if n, k := clauseHead(nodes, i); n > 0 {
			if len(cur.head)+len(cur.body) > 0 {
				out = append(out, cur)
			}
			cur = clause{head: nodes[i : i+n], kind: k}
			i += n
			continue
		}
		cur.body = append(cur.body, nodes[i])
		i++
	}
	if len(cur.head)+len(cur.body) > 0 {
		out = append(out, cur)
	}
	return out
}

// clauseHead reports whether a clause starts at nodes[i]: how many nodes its
// head takes, and its kind. The checks on the words around are the places a
// clause word is not a clause: IS DISTINCT FROM, FOR UPDATE, ON DELETE SET
// NULL, DEFAULT VALUES, ON CONFLICT … DO UPDATE, LEFT(…) the function,
// WITH TIME ZONE.
func clauseHead(nodes []fnode, i int) (int, clKind) {
	at := func(j int) string {
		if j < 0 || j >= len(nodes) {
			return ""
		}
		return nodes[j].kw()
	}
	w, prev, next := at(i), at(i-1), at(i+1)
	first := true // nothing but comments before i
	for _, n := range nodes[:i] {
		if n.tok == nil || (n.tok.kind != ftLineComment && n.tok.kind != ftBlockComment) {
			first = false
			break
		}
	}
	switch w {
	case "select":
		if next == "distinct" || next == "all" {
			return 2, clList
		}
		return 1, clList
	case "from":
		if prev == "distinct" {
			return 0, 0
		}
		return 1, clPlain
	case "where", "having":
		return 1, clCond
	case "group", "order":
		if next == "by" {
			return 2, clList
		}
	case "set":
		if (prev == "delete" || prev == "update") && (next == "null" || next == "default") {
			return 0, 0
		}
		return 1, clList
	case "values":
		if prev == "default" {
			return 0, 0
		}
		return 1, clList
	case "returning":
		return 1, clList
	case "limit", "offset", "window", "fetch":
		return 1, clPlain
	case "for":
		// the row-locking clause: FOR UPDATE, FOR SHARE, FOR NO KEY
		// UPDATE (share and no are not keywords, so the word is read)
		var after string
		if t := nodes[min(i+1, len(nodes)-1)].tok; i+1 < len(nodes) && t != nil {
			after = t.low
		}
		switch after {
		case "update", "share", "no", "key":
			return 1, clPlain
		}
	case "union", "intersect", "except":
		if next == "all" || next == "distinct" {
			return 2, clPlain
		}
		return 1, clPlain
	case "with":
		if first {
			return 1, clPlain
		}
	case "update":
		switch prev {
		case "for", "do", "key", "on", "before", "after", "of", "or":
			return 0, 0
		}
		return 1, clPlain
	case "delete", "insert":
		switch prev {
		case "on", "before", "after", "of", "or":
			return 0, 0
		}
		if (w == "delete" && next == "from") || (w == "insert" && next == "into") {
			return 2, clPlain
		}
		return 1, clPlain
	case "join":
		return 1, clJoin
	case "natural", "inner", "cross", "left", "right", "full":
		j := i
		for {
			switch at(j) {
			case "natural", "inner", "cross", "left", "right", "full", "outer":
				j++
				continue
			case "join":
				return j - i + 1, clJoin
			}
			return 0, 0
		}
	case "on":
		if next == "conflict" {
			return 2, clPlain
		}
	case "create":
		if first {
			return 1, clCreate
		}
	}
	return 0, 0
}

// ---------------------------------------------------------------------------
// Writer
// ---------------------------------------------------------------------------

// fmtWriter lays tokens out into lines.
//
// It has two modes. The normal one writes the output. The flat one (used
// through try) writes a piece on one line to measure it, and notes in
// broken anything that cannot sit on one line — a line comment, a text with
// a line break, a subquery of more than two clauses — so the caller knows
// to break it instead.
type fmtWriter struct {
	buf       []byte
	lineStart int   // where the current line starts in buf
	col       int   // display column at the end of buf
	indent    int   // the current line's indent level
	prev      *ftok // the last token on this line; nil at a line's start
	glue      bool  // the next token attaches with no space (after a unary -)
	breakNext bool  // a line comment ended the line: the next token starts a new one
	flat      bool
	broken    bool
}

// nl starts a new line at indent level n. On a line that holds nothing yet
// it only resets the indent, so breaks asked for twice never leave a blank
// line.
func (w *fmtWriter) nl(n int) {
	if w.flat {
		return
	}
	if w.prev == nil {
		w.buf = w.buf[:w.lineStart] // nothing on it yet: drop its indent
	} else {
		w.buf = append(w.buf, '\n')
		w.lineStart = len(w.buf)
	}
	w.buf = append(w.buf, strings.Repeat(fmtIndent, n)...)
	w.col, w.indent, w.prev, w.glue, w.breakNext = n*len(fmtIndent), n, nil, false, false
}

// blank writes an empty line (between statements).
func (w *fmtWriter) blank() {
	w.buf = append(w.buf[:w.lineStart], '\n')
	w.lineStart = len(w.buf)
	w.col = 0
}

// tok writes one token, with the space before it that its neighbors call
// for.
func (w *fmtWriter) tok(t *ftok) {
	if t == nil {
		return
	}
	comment := t.kind == ftLineComment || t.kind == ftBlockComment
	// after a line comment, or before a comment that had a line of its
	// own in the source
	if w.breakNext || (comment && t.nl && w.prev != nil && !w.flat) {
		w.nl(w.indent)
	}
	if w.prev != nil && !w.glue && fmtSpace(w.prev, t) {
		w.buf = append(w.buf, ' ')
		w.col++
	}
	w.buf = append(w.buf, t.text...)
	if i := strings.LastIndexByte(t.text, '\n'); i >= 0 {
		// a multi-line string or comment: the column restarts after it
		w.col = utf8.RuneCountInString(t.text[i+1:])
		if w.flat {
			w.broken = true
		}
	} else {
		w.col += utf8.RuneCountInString(t.text)
	}
	w.glue = fmtUnary(w.prev, t)
	w.prev = t
	if t.kind == ftLineComment {
		if w.flat {
			w.broken = true
		} else {
			w.breakNext = true
		}
	}
}

// fmtSpace reports whether a space goes between prev and t.
func fmtSpace(prev, t *ftok) bool {
	switch {
	case t.kind == ftComma || t.kind == ftSemi || t.kind == ftClose || t.kind == ftCloseBr:
		return false
	case t.kind == ftDot || prev.kind == ftDot:
		return false
	case prev.kind == ftOpen || prev.kind == ftOpenBr:
		return false
	case t.text == "::" || prev.text == "::":
		return false
	case t.kind == ftOpen:
		switch prev.kind {
		case ftKeyword:
			// IN (…), AS (…), OVER (…) — but LEFT(…), CAST(…) are
			// calls, kept as written
			switch prev.low {
			case "left", "right", "cast":
				return t.space
			}
			return true
		case ftWord, ftIdent, ftClose, ftCloseBr, ftParam:
			// a call keeps its "f(" and a table its "t (a, b)": as written
			return t.space
		}
		return true
	case t.kind == ftOpenBr:
		// arr[1], (…)[1], ARRAY[1, 2]
		switch prev.kind {
		case ftWord, ftIdent, ftClose, ftCloseBr, ftParam, ftString:
			return false
		case ftKeyword:
			return prev.low != "array"
		}
		return true
	}
	return true
}

// fmtUnary reports whether t, written after prev, is a sign rather than an
// operator — so the number after it attaches: "= -1", "(-x)", "THEN -1".
func fmtUnary(prev, t *ftok) bool {
	if t.kind != ftOp || (t.text != "-" && t.text != "+" && t.text != "~") {
		return false
	}
	if prev == nil {
		return true
	}
	switch prev.kind {
	case ftOp, ftComma, ftOpen, ftOpenBr:
		return true
	case ftKeyword:
		switch prev.low {
		case "null", "true", "false", "end":
			return false
		}
		return true
	}
	return false
}

// try writes nodes on the current line if they fit there whole, and
// reports whether it did. In flat mode it always writes them.
func (w *fmtWriter) try(nodes ...fnode) bool {
	if w.flat {
		w.inline(nodes)
		return true
	}
	if w.breakNext {
		w.nl(w.indent) // start the line a comment ended, then measure from it
	}
	m := &fmtWriter{flat: true, prev: w.prev, glue: w.glue}
	m.inline(nodes)
	if m.broken || w.col+m.col > fmtWidth {
		return false
	}
	w.buf = append(w.buf, m.buf...)
	w.col += m.col
	w.prev, w.glue = m.prev, m.glue
	return true
}

// inline writes nodes one after another, each group laid out by its kind.
func (w *fmtWriter) inline(nodes []fnode) {
	for _, n := range nodes {
		w.node(n)
	}
}

// node writes one node. A group is tried on the current line first; when it
// does not fit, it breaks by its kind (see Format).
func (w *fmtWriter) node(n fnode) {
	if !n.group() {
		w.tok(n.tok)
		return
	}
	query := n.open.kind == ftOpen && isQuery(n.kids)
	if w.flat {
		w.tok(n.open)
		if query {
			w.seq(n.kids, 0)
		} else {
			w.inline(n.kids)
		}
		w.tok(n.close)
		return
	}
	if w.try(n) {
		return
	}
	if n.open.kind == ftKeyword { // CASE … END
		base := w.indent
		w.tok(n.open)
		parts := splitBefore(n.kids, func(k fnode) bool { return k.kw() == "when" || k.kw() == "else" })
		w.inline(parts[0]) // the operand of CASE x WHEN …
		for _, p := range parts[1:] {
			w.nl(base + 1)
			w.inline(p)
		}
		w.nl(base)
		w.tok(n.close)
		return
	}
	if query {
		base := w.indent
		w.tok(n.open)
		w.nl(base + 1)
		w.seq(n.kids, base+1)
		w.nl(base)
		w.tok(n.close)
		return
	}
	if items := splitAfter(n.kids, isComma); len(items) > 1 {
		w.block(n, items)
		return
	}
	w.tok(n.open)
	w.inline(n.kids)
	w.tok(n.close)
}

// block writes a bracket with one item per line between, the closer back at
// the line's indent:
//
//	(
//	  a int,
//	  b text
//	)
func (w *fmtWriter) block(n fnode, items [][]fnode) {
	base := w.indent
	w.tok(n.open)
	for _, it := range items {
		w.nl(base + 1)
		w.inline(it)
	}
	w.nl(base)
	w.tok(n.close)
}

// seq writes a statement or a subquery: a clause per line at indent level
// n. In flat mode the clauses run on one line, and a query of more than two
// clauses marks it broken.
func (w *fmtWriter) seq(nodes []fnode, n int) {
	cls := clauses(nodes)
	if w.flat && len(cls) > 2 {
		w.broken = true
	}
	for i, c := range cls {
		if i > 0 {
			w.nl(n)
		}
		w.clause(c, n)
	}
}

// clause writes its head and lays out its body by kind.
func (w *fmtWriter) clause(c clause, n int) {
	w.inline(c.head)
	switch c.kind {
	case clList:
		if w.try(c.body...) {
			return
		}
		items := splitAfter(c.body, isComma)
		if len(items) < 2 {
			w.inline(c.body)
			return
		}
		for _, it := range items {
			w.nl(n + 1)
			w.inline(it)
		}
	case clCond:
		w.cond(c.body, n)
	case clJoin:
		if w.try(c.body...) {
			return
		}
		on := -1
		for i, b := range c.body {
			if b.kw() == "on" {
				on = i
				break
			}
		}
		if on < 0 {
			w.inline(c.body)
			return
		}
		w.inline(c.body[:on])
		// after a block (") ga") ON stays on the closing line if it fits
		if w.try(c.body[on:]...) {
			return
		}
		w.nl(n + 1)
		w.node(c.body[on])
		w.cond(c.body[on+1:], n+1)
	case clCreate:
		// CREATE TABLE t (…): the column list is a block, however short
		g := -1
		table := false
		for i, b := range c.body {
			if b.kw() == "table" {
				table = true
			}
			if b.group() && b.open.kind == ftOpen {
				g = i
				break
			}
		}
		if w.flat || !table || g < 0 || isQuery(c.body[g].kids) {
			w.inline(c.body)
			return
		}
		w.inline(c.body[:g])
		w.block(c.body[g], splitAfter(c.body[g].kids, isComma))
		w.inline(c.body[g+1:])
	default:
		w.inline(c.body)
	}
}

// cond writes a condition: on the line if it fits, else an AND / OR per
// line under it. The AND of BETWEEN … AND … is not a break.
func (w *fmtWriter) cond(body []fnode, n int) {
	if w.try(body...) {
		return
	}
	between := false
	parts := splitBefore(body, func(k fnode) bool {
		switch k.kw() {
		case "between":
			between = true
		case "and":
			if between {
				between = false
				return false
			}
			return true
		case "or":
			return true
		}
		return false
	})
	w.inline(parts[0])
	for _, p := range parts[1:] {
		w.nl(n + 1)
		w.inline(p)
	}
}

// isQuery reports whether a bracket's contents are a query: a subquery, a
// CTE's body, an IN (SELECT …).
func isQuery(kids []fnode) bool {
	for _, k := range kids {
		if k.tok != nil && (k.tok.kind == ftLineComment || k.tok.kind == ftBlockComment) {
			continue
		}
		switch k.kw() {
		case "select", "with", "insert", "update", "delete", "values":
			return true
		}
		return false
	}
	return false
}

func isComma(n fnode) bool { return n.tok != nil && n.tok.kind == ftComma }

// splitAfter cuts nodes after each separator, which stays at the end of its
// piece — a list item keeps its comma. A comment on the separator's line
// stays with the piece too, so "a, -- note" keeps its note beside it.
func splitAfter(nodes []fnode, sep func(fnode) bool) [][]fnode {
	var out [][]fnode
	var cur []fnode
	for i := 0; i < len(nodes); i++ {
		cur = append(cur, nodes[i])
		if !sep(nodes[i]) {
			continue
		}
		for i+1 < len(nodes) && nodes[i+1].tok != nil && !nodes[i+1].tok.nl &&
			(nodes[i+1].tok.kind == ftLineComment || nodes[i+1].tok.kind == ftBlockComment) {
			i++
			cur = append(cur, nodes[i])
		}
		out = append(out, cur)
		cur = nil
	}
	if len(cur) > 0 || len(out) == 0 {
		out = append(out, cur)
	}
	return out
}

// splitBefore cuts nodes before each separator, which starts its piece — a
// condition's AND leads its line. The first piece may be empty.
func splitBefore(nodes []fnode, sep func(fnode) bool) [][]fnode {
	out := [][]fnode{nil}
	for _, n := range nodes {
		if sep(n) {
			out = append(out, nil)
		}
		out[len(out)-1] = append(out[len(out)-1], n)
	}
	return out
}
