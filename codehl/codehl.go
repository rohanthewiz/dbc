// Package codehl classifies the spans of a fenced code block for syntax
// highlighting in the assistant's answers: SQL, Go, Python, JavaScript,
// JSON and shell.
//
// WHY A HAND-WRITTEN LEXER and not chroma or a grammar: the answers are short,
// redrawn every frame while they stream, and only a handful of languages
// matter in a SQL client's answers. A
// table-driven scanner — comments, strings, numbers, a keyword list — is a
// few hundred lines with no dependency, and it degrades gracefully: a
// construct it does not know (a JS regex literal, a Go struct tag) is merely
// uncolored or colored as its nearest lexical cousin, never an error.
//
// SQL is not lexed here: it delegates to sqlsplit.Lex, the scanner the editor
// colors with, so a statement looks the same in an answer as it will once
// inserted into the editor. Shell has a scanner of its own too (lexShell):
// its quoting, $variables, word-bound comments and heredocs fit none of the
// C-family switches.
//
// The web pane draws answers in the browser, so web/static/js/hl.js mirrors
// this file rule for rule. Its keyword lists are checked against Keywords by
// web's tests; the scanning rules are kept in step by hand.
package codehl

import (
	"slices"
	"strings"

	"github.com/rohanthewiz/dbc/sqlsplit"
)

// Kind classifies a span. Text is everything no span covers.
type Kind int

const (
	Text    Kind = iota
	Keyword      // a reserved word (and the literal constants: nil, None, true…)
	String       // any string, rune or template literal
	Number       // a numeric literal
	Comment      // line or block comment
	Ident        // a quoted SQL identifier ("…" or `…`)
	Param        // a SQL bind parameter ($1, ?, :name)
	// Var is a shell variable ($HOME, ${X:-1}, $?). It was once drawn as
	// Param, which both front ends color as an error (bind parameters are
	// rare enough in SQL to deserve the alarm); in a shell block $vars are
	// on most lines, and a page of error red read as a page of mistakes.
	Var
)

// Span is one classified run, as byte offsets into the source. Spans are
// sorted, never overlap, and start and end on rune boundaries.
type Span struct {
	Start, End int
	Kind       Kind
}

// Lang maps a fence's language tag to the lexer that draws it: "sql", "go",
// "python", "js", "json", "shell", or "" for a language not highlighted.
//
// An untagged fence is SQL, the same call tui/chat.go isSQLLang makes for
// ⤓ insert: the preamble asks for ```sql and models often drop the tag, so
// in a SQL assistant an untagged block is overwhelmingly SQL. Coloring a
// plain-text block that way costs little — only SQL keywords light up. The
// one exception, decided by Lex from the content, is untagged JSON.
func Lang(tag string) string {
	switch strings.ToLower(strings.TrimSpace(tag)) {
	case "", "sql", "postgres", "postgresql", "psql", "mysql", "sqlite", "pgsql", "plpgsql":
		return "sql"
	case "go", "golang":
		return "go"
	case "python", "py", "python3", "py3":
		return "python"
	case "js", "javascript", "jsx", "mjs", "cjs", "node":
		return "js"
	case "json", "jsonc", "json5", "jsonl", "ndjson":
		return "json"
	case "sh", "bash", "shell", "zsh", "console", "shell-session", "shellsession", "terminal":
		return "shell"
	}
	return ""
}

// Lex classifies src as the language its fence tag names. It returns nil
// for a language it does not highlight, which callers draw as plain text.
func Lex(tag, src string) []Span {
	lang := Lang(tag)
	switch {
	case lang == "sql" && strings.TrimSpace(tag) == "" && looksJSON(src):
		// SQL never opens with { or [, while a JSON value nearly always
		// does; lexed as SQL, its "keys" would be drawn as quoted
		// identifiers. Only the colors change — the TUI and the page still
		// offer ⤓ insert on an untagged block, as they decide by tag.
		lang = "json"
	case lang == "sql":
		return lexSQL(src)
	case lang == "shell":
		return lexShell(src)
	}
	sp, ok := specs[lang]
	if !ok {
		return nil
	}
	return sp.lex(src)
}

// Keywords returns the sorted keyword list of a language (by fence tag), so
// the browser's copy in hl.js can be tested against the one source of truth.
func Keywords(tag string) []string {
	var set map[string]bool
	switch lang := Lang(tag); lang {
	case "sql":
		return sqlsplit.Keywords()
	case "shell":
		set = shellKeywords
	default:
		sp, ok := specs[lang]
		if !ok {
			return nil
		}
		set = sp.keywords
	}
	out := make([]string, 0, len(set))
	for w := range set {
		out = append(out, w)
	}
	slices.Sort(out)
	return out
}

