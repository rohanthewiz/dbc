package tui

import (
	"slices"
	"sort"

	"github.com/rohanthewiz/dbc/sqlsplit"
)

// The editor's syntax coloring, kept per row and lexed as far down as a
// frame draws.
//
// It used to be the whole buffer lexed on every edit, with a Style made for
// every rune: on a 3.5 MB migration ~40 ms and 42 MB of allocation per
// keystroke (N-279). Now each row keeps its tokens, as rune columns, and
// where the lexer stands at the row's start (hlIn). An edit drops what it
// can have changed, and the next frame lexes from the first such row down
// to the last row it draws, no further:
//
//	row   in         lexed     after an edit to row 5, drawing rows 3–8
//	 0    ·          ✓         kept
//	 …
//	 4    ·          ✓         lexed again (q: the row above the edit)
//	 5    ·          ✓         lexed again
//	 6    in /* (5)  ✓         lexed again; its old tokens were for a
//	 7    ·          ✓  ┐      different "in" (5's comment is new)
//	 8    ·          ✓  ├ tentative: kept from before, moved with the
//	 9    ·          ✓  ┘ rows; confirmed when the lexer reaches 7 with
//	                         the "in" they were lexed from, then skipped
//
// Where the lexer stands (hlIn) is "between tokens" (·) or "inside a token
// begun back rows up, at byte col of that row" — a comment, string or
// dollar body that runs over line breaks. A row's tokens depend only on
// that and on its own text, since nothing in the lexer looks past the
// row's newline from inside the row (see shift for the one place a row's
// end changes); so a tentative row whose recorded "in" is reached again
// between tokens is as it was, and so is every row after it that was
// lexed in the same pass. A row reached inside a token is not taken on
// trust: two tokens begun at the same place can still differ past an edit
// (a nested comment's depth), so the lexer goes on until it is between
// tokens again.
//
// To resume inside a token the lexer starts at the token's own start —
// sqlsplit.LexRange takes any byte the whole scan stops at — and reads the
// whole of it again: a long function body costs its own length per edit,
// never the buffer's.

// hlIn is where the lexer stands at a row's start: between tokens (the
// zero value), or inside a token that began back rows up, col bytes into
// that row.
type hlIn struct {
	inTok bool
	back  int
	col   int
}

// hlTok is one colored span of a row, in rune columns [from, to).
type hlTok struct {
	from, to int32
	kind     sqlsplit.TokenKind
}

// hlRow is one row's coloring.
type hlRow struct {
	// in is where the lexer stands at the row's start: found by lexing the
	// row above, which always sets it (row 0's is "between tokens"). A row
	// is only ever reached by the lexer from the row above, or at the end
	// of a run of lexed rows (spread), so its in has always been set when
	// it is read: a blank row from shift follows the row shift keeps or
	// another blank row, never a lexed one.
	in hlIn
	// lexed: toks are the row's tokens, lexed from in. Certain below
	// hlCache.done; past it, tentative (kept from before an edit).
	lexed bool
	toks  []hlTok
}

// hlCache is the editor's coloring for the rows it was lexed over.
type hlCache struct {
	ok   bool
	d    sqlsplit.Dialect
	rows [][]rune // the rows (an editor snapshot) ln describes
	ln   []hlRow
	// done: rows [0, done) are lexed for certain, and ln[done].in is
	// certain too (when done < len(ln)) — where the next lexing begins.
	done int
}

// highlight brings the coloring up to date with the buffer and lexed down
// to row upto, and returns it.
func (e *editor) highlight(upto int) *hlCache {
	h := &e.hl
	now := e.snap()
	switch {
	case !h.ok || h.d != e.dialect:
		*h = hlCache{ok: true, d: e.dialect, rows: now, ln: make([]hlRow, len(now))}
	case len(h.rows) != len(now) || (len(now) > 0 && &h.rows[0] != &now[0]):
		// another snapshot: the buffer was edited since
		p, s := rowsDiff(h.rows, now)
		h.shift(p, s, len(now))
		h.rows = now
	}
	if upto = min(upto, len(now)-1); h.done <= upto {
		h.lex(e, upto)
	}
	return h
}

// shift fits ln to an edit that left the first p and last s rows alone, of
// nowN rows now. Rows from q on are dropped from the certain part, q being
// the row above the edit: a row's tokens can read its newline (a MySQL
// backslash before it, a -- needing a blank after it), and when the edit
// added or took away the last line, the row above it gained or lost that
// newline. The last s rows keep their tokens, tentatively, moved with
// them.
func (h *hlCache) shift(p, s, nowN int) {
	oldN := len(h.ln)
	q := max(p-1, 0)
	// where q begins depends on the rows above it alone, which the edit
	// left as they were: kept, its tokens dropped. Past done it is as
	// tentative as the row above it, and stands or falls with that row
	// (lexing the row above always sets the "in" of the row below, so the
	// two were found together).
	keep := hlRow{} // row 0 begins between tokens
	if q > 0 {
		keep = hlRow{in: h.ln[q].in}
	}
	// the tail starts below q in every case: when p is 0 and every row is
	// in the tail (rows taken off the top), row 0 is q, not a tail row.
	// The rows between become blank ones, in place: a new slice of every
	// row per edit was 4 MB a keystroke on a 70,000-line buffer.
	//
	//	old  │ head [0,q) │ dropped [q, oldTailFrom) │ tail │
	//	new  │ head [0,q) │ keep, blanks [q, tailFrom) │ tail │
	//
	// The dropped range can be empty: lines added at the very top leave
	// every old row in the tail, row 0 included.
	tailFrom := max(nowN-s, q+1)
	oldTailFrom := oldN - (nowN - tailFrom)
	fresh := make([]hlRow, tailFrom-q)
	fresh[0] = keep
	h.ln = slices.Replace(h.ln, q, oldTailFrom, fresh...)
	h.done = min(h.done, q)
}

