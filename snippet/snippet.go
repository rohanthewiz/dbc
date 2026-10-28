// Package snippet parses and expands live snippets: named templates with
// tab stops, typed into the SQL editor by name (then Tab) or picked from a
// list, in the terminal UI and in dbc web alike.
//
// THE TEMPLATE SYNTAX. Everything is literal text except these:
//
//	$VAR1$          a tab stop. Tab visits the stops by number (1, 2, …), not
//	                by where they sit; the same number used twice is ONE stop
//	                shown twice, and typing into it fills both ("mirrors").
//	$VAR2:id$       a stop with default text, inserted already selected:
//	                typing replaces it, Tab keeps it. Of a stop's uses, the
//	                first one giving a default sets it for all of them.
//	$SELECTION$     the text that was selected when the snippet was picked
//	                ("" when nothing was) — a "surround with" snippet.
//	$END$           where the caret goes after the last stop; the end of the
//	                snippet when there is none.
//	$$              a literal "$".
//
// Any other "$" is literal, so a Postgres parameter ($1) or a dollar-quote
// tag ($body$) needs no escaping; only a literal "$$" does ($$$$), because
// "$$" is the escape.
//
// WHY ONE EXPANDER FOR BOTH UIS. The terminal's editor is dbc's own, and
// tracks the stops itself (tui/snippets.go); dbc web hands Monaco a snippet
// in Monaco's own syntax (Expansion.Monaco). Both start from the same
// Expansion, so the indentation, the placement of a multi-line selection
// and the stop order are decided once, here, and cannot drift apart.
package snippet

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// NameMax bounds a snippet's name: it is typed as a word, so a long one is
// a mistake, and the pickers' name column is sized to it.
const NameMax = 40

var nameRe = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// ValidName reports why name cannot name a snippet, or nil. A name is what
// is typed before Tab, and the editors find it as the word before the
// caret, so it must be one word by the editors' rule: letters, digits and
// '_' — a '-' or '.' would end the word early and never match.
func ValidName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("a snippet needs a name")
	case len(name) > NameMax:
		return fmt.Errorf("a snippet name is at most %d characters", NameMax)
	case !nameRe.MatchString(name):
		return fmt.Errorf("a snippet name is letters, digits and '_' only (it is typed as one word, then Tab)")
	}
	return nil
}

// pieceKind is what one piece of a parsed template is.
type pieceKind int

const (
	pText      pieceKind = iota // literal text ("$$" already undone)
	pVar                        // a $VARn$ stop
	pSelection                  // $SELECTION$
	pEnd                        // $END$
)

type piece struct {
	kind pieceKind
	text string // pText: the text; pVar: its default ("" for none)
	n    int    // pVar: the stop number
	dflt bool   // pVar: a default was written ($VAR1:$ gives an empty one)
}

// Template is a parsed snippet body.
type Template struct {
	pieces []piece
}

// UsesSelection reports whether the template has a $SELECTION$: the
// pickers mark such a snippet, since it means most with text selected.
func (t Template) UsesSelection() bool {
	return slices.ContainsFunc(t.pieces, func(p piece) bool { return p.kind == pSelection })
}

// Parse reads a snippet body. It fails only on what is plainly a broken
// stop — "$VAR" and digits with no closing "$", a number of 0 — or on a
// second $END$, so a save can refuse those with the reason; anything else
// with a "$" in it is literal text (see the package comment).
func Parse(body string) (Template, error) {
	var t Template
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			t.pieces = append(t.pieces, piece{kind: pText, text: lit.String()})
			lit.Reset()
		}
	}
	ends := 0
	for i := 0; i < len(body); {
		if body[i] != '$' {
			// copy up to the next '$' in one go: most of a body is plain
			j := strings.IndexByte(body[i:], '$')
			if j < 0 {
				j = len(body) - i
			}
			lit.WriteString(body[i : i+j])
			i += j
			continue
		}
		rest := body[i:]
		switch {
		case strings.HasPrefix(rest, "$$"):
			lit.WriteByte('$')
			i += 2
		case strings.HasPrefix(rest, "$SELECTION$"):
			flush()
			t.pieces = append(t.pieces, piece{kind: pSelection})
			i += len("$SELECTION$")
		case strings.HasPrefix(rest, "$END$"):
			if ends++; ends > 1 {
				return Template{}, fmt.Errorf("$END$ appears more than once: the caret can end in one place only")
			}
			flush()
			t.pieces = append(t.pieces, piece{kind: pEnd})
			i += len("$END$")
		case isVarStart(rest):
			p, n, err := parseVar(rest)
			if err != nil {
				return Template{}, err
			}
			flush()
			t.pieces = append(t.pieces, p)
			i += n
		default:
			lit.WriteByte('$')
			i++
		}
	}
	flush()
	return t, nil
}

