package sqlsplit

import "strings"

// body follows one statement through the scan, for the statements whose
// own text holds semicolons that do not end them: a compound body, and a
// rule's action list.
//
//	CREATE TRIGGER del AFTER DELETE ON orders BEGIN    ┐
//	  DELETE FROM order_items WHERE order_id = old.id; │ one statement:
//	  UPDATE stats SET n = n - 1;                      │ the ; inside
//	END;                                               ┘ BEGIN … END hold
//
//	CREATE RULE r AS ON INSERT TO t DO ALSO (INSERT INTO a VALUES (1); INSERT INTO b VALUES (2));
//	                                        └──── the ; inside ( … ) holds ────┘
//
// Cutting such a statement at its inner ; sends the server the head alone
// (`CREATE TRIGGER … BEGIN DELETE FROM order_items …`, refused) and then
// each body statement on its own — run for real under --keep-going — so
// none of these objects could be created from the editor or `dbc -f`.
//
// Which statements: the head decides, read off the statement's first
// words (head):
//
//	CREATE [OR REPLACE] [TEMP|TEMPORARY] [CONSTRAINT] [AGGREGATE]
//	       [DEFINER = user]  TRIGGER | PROCEDURE | FUNCTION | EVENT  → blocks
//	CREATE [OR REPLACE] RULE                                         → parens
//	anything else                                                    → nothing
//
// That covers SQLite's and MySQL's triggers (BEGIN … END), MySQL's
// procedures, functions and events, and Postgres's SQL-standard bodies
// (BEGIN ATOMIC … END). A Postgres body in a dollar quote or a string is
// opaque to the scan already, so it never shows the tracker a BEGIN.
//
// Blocks are counted by their words, as a stack:
//
//	BEGIN (outside parens)  push b, when a word or nothing yet follows it
//	                        (not an operator or a ;: IF begin > 0, SET x =
//	                        begin; — that is a name)
//	CASE                    push c (an expression or MySQL's CASE statement)
//	END IF|LOOP|WHILE|REPEAT  nothing: closes a block the stack does not hold
//	END, c on top           pop: closes a CASE (END or END CASE)
//	END, b on top           pop when a ; (or the end) follows, past an
//	                        optional label — END; END lbl; — and not when
//	                        CASE or (MariaDB's) FOR does
//
// MySQL's IF, LOOP, WHILE and REPEAT are not pushed: their END is always
// followed by their own word, so skipping both keeps the stack even, and
// IF has a function form (IF(a, b, c)) a word count could not tell apart.
// The ; rule on END, b keeps a column called end (`INSERT INTO log
// (start, end)`, `SELECT start, end FROM …`) from closing the body: the
// END that closes a block is followed by its ; — or by a label and then
// its ;.
//
// Misreading is cheap in one direction only, and the rules lean that way.
// A statement taken for a body that has none (a view whose name is
// "function") is unharmed: valid SQL closes every CASE it opens, and no
// BEGIN means nothing to hold. A body whose END is missed merges what
// follows into the statement, which the server then refuses — the same
// as an unclosed string. Only a stray BEGIN or END could still cut a body
// short, and that is what the scan did to every body before.
type body struct {
	st     bodyState
	stack  []byte // open blocks, innermost last: 'b' BEGIN, 'c' CASE
	parens int    // depth of ( … ) in the statement
	words  int    // words read in the DEFINER clause, to give up on a long one
	// endCase is 1 + the offset of the CASE in an END CASE (0: none): that
	// CASE ends a block, so it must not open one
	endCase int
}

type bodyState uint8

const (
	bodyFirst   bodyState = iota // no code yet: the first word decides
	bodyCreate                   // after CREATE, reading its modifiers
	bodyDefiner                  // inside CREATE DEFINER = user
	bodyNone                     // not a statement with a body
	bodyBlocks                   // a routine or trigger: BEGIN/CASE … END
	bodyParens                   // a rule: ( … )
)

// definerWords is how many words a DEFINER clause is given before the
// head stops looking for its object: user@host, quoted or not, or
// CURRENT_USER — two at most, with a margin.
const definerWords = 4

// reset readies b for the next statement.
func (b *body) reset() {
	*b = body{stack: b.stack[:0]}
}

// active reports whether b still wants to see the statement's words and
// brackets: while it decides, and in a statement with a body. Once it has
// decided there is none, the scan skips it.
func (b *body) active() bool { return b.st != bodyNone }

// holds reports whether a ; here belongs to the statement rather than
// ending it.
func (b *body) holds() bool {
	switch b.st {
	case bodyBlocks:
		return len(b.stack) > 0
	case bodyParens:
		return b.parens > 0
	}
	return false
}

// code is any code that is not a word: a string, a quoted name, an
// operator. As the statement's first code it means no head of interest.
func (b *body) code() {
	if b.st == bodyFirst {
		b.st = bodyNone
	}
}

// open and close follow ( and ). A statement that opens with one is not a
// CREATE.
func (b *body) open() {
	b.code()
	b.parens++
}

func (b *body) close() {
	b.code()
	if b.parens > 0 {
		b.parens--
	}
}