// lex lexes from row done down to row upto, skipping the tentative rows it
// can confirm (see the top of the file).
//
// It goes in batches of rows, doubling: one lex down to upto could read
// the whole buffer for rows a confirmation a few rows in makes certain —
// an edit at the top while the usages list asks for the last row. A token
// running past a batch is read again from its start by the next one, as
// often as there are batches, which the doubling keeps to a few.
func (h *hlCache) lex(e *editor, upto int) {
	b := e.buffer()
	for batch := 64; h.done <= upto; batch *= 2 {
		r := h.done
		in := h.ln[r].in
		// where the lexer resumes: this row's start, or the start of the
		// token it begins inside of
		from, fromRow := b.starts[r], r
		if in.inTok {
			fromRow = r - in.back
			from = b.starts[fromRow] + in.col
		}
		last := min(upto, r+batch)
		toks := h.d.LexRange(b.text, from, b.rowStart(last+1))
		h.spread(b, toks, fromRow, r, last)
	}
}

// spread hands toks — lexed from row fromRow, whose tokens before row r
// are known already — out to rows r, r+1, … as far as upto, or further
// when the rows past upto can be confirmed, setting done past the last
// row it settles.
func (h *hlCache) spread(b *editorBuf, toks []sqlsplit.Token, fromRow, r, upto int) {
	rows := h.rows
	ti := 0
	// skip the tokens that end before row r begins (the earlier part of
	// the token r begins inside of runs into r, and stays)
	for ti < len(toks) && toks[ti].End <= b.starts[r] {
		ti++
	}
	for k := r; k <= upto; k++ {
		start := b.starts[k]
		end := start + runesLen(rows[k]) // the row's bytes, its newline not included
		row := &h.ln[k]
		// the row's old tokens are overwritten in place: each row's slice
		// is its own (shift moves rows, never copies one to two places),
		// and a frame reads them only while it draws
		row.toks = row.toks[:0]
		cols := colCursor{row: rows[k], ascii: end-start == len(rows[k])}
		for j := ti; j < len(toks) && toks[j].Start < end; j++ {
			t := toks[j]
			if t.End <= start {
				continue
			}
			a, z := max(t.Start, start)-start, min(t.End, end)-start
			row.toks = append(row.toks, hlTok{from: int32(cols.at(a)), to: int32(cols.at(z)), kind: t.Kind})
		}
		row.lexed = true
		// where the next row begins: inside the token running over this
		// row's newline, if one does.
		//
		// A token cut off by the buffer's end is still open there: a
		// string left open runs to the end, and when the last row is
		// empty its end is that row's start. No finished token ends on a
		// newline (a line comment's stops before it), so one ending at
		// len(text) on this row's newline is unfinished, and text added
		// on the next row goes on inside it. Called between tokens, that
		// row was lexed as code once text was added to it.
		next := hlIn{}
		nextStart := end + 1
		open := func(t sqlsplit.Token) bool { return t.End > nextStart || t.End == len(b.text) }
		for ti < len(toks) && toks[ti].End <= nextStart && !open(toks[ti]) {
			ti++
		}
		if ti < len(toks) && toks[ti].Start < nextStart && open(toks[ti]) {
			t := toks[ti]
			tr := rowOf(b, t.Start, fromRow, k)
			next = hlIn{inTok: true, back: k + 1 - tr, col: t.Start - b.starts[tr]}
		}
		h.done = k + 1
		if k+1 >= len(h.ln) {
			return
		}
		nx := &h.ln[k+1]
		if nx.lexed && nx.in == next && !next.inTok {
			// a tentative row reached as it was lexed: it and the run of
			// tentative rows after it stand
			for h.done < len(h.ln) && h.ln[h.done].lexed {
				h.done++
			}
			return
		}
		nx.in, nx.lexed, nx.toks = next, false, nil
	}
}

// rowOf is the row byte off falls in, among rows lo…hi: a binary search,
// since a token can run over thousands of rows and is asked about on each.
func rowOf(b *editorBuf, off, lo, hi int) int {
	return lo + sort.Search(hi-lo+1, func(i int) bool { return b.starts[lo+i] > off }) - 1
}

// colCursor turns byte offsets into a row, asked in increasing order, into
// rune columns — at once on an ASCII row, else by walking it once.
type colCursor struct {
	row        []rune
	ascii      bool
	col, bytes int
}

func (c *colCursor) at(off int) int {
	if c.ascii {
		return off
	}
	if off < c.bytes {
		// asked out of order, which spread's in-order tokens never do:
		// start again rather than answer wrong
		c.col, c.bytes = 0, 0
	}
	for c.col < len(c.row) && c.bytes < off {
		c.bytes += runeBytes(c.row[c.col])
		c.col++
	}
	return c.col
}

// rowStyles is row's style per rune, lexing down to it if need be: the
// usages list draws the editor's lines in their colors.
func (e *editor) rowStyles(st styles, row int) []Style {
	if row < 0 || row >= len(e.lines) {
		return nil
	}
	h := e.highlight(row)
	out := make([]Style, len(e.lines[row]))
	for i := range out {
		out[i] = st.base
	}
	for _, t := range h.ln[row].toks {
		for i := int(t.from); i < int(t.to) && i < len(out); i++ {
			out[i] = tokenStyle(st, t.kind)
		}
	}
	return out
}