// isVarStart reports whether s starts "$VAR" and a digit: from there on it
// can only be a stop, and a malformed one is an error rather than text.
// "$VAR$" or "$VARIANT" stay literal — a dollar-quote tag may be named so.
func isVarStart(s string) bool {
	return strings.HasPrefix(s, "$VAR") && len(s) > 4 && s[4] >= '0' && s[4] <= '9'
}

// parseVar reads "$VARn$" or "$VARn:default$" at the start of s and
// returns the piece and how many bytes it took. In a default, "$$" is a
// literal "$" as everywhere else, and the first lone "$" closes it.
func parseVar(s string) (piece, int, error) {
	i := 4
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	num := s[4:i]
	n, err := strconv.Atoi(num)
	if err != nil || n < 1 || n > 999 {
		return piece{}, 0, fmt.Errorf("$VAR%s: stops are numbered from 1 to 999", num)
	}
	p := piece{kind: pVar, n: n}
	if i < len(s) && s[i] == '$' {
		return p, i + 1, nil
	}
	if i >= len(s) || s[i] != ':' {
		return piece{}, 0, fmt.Errorf("$VAR%d is not closed: write $VAR%d$, or $VAR%d:default$", n, n, n)
	}
	p.dflt = true
	var def strings.Builder
	for j := i + 1; j < len(s); j++ {
		if s[j] != '$' {
			def.WriteByte(s[j])
			continue
		}
		if j+1 < len(s) && s[j+1] == '$' {
			def.WriteByte('$')
			j++
			continue
		}
		if strings.Contains(def.String(), "\n") {
			return piece{}, 0, fmt.Errorf("$VAR%d's default runs over a line end: a default is one line", n)
		}
		p.text = def.String()
		return p, j + 1, nil
	}
	return piece{}, 0, fmt.Errorf("$VAR%d:… is not closed: its default ends at a \"$\"", n)
}

// Context is where a snippet is being inserted.
type Context struct {
	// Selection is the text the snippet replaces and $SELECTION$ stands
	// for; "" when nothing was selected.
	Selection string
	// SelPrefix is the text on the selection's first line before it. When
	// it is blank the selection started in its line's indentation, so that
	// line's indent counts when the selection's lines are re-indented (see
	// placeSelection); otherwise the selection started mid-line and only
	// its later lines' indents do.
	SelPrefix string
	// Indent is the leading whitespace of the line the snippet goes on:
	// every line of the body after the first gets it, so a snippet typed
	// inside an indented block stays inside it.
	Indent string
}

// Span is a byte range [From, To) of Expansion.Text.
type Span struct{ From, To int }

// Stop is one tab stop of an expansion: all of its uses, in text order.
// The first use is where Tab lands; the others mirror it.
type Stop struct {
	N       int
	Default string
	Spans   []Span
}

// Expansion is a template expanded in a Context: the text to insert and,
// as offsets into it, its stops and where the caret ends.
type Expansion struct {
	Text  string
	Stops []Stop // by number, ascending: the order Tab visits them
	End   int    // the caret after the last stop ($END$, else the end)
}

// Expand fills t in for ctx.
//
// A selection ending in a newline (whole lines picked) has that newline
// kept after the expansion rather than inside it: the snippet replaced the
// newline too, and without it the line after the selection would be pulled
// up onto the snippet's last line. End stays before it.
func Expand(t Template, ctx Context) Expansion {
	sel, trailer := ctx.Selection, ""
	if cut, ok := strings.CutSuffix(sel, "\n"); ok {
		sel, trailer = cut, "\n"
	}

	// A stop's default is the first one written among its uses, so the
	// later uses — usually bare $VARn$ — show the same text.
	defaults := map[int]string{}
	for _, p := range t.pieces {
		if p.kind == pVar && p.dflt {
			if _, ok := defaults[p.n]; !ok {
				defaults[p.n] = p.text
			}
		}
	}

	var b strings.Builder
	stops := map[int]*Stop{}
	end := -1
	// The body's own newlines carry the caret line's indent; a default is
	// one line (Parse), and a selection is placed by its own rule.
	writeBody := func(s string) { b.WriteString(strings.ReplaceAll(s, "\n", "\n"+ctx.Indent)) }
	for _, p := range t.pieces {
		switch p.kind {
		case pText:
			writeBody(p.text)
		case pSelection:
			b.WriteString(placeSelection(sel, ctx.SelPrefix, lineIndent(b.String())))
		case pEnd:
			end = b.Len()
		case pVar:
			st := stops[p.n]
			if st == nil {
				st = &Stop{N: p.n, Default: defaults[p.n]}
				stops[p.n] = st
			}
			from := b.Len()
			b.WriteString(st.Default)
			st.Spans = append(st.Spans, Span{from, b.Len()})
		}
	}
	x := Expansion{Text: b.String(), End: end}
	if x.End < 0 {
		x.End = len(x.Text)
	}
	x.Text += trailer
	for _, st := range stops {
		x.Stops = append(x.Stops, *st)
	}
	slices.SortFunc(x.Stops, func(a, b Stop) int { return a.N - b.N })
	return x
}