// looksJSON reports whether src's first non-blank byte opens an object or
// an array.
func looksJSON(src string) bool {
	t := strings.TrimLeft(src, " \t\r\n")
	return t != "" && (t[0] == '{' || t[0] == '[')
}

// lexSQL translates sqlsplit's tokens one for one; TokText never appears in
// its output, but is mapped anyway so a new kind there degrades to plain.
func lexSQL(src string) []Span {
	toks := sqlsplit.Lex(src)
	out := make([]Span, 0, len(toks))
	for _, t := range toks {
		k := Text
		switch t.Kind {
		case sqlsplit.TokKeyword:
			k = Keyword
		case sqlsplit.TokString:
			k = String
		case sqlsplit.TokNumber:
			k = Number
		case sqlsplit.TokComment:
			k = Comment
		case sqlsplit.TokIdent:
			k = Ident
		case sqlsplit.TokParam:
			k = Param
		}
		if k != Text {
			out = append(out, Span{t.Start, t.End, k})
		}
	}
	return out
}

// spec is one C-family-ish language as data. The languages differ
// only in these switches, so one scanner (lex) serves them all.
type spec struct {
	keywords     map[string]bool
	lineComment  string // "//" or "#"
	blockComment bool   // /* … */
	// multiQuote opens a string that may span lines: Go's raw `…` (no
	// escapes) or JS's template `…` (escapes honored; ${…} is not parsed,
	// so the whole template, interpolations included, is one string).
	multiQuote   byte
	multiEscapes bool
	triple       bool // Python's ''' and """ (span lines, escapes honored)
	prefixes     bool // Python's r"", b"", f"", rb"" … string prefixes
	dollarIdent  bool // $ is an identifier character (JS)
}

var specs = map[string]*spec{
	"go": {
		keywords: words(`
			break case chan const continue default defer else fallthrough for
			func go goto if import interface map package range return select
			struct switch type var
			true false nil iota`),
		lineComment: "//", blockComment: true,
		multiQuote: '`',
	},
	"python": {
		keywords: words(`
			False None True and as assert async await break class continue
			def del elif else except finally for from global if import in is
			lambda nonlocal not or pass raise return try while with yield`),
		lineComment: "#",
		triple:      true, prefixes: true,
	},
	"js": {
		keywords: words(`
			async await break case catch class const continue debugger default
			delete do else export extends finally for function if import in
			instanceof let new return super switch this throw try typeof var
			void while with yield
			true false null undefined NaN Infinity`),
		lineComment: "//", blockComment: true,
		multiQuote: '`', multiEscapes: true,
		dollarIdent: true,
	},
	// JSON is JavaScript's literal syntax, so the same scanner draws it:
	// keys and values are strings, the three constants are keywords.
	// Comments are on for JSONC (tsconfig, VS Code settings); strict JSON
	// has none, so they cost nothing there.
	"json": {
		keywords:    words(`true false null`),
		lineComment: "//", blockComment: true,
	},
}

func words(s string) map[string]bool {
	m := map[string]bool{}
	for w := range strings.FieldsSeq(s) {
		m[w] = true
	}
	return m
}

// lex scans src once, left to right. Every branch consumes at least one
// byte, so the loop always terminates; anything unrecognized (operators,
// punctuation, whitespace) is skipped and stays Text.
//
// SINGLE-LINE STRINGS STOP AT THE NEWLINE even when unterminated. The scanner
// has no grammar, so it will sometimes mistake a quote for an opener (an
// apostrophe inside a JS regex literal, say); stopping at the line's end
// keeps that mistake to one line instead of coloring the rest of the block.
func (sp *spec) lex(src string) []Span {
	var out []Span
	emit := func(start, end int, k Kind) {
		if end > start {
			out = append(out, Span{start, end, k})
		}
	}
	n := len(src)
	i := 0
	for i < n {
		c := src[i]
		switch {
		case sp.lineComment != "" && strings.HasPrefix(src[i:], sp.lineComment):
			j := strings.IndexByte(src[i:], '\n')
			if j < 0 {
				j = n
			} else {
				j += i
			}
			emit(i, j, Comment)
			i = j
		case sp.blockComment && strings.HasPrefix(src[i:], "/*"):
			j := strings.Index(src[i+2:], "*/")
			if j < 0 {
				j = n
			} else {
				j += i + 4
			}
			emit(i, j, Comment)
			i = j
		case sp.triple && (strings.HasPrefix(src[i:], `"""`) || strings.HasPrefix(src[i:], "'''")):
			j := scanTriple(src, i)
			emit(i, j, String)
			i = j
		case c == '"' || c == '\'':
			j := scanLine(src, i)
			emit(i, j, String)
			i = j
		case sp.multiQuote != 0 && c == sp.multiQuote:
			j := scanMulti(src, i, sp.multiEscapes)
			emit(i, j, String)
			i = j
		case isDigit(c) && !sp.wordBefore(src, i),
			c == '.' && i+1 < n && isDigit(src[i+1]) && !sp.wordBefore(src, i):
			j := scanNumber(src, i)
			emit(i, j, Number)
			i = j
		case sp.isWord(c):
			j := i
			for j < n && (sp.isWord(src[j]) || isDigit(src[j])) {
				j++
			}
			w := src[i:j]
			// a Python string prefix (r, b, f, rb, …) glued to a quote
			// makes the whole thing one string: r"\d+" is not r + "\d+"
			if sp.prefixes && j < n && (src[j] == '"' || src[j] == '\'') && isStrPrefix(w) {
				var k int
				if strings.HasPrefix(src[j:], `"""`) || strings.HasPrefix(src[j:], "'''") {
					k = scanTriple(src, j)
				} else {
					k = scanLine(src, j)
				}
				emit(i, k, String)
				i = k
				continue
			}
			if sp.keywords[w] && !memberAccess(src, i) {
				emit(i, j, Keyword)
			}
			i = j
		default:
			i++
		}
	}
	return out
}

