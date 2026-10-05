package codehl

import "strings"

// Shell gets its own scanner because its lexical rules are not C's:
//
//   - a word is anything up to whitespace or an operator, so --if-exists
//     and ./run.sh are single words and their "if"/"run" are not keywords
//   - # opens a comment only where a word could start (${#arr} and a#b
//     are not comments)
//   - '…' has no escapes, "…" does, and both may span lines
//   - $NAME, ${…} and the special parameters ($?, $1, $@ …) are variables,
//     drawn as Param
//   - a heredoc (<<EOF … EOF) is a string from the line after its opener to
//     its terminator line — psql <<SQL is how a DB answer runs a script
//
// Numbers are deliberately left plain: in shell they are mostly arguments
// (-p 5432, 2>&1, postgres:16) and coloring them would be noise.

// shellKeywords are the reserved words, plus the declaration and control
// builtins a reader scans for. Not "select" or "time": both are rare as
// shell syntax and common as plain words in a DB answer.
var shellKeywords = words(`
	if then else elif fi for while until do done case esac in function
	export local readonly declare unset return exit break continue`)

// lexShell scans src once, as spec.lex does. heredoc holds the terminator
// of a heredoc opened on the current line; its body starts at the next
// newline, so the rest of the opening line (| grep x, a closing quote) is
// still scanned as code first.
func lexShell(src string) []Span {
	var out []Span
	emit := func(start, end int, k Kind) {
		if end > start {
			out = append(out, Span{start, end, k})
		}
	}
	n := len(src)
	heredoc := ""
	i := 0
	for i < n {
		c := src[i]
		switch {
		case c == '\n' && heredoc != "":
			j := heredocEnd(src, i+1, heredoc)
			emit(i+1, j, String)
			i, heredoc = j, ""
		case c == '#' && shellWordStart(src, i):
			j := strings.IndexByte(src[i:], '\n')
			if j < 0 {
				j = n
			} else {
				j += i
			}
			emit(i, j, Comment)
			i = j
		case c == '\'' || c == '"':
			j := scanShellQuote(src, i)
			emit(i, j, String)
			i = j
		case c == '$':
			j := shellVar(src, i)
			emit(i, j, Param)
			i = max(j, i+1) // a bare $ (a prompt, $(…)) is plain text
		case strings.HasPrefix(src[i:], "<<<"):
			// a here-string, whose word is just an argument; consumed
			// whole so its last two <s are not then read as a heredoc
			i += 3
		case strings.HasPrefix(src[i:], "<<"):
			if term, ws, j, ok := heredocOpen(src, i); ok {
				emit(ws, j, String)
				heredoc = term
				i = j
			} else {
				i += 2
			}
		case isShellWord(c):
			j := i
			for j < n && isShellWord(src[j]) {
				j++
			}
			if shellKeywords[src[i:j]] {
				emit(i, j, Keyword)
			}
			i = j
		default:
			i++
		}
	}
	return out
}

// isShellWord is a byte of an unquoted word. Besides identifier bytes it
// takes the punctuation that sits inside words — flags, paths, assignments,
// image tags — so a keyword only matches as a whole word. Non-ASCII counts,
// keeping runes whole as in spec.isWord.
func isShellWord(c byte) bool {
	return c == '_' || c >= 0x80 || isDigit(c) ||
		('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') ||
		strings.IndexByte("-./:=+@%,~^", c) >= 0
}

// shellWordStart is true where a new word may begin: the start of the
// source, or after whitespace or an operator.
func shellWordStart(src string, i int) bool {
	return i == 0 || strings.IndexByte(" \t\r\n;|&()", src[i-1]) >= 0
}

// scanShellQuote scans a '…' or "…" from its opening quote. Both may span
// lines in shell, and do in practice (psql -c "\n SELECT …\n"); only "…"
// honors backslash escapes. When the quote never closes — most often a
// stray apostrophe in a console block's output, as in "doesn't" — the
// string ends at its own line instead, so the mistake colors one line, not
// the rest of the block.
func scanShellQuote(src string, i int) int {
	q := src[i]
	for j := i + 1; j < len(src); j++ {
		switch src[j] {
		case '\\':
			if q == '"' {
				j++
			}
		case q:
			return j + 1
		}
	}
	if nl := strings.IndexByte(src[i:], '\n'); nl >= 0 {
		return i + nl
	}
	return len(src)
}

// shellVar returns the end of the variable at src[i] == '$', or i when
// none starts there: $NAME, ${…} (to its brace, or the line's end when
// unclosed), or a one-character special ($1, $?, $#, $@, $!, $$, $*, $-).
func shellVar(src string, i int) int {
	if i+1 >= len(src) {
		return i
	}
	c := src[i+1]
	switch {
	case c == '{':
		for j := i + 2; j < len(src); j++ {
			if src[j] == '}' {
				return j + 1
			}
			if src[j] == '\n' {
				return j
			}
		}
		return len(src)
	case c == '_' || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z'):
		j := i + 2
		for j < len(src) && (src[j] == '_' || isDigit(src[j]) ||
			('a' <= src[j] && src[j] <= 'z') || ('A' <= src[j] && src[j] <= 'Z')) {
			j++
		}
		return j
	case isDigit(c) || strings.IndexByte("?#@!$*-", c) >= 0:
		return i + 2
	}
	return i
}

// heredocOpen parses a heredoc opener at src[i:] == "<<": an optional -
// (strip leading tabs), optional blanks, and a word that may be quoted
// ('EOF' or "EOF" — quoting only turns off expansion, the terminator is
// the bare word). It returns the terminator, where the word starts and
// ends, and whether this was a heredoc at all.
func heredocOpen(src string, i int) (term string, wordStart, end int, ok bool) {
	j := i + 2
	if j < len(src) && src[j] == '-' {
		j++
	}
	for j < len(src) && (src[j] == ' ' || src[j] == '\t') {
		j++
	}
	wordStart = j
	var q byte
	if j < len(src) && (src[j] == '\'' || src[j] == '"') {
		q = src[j]
		j++
	}
	// a terminator must start like a name: 1<<2 inside $((…)) is a shift,
	// not a heredoc ended by a line reading "2"
	if j >= len(src) || isDigit(src[j]) {
		return "", 0, 0, false
	}
	k := j
	for k < len(src) && (src[k] == '_' || isDigit(src[k]) ||
		('a' <= src[k] && src[k] <= 'z') || ('A' <= src[k] && src[k] <= 'Z')) {
		k++
	}
	if k == j {
		return "", 0, 0, false
	}
	term, end = src[j:k], k
	if q != 0 {
		if k >= len(src) || src[k] != q {
			return "", 0, 0, false
		}
		end++
	}
	return term, wordStart, end, true
}

// heredocEnd returns the end (newline excluded) of the first line from
// start whose text is term, or the end of src for an unclosed heredoc.
// Surrounding blanks are ignored, which is laxer than bash (it strips
// only tabs, and only for <<-) but right for answers, whose code a model
// often indents.
func heredocEnd(src string, start int, term string) int {
	for at := start; at < len(src); {
		end := strings.IndexByte(src[at:], '\n')
		if end < 0 {
			end = len(src)
		} else {
			end += at
		}
		if strings.TrimSpace(src[at:end]) == term {
			return end
		}
		at = end + 1
	}
	return len(src)
}