// lineIndent is the leading whitespace of the last line of s — the line
// the next write lands on.
func lineIndent(s string) string {
	line := s[strings.LastIndexByte(s, '\n')+1:]
	return line[:len(line)-len(strings.TrimLeft(line, " \t"))]
}

// placeSelection lays a multi-line selection out at a $SELECTION$ whose
// line is indented by at: its lines keep their indentation relative to each
// other, with the least-indented one at the placeholder's.
//
//	selection (from column 4)   body                 result
//	    SELECT a                WITH q AS (          WITH q AS (
//	      FROM t                    $SELECTION$          SELECT a
//	    WHERE x                 )                          FROM t
//	                                                     WHERE x
//	                                                 )
//
// The first line goes where $SELECTION$ is, so its own leading blanks come
// off (keeping any it has beyond the common indent, when the selection
// started in the indentation — SelPrefix blank). A single line is
// inserted as it is, and blank lines stay empty rather than gain trailing
// blanks.
func placeSelection(sel, prefix, at string) string {
	lines := strings.Split(sel, "\n")
	if len(lines) == 1 {
		return sel
	}
	blankPrefix := strings.TrimSpace(prefix) == ""
	common := -1
	consider := func(n int) {
		if common < 0 || n < common {
			common = n
		}
	}
	first := lead(lines[0])
	if blankPrefix && strings.TrimSpace(lines[0]) != "" {
		first += len([]rune(prefix))
		consider(first)
	}
	for _, l := range lines[1:] {
		if strings.TrimSpace(l) != "" {
			consider(lead(l))
		}
	}
	common = max(common, 0)

	var b strings.Builder
	if blankPrefix {
		b.WriteString(strings.Repeat(" ", max(first-common, 0)))
		b.WriteString(strings.TrimLeft(lines[0], " \t"))
	} else {
		b.WriteString(lines[0])
	}
	for _, l := range lines[1:] {
		b.WriteByte('\n')
		if strings.TrimSpace(l) == "" {
			continue
		}
		b.WriteString(at)
		b.WriteString(string([]rune(l)[common:]))
	}
	return b.String()
}

// lead counts a line's leading blanks, a tab as one: the editors store
// spaces (the TUI turns tabs into four), so a mix is rare, and counting a
// tab as one keeps the cut on a rune boundary.
func lead(l string) int {
	return len([]rune(l)) - len([]rune(strings.TrimLeft(l, " \t")))
}

// Monaco renders x in the snippet syntax Monaco's snippet controller
// inserts, so dbc web runs this same expansion with Monaco's own stop
// handling: a stop's uses become ${n:text} (Monaco mirrors equal numbers),
// End becomes $0, and the text between is escaped ("$", "}" and "\" each
// take a backslash).
//
// Insert it with adjustWhitespace off: the indentation is already in the
// text, and Monaco re-indenting it again would push the lines over twice.
func (x Expansion) Monaco() string {
	type mark struct {
		at   int
		end  int // the span's end; = at for $0
		n    int // 0 for End
		text string
	}
	var marks []mark
	for _, st := range x.Stops {
		for _, sp := range st.Spans {
			marks = append(marks, mark{sp.From, sp.To, st.N, x.Text[sp.From:sp.To]})
		}
	}
	marks = append(marks, mark{at: x.End, end: x.End})
	// Spans never overlap, so start order is text order. At a tie the
	// shorter goes first: only an empty mark ($0, or a stop with no
	// default) can share its start with another, and it must be written
	// before the text of one that starts there ("$END$$VAR1:x$" is $0 then
	// ${1:x}); two empty marks may go either way.
	slices.SortStableFunc(marks, func(a, b mark) int {
		if a.at != b.at {
			return a.at - b.at
		}
		return a.end - b.end
	})

	var b strings.Builder
	at := 0
	for _, mk := range marks {
		b.WriteString(monacoEscape(x.Text[at:mk.at]))
		switch {
		case mk.n == 0:
			b.WriteString("$0")
		case mk.text == "":
			b.WriteString("${" + strconv.Itoa(mk.n) + "}")
		default:
			b.WriteString("${" + strconv.Itoa(mk.n) + ":" + monacoEscape(mk.text) + "}")
		}
		at = mk.end
	}
	b.WriteString(monacoEscape(x.Text[at:]))
	return b.String()
}

var monacoEscaper = strings.NewReplacer(`\`, `\\`, `$`, `\$`, `}`, `\}`)

func monacoEscape(s string) string { return monacoEscaper.Replace(s) }
