package sqlsplit

import "strings"

// Token classes for syntax highlighting.
//
// This is deliberately the SAME scanner the splitter uses (skipQuoted,
// skipBlockComment, dollarTag…), not a second lexer beside it. The editor
// both colors the buffer and runs "the statement under the cursor"; if the
// two disagreed about where a string or a comment ends, a user would see a
// semicolon drawn as code that the splitter treats as inside a string, and
// the statement that ran would not be the one on screen. One scanner makes
// that impossible.
//
// Like the splitter it is lexical only: a keyword is a word on a fixed list,
// not a parse. Highlighting `select` as a keyword inside an identifier
// position is a cost worth paying for a scanner with no grammar to maintain.

// TokenKind classifies a span of a SQL buffer.
type TokenKind int

const (
	TokText    TokenKind = iota // anything not classified below
	TokKeyword                  // a reserved word
	TokString                   // '…' or a dollar-quoted body
	TokIdent                    // "…" or `…` quoted identifier
	TokNumber                   // a numeric literal
	TokComment                  // -- … or /* … */
	TokParam                    // $1, ?, :name
)

// Token is one classified span, as byte offsets into the buffer.
type Token struct {
	Start, End int
	Kind       TokenKind
}

// Lex classifies every non-text span of sql. Spans not covered by a token
// are TokText; returning only the interesting ones keeps the slice short for
// the per-frame redraw that calls this.
func Lex(sql string) []Token {
	var out []Token
	emit := func(start, end int, k TokenKind) {
		if end > start {
			out = append(out, Token{Start: start, End: end, Kind: k})
		}
	}
	i := 0
	for i < len(sql) {
		c := sql[i]
		switch {
		case isLineCommentAt(sql, i):
			j := skipLineComment(sql, i)
			// the newline that ends a line comment is not part of it
			end := j
			if end > i && sql[end-1] == '\n' {
				end--
			}
			emit(i, end, TokComment)
			i = j
		case isBlockCommentAt(sql, i):
			j := skipBlockComment(sql, i)
			emit(i, j, TokComment)
			i = j
		case c == '\'':
			j := skipQuoted(sql, i, c)
			emit(i, j, TokString)
			i = j
		case c == '"', c == '`':
			j := skipQuoted(sql, i, c)
			emit(i, j, TokIdent)
			i = j
		case c == '$':
			if tag, ok := dollarTag(sql, i); ok {
				j := skipDollarQuoted(sql, i, tag)
				emit(i, j, TokString)
				i = j
				continue
			}
			j := i + 1
			for j < len(sql) && isDigit(sql[j]) {
				j++
			}
			emit(i, j, TokParam)
			i = j
		case c == '?':
			emit(i, i+1, TokParam)
			i++
		case c == ':' && i+1 < len(sql) && isWordByte(sql[i+1]) && !isDigit(sql[i+1]) &&
			(i == 0 || sql[i-1] != ':'):
			// :name is a named parameter; ::type is a Postgres cast and
			// is left as text
			j := i + 1
			for j < len(sql) && isWordByte(sql[j]) {
				j++
			}
			emit(i, j, TokParam)
			i = j
		case isDigit(c) && (i == 0 || !isWordByte(sql[i-1])):
			j := i
			for j < len(sql) && (isDigit(sql[j]) || sql[j] == '.' || sql[j] == 'e' || sql[j] == 'E') {
				j++
			}
			emit(i, j, TokNumber)
			i = j
		case isWordByte(c):
			j := i
			for j < len(sql) && isWordByte(sql[j]) {
				j++
			}
			if keywords[strings.ToLower(sql[i:j])] {
				emit(i, j, TokKeyword)
			}
			i = j
		default:
			i++
		}
	}
	return out
}

// keywords is the highlighted vocabulary: the common reserved words of the
// four dialects dbc speaks. Not exhaustive on purpose — a word missing here
// is merely uncolored, while a type name colored as a keyword everywhere it
// appears as a column would be noise.
var keywords = func() map[string]bool {
	m := map[string]bool{}
	for w := range strings.FieldsSeq(`
		select from where and or not in is null like ilike between exists
		insert into values update set delete returning
		create alter drop table view index sequence schema database if
		primary key foreign references unique check default constraint
		join inner left right full outer cross on using natural
		group by order having limit offset fetch first next rows only
		union all intersect except distinct as asc desc nulls last
		case when then else end cast
		begin commit rollback transaction savepoint release
		with recursive over partition window
		grant revoke explain analyze vacuum pragma show describe
		true false`) {
		m[w] = true
	}
	return m
}()