// word takes the word sql[start:end]; qualified says it follows a '.' (a
// column or schema-qualified name: old.end), ':', '@' or '$' (a parameter
// or variable), so it is no keyword whatever it spells. d and delim are
// the scan's, for END's look past itself.
func (b *body) word(sql string, start, end int, qualified bool, d Dialect, delim string) {
	w := sql[start:end]
	switch b.st {
	case bodyFirst:
		if strings.EqualFold(w, "create") && !qualified {
			b.st = bodyCreate
		} else {
			b.st = bodyNone
		}
	case bodyCreate:
		switch strings.ToLower(w) {
		case "or", "replace", "temp", "temporary", "constraint", "aggregate":
		case "definer":
			b.st = bodyDefiner
		case "trigger", "procedure", "function", "event":
			b.st = bodyBlocks
		case "rule":
			b.st = bodyParens
		default:
			b.st = bodyNone // TABLE, VIEW, INDEX, …
		}
	case bodyDefiner:
		// the user is words, strings or quoted names around an @; only
		// the object word ends it
		switch strings.ToLower(w) {
		case "trigger", "procedure", "function", "event":
			b.st = bodyBlocks
		default:
			if b.words++; b.words > definerWords {
				b.st = bodyNone // CREATE DEFINER = … VIEW, or no object
			}
		}
	case bodyBlocks:
		if qualified {
			return
		}
		switch {
		case strings.EqualFold(w, "begin"):
			// a BEGIN inside parens is a name (a parameter called begin),
			// and so is one an operator or a comma follows (IF begin > 0)
			if b.parens == 0 && opensBlock(sql, end, d) {
				b.stack = append(b.stack, 'b')
			}
		case strings.EqualFold(w, "case"):
			if start+1 != b.endCase {
				b.stack = append(b.stack, 'c')
			}
		case strings.EqualFold(w, "end"):
			n := len(b.stack)
			if n == 0 {
				return
			}
			after, next := wordAfter(sql, end, d)
			switch next {
			case "if", "loop", "while", "repeat":
				return // closes a block the stack does not hold
			case "case":
				b.endCase = after + 1
			}
			if b.stack[n-1] == 'c' || closesBlock(sql, after, next, d, delim) {
				b.stack = b.stack[:n-1]
			}
		}
	}
}

// opensBlock reports whether the BEGIN just before sql[at:] opens a block:
// what follows it, past blanks and comments, is a word (its first
// statement, ATOMIC, or the END of an empty block), or nothing yet — a
// body still being typed. Anything else makes it a name: an operator, a
// comma, a bracket (IF begin > 0), or a ; (SET x = begin;). Inside a body
// a BEGIN is never a transaction's, so `BEGIN;` is no block there either.
func opensBlock(sql string, at int, d Dialect) bool {
	i := codeAt(sql, at, d)
	return i == len(sql) || isWordByte(sql[i])
}

// wordAfter returns the offset of the first code past sql[at:] (past
// blanks and comments) and the word there, lowercased: "" when the code
// there is no word.
func wordAfter(sql string, at int, d Dialect) (int, string) {
	i := codeAt(sql, at, d)
	j := i
	for j < len(sql) && (isWordByte(sql[j]) || sql[j] == '$') {
		j++
	}
	return i, strings.ToLower(sql[i:j])
}

// closesBlock reports whether an END closes the BEGIN block on top of the
// stack, given what follows it: the code at sql[at:] and the word there
// (wordAfter's). It does when that is the statement's end (a ;, the
// scan's delimiter, the buffer's end), or a label and then that end. END
// CASE closes a CASE, so with a BEGIN on top it means a CASE the stack
// lost, and MariaDB's END FOR a loop it never held; neither closes the
// block.
func closesBlock(sql string, at int, next string, d Dialect, delim string) bool {
	ends := func(i int) bool {
		return i == len(sql) || sql[i] == ';' || strings.HasPrefix(sql[i:], delim)
	}
	switch {
	case ends(at):
		return true
	case next == "", next == "case", next == "for":
		return false // END, … or END) …: a column called end
	}
	return ends(codeAt(sql, at+len(next), d)) // END label;
}

// delimiterCmd reports whether a MySQL client DELIMITER command sits at
// sql[i], i being the first non-blank of its line: the word DELIMITER (any
// case), blanks, and the new delimiter, which runs to the next blank.
// It returns that delimiter and the offset past the command's line, its
// newline included — the rest of the line is the client's, not SQL.
//
// The mysql client reads the command only at the start of a line and of a
// statement (nothing of one typed yet), and refuses a delimiter holding a
// backslash; the caller checks the first two, and a refused one is left
// to be sent as code, for the server to refuse.
func delimiterCmd(sql string, i int) (string, int, bool) {
	const word = "delimiter"
	if len(sql)-i <= len(word) || !strings.EqualFold(sql[i:i+len(word)], word) {
		return "", 0, false
	}
	j := i + len(word)
	if sql[j] != ' ' && sql[j] != '\t' {
		return "", 0, false // DELIMITERS, or DELIMITER with nothing after
	}
	for j < len(sql) && (sql[j] == ' ' || sql[j] == '\t') {
		j++
	}
	k := j
	for k < len(sql) && !isSpace(sql[k]) {
		k++
	}
	if k == j || strings.IndexByte(sql[j:k], '\\') >= 0 {
		return "", 0, false
	}
	next := len(sql)
	if nl := strings.IndexByte(sql[k:], '\n'); nl >= 0 {
		next = k + nl + 1
	}
	return sql[j:k], next, true
}

// atLineStart reports whether only blanks stand between the start of
// sql[i]'s line and i.
func atLineStart(sql string, i int) bool {
	for j := i - 1; j >= 0; j-- {
		switch sql[j] {
		case '\n':
			return true
		case ' ', '\t', '\r':
		default:
			return false
		}
	}
	return true
}