// isWord reports an identifier byte. Any non-ASCII byte counts, which keeps
// a multi-byte rune whole inside one identifier — spans then always end on
// rune boundaries without decoding UTF-8.
func (sp *spec) isWord(c byte) bool {
	return c == '_' || c >= 0x80 || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') ||
		(sp.dollarIdent && c == '$')
}

// wordBefore is true when src[i] continues an identifier (the 2 in x2), so
// it is not the start of a number.
func (sp *spec) wordBefore(src string, i int) bool {
	return i > 0 && (sp.isWord(src[i-1]) || isDigit(src[i-1]))
}

// memberAccess is true for a word right after a dot: obj.default, x.type,
// promise.catch are property names, not keywords. A spread (...this) is
// the exception — there the word is an expression.
func memberAccess(src string, i int) bool {
	return i > 0 && src[i-1] == '.' && !(i >= 3 && src[i-3:i] == "...")
}

func isStrPrefix(w string) bool {
	switch strings.ToLower(w) {
	case "r", "u", "b", "f", "br", "rb", "fr", "rf":
		return true
	}
	return false
}

func isDigit(c byte) bool { return '0' <= c && c <= '9' }

// scanLine scans a '…' or "…" string with backslash escapes from its opening
// quote at i, to just past the closing quote — or to the end of the line
// (the newline excluded) when it is unterminated.
func scanLine(src string, i int) int {
	q := src[i]
	j := i + 1
	for j < len(src) {
		switch src[j] {
		case '\\':
			// an escaped newline is a line continuation, which keeps the
			// string open; any other escaped byte is just skipped
			j += 2
			continue
		case q:
			return j + 1
		case '\n':
			return j
		}
		j++
	}
	return len(src)
}

// scanTriple scans a Python triple-quoted string (either quote) from i to
// past its closing triple, or to the end of src.
func scanTriple(src string, i int) int {
	q := src[i : i+3]
	j := i + 3
	for j < len(src) {
		if src[j] == '\\' {
			j += 2
			continue
		}
		if strings.HasPrefix(src[j:], q) {
			return j + 3
		}
		j++
	}
	return len(src)
}

// scanMulti scans a `…` string (Go raw or JS template) from i to past its
// closing backtick, or to the end of src.
func scanMulti(src string, i int, escapes bool) int {
	q := src[i]
	j := i + 1
	for j < len(src) {
		if escapes && src[j] == '\\' {
			j += 2
			continue
		}
		if src[j] == q {
			return j + 1
		}
		j++
	}
	return len(src)
}

// scanNumber consumes a numeric literal loosely: digits, letters (0x1F,
// 1e9, 10n, 3j, 0b1010), underscores and dots, plus a sign right after an
// exponent marker. Loose is fine — this is coloring, not parsing — but a
// hex literal's e/E/f is a digit, so 0xE+1 is not read as an exponent.
func scanNumber(src string, i int) int {
	hex := strings.HasPrefix(src[i:], "0x") || strings.HasPrefix(src[i:], "0X")
	j := i
	for j < len(src) {
		c := src[j]
		switch {
		case isDigit(c) || c == '_' || c == '.' || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z'):
			j++
			if !hex && (c == 'e' || c == 'E') && j < len(src) && (src[j] == '+' || src[j] == '-') {
				j++
			}
		default:
			return j
		}
	}
	return j
}
