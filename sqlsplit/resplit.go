package sqlsplit

import (
	"slices"
	"sort"
)

// Splitting is a buffer split into statements, kept so that the buffer
// after an edit can be split again around the edit instead of from the top
// (Edit). An editor splits its buffer after every keystroke — for the
// statement Ctrl+R runs, the gutter's marker, the title's "statement 2/4"
// and completion's statement — and on a 3.5 MB migration a whole Split
// costs ~18 ms a key, where Edit costs what the copy of the chunk and
// statement lists does, ~0.6 ms (N-279).
//
// A Splitting is never changed once made: Edit returns a new one, and the
// slice Stmts returns may be held on to.
type Splitting struct {
	d      Dialect
	n      int        // the length of the buffer split
	chunks []chunk    // scan's chunks, each with the terminator in force at its start
	delims delimTable // what chunk.delim indexes
	stmts  []Stmt
}

// Splitting splits sql as Split does, keeping what Edit needs.
func (d Dialect) Splitting(sql string) *Splitting {
	tab := delimTable{";"}
	chunks, _ := scanFrom(sql, d, 0, ";", &tab, nil)
	return &Splitting{d: d, n: len(sql), chunks: chunks, delims: tab, stmts: d.stmtsOf(sql, chunks, nil)}
}

// Stmts is the statements, as Split returns them. The slice is shared:
// callers must not modify it.
func (s *Splitting) Stmts() []Stmt { return s.stmts }

// Dialect is the dialect s was split as.
func (s *Splitting) Dialect() Dialect { return s.d }

// Edit returns the split of sql, the buffer s was made for with its bytes
// [at, oldEnd) replaced by sql[at:newEnd]. It equals s.Dialect().Splitting(
// sql) — FuzzSplitEdit and TestSplittingEditMatchesSplit hold it to that —
// but scans only from a little before the edit until the chunks line up
// with the old ones again:
//
//	old  │ c0 │ c1 │ c2 ┊edit┊ c3 │ c4 │ c5 │ …
//	           ▲ resume        ▲ rejoin: c4's start, shifted, past the edit, same terminator
//	new  │ c0 │ c1 │ c2'┊    ┊c3'│ c4 │ c5 │ …   c4… copied, offsets moved by the edit's delta
//
// Where to resume. A chunk's start is a clean break (scanFrom), so a scan
// may begin at any of them; Edit begins one chunk before the chunk the edit
// is in. The chunk the edit is in would not do: the cut that ended the chunk
// before it read ahead (remarkEnd) up to the first code after its
// terminator, which can be inside the edit — typing "-- x" after "SELECT
// 1; " makes those bytes the remark of statement 1. Nothing reads further
// ahead than that.
//
// Where to rejoin. At a new chunk start x, the old scan is known to go on
// as the new one would if an old chunk started at x−delta with the same
// terminator, and everything the scan from x can read is past the edit.
// It reads forward, and back by one byte at most: x−1 is the terminator's
// last byte or x is a newline, so the rules that look back (a word after
// '.', E'…' on Postgres, a $ after a name, MySQL's DELIMITER at a line's
// start) stop at x−1 or at x. Taking x past the edit's end (x > newEnd)
// keeps x−1 out of the edit too. The first rejoin that qualifies is taken.
//
//	SELECT 1;⏎SELECT 2;⏎     x right past ; (code on its line) or at ⏎
//	         ▲         ▲
//
// What is copied. Every chunk and statement before the resumed chunk is
// kept as is, every one from the rejoin on is moved by delta, and each
// statement's Text is sliced again from sql, so no old buffer is kept
// alive by a statement that outlives it. That copy is O(statements), a
// memmove's worth on a 3.5 MB buffer, against Split's O(bytes) of scanning.
func (s *Splitting) Edit(sql string, at, oldEnd, newEnd int) *Splitting {
	delta := newEnd - oldEnd
	cs := s.chunks
	if at < 0 || at > oldEnd || at > newEnd || oldEnd > s.n || newEnd > len(sql) || len(sql) != s.n+delta {
		// not an edit of the buffer s was made for: split it whole
		return s.d.Splitting(sql)
	}
	// k is the last chunk starting at or before the edit; none (an empty
	// buffer, or an edit inside a byte order mark) means the scan from the
	// top, which is where the mark is looked for
	k := sort.Search(len(cs), func(i int) bool { return cs[i].start > at }) - 1
	if k < 0 {
		return s.d.Splitting(sql)
	}
	r := max(k-1, 0)
	from := cs[r].start

	join := -1
	rejoin := func(x int, delim string) bool {
		if x <= newEnd {
			return false
		}
		old := x - delta
		j := sort.Search(len(cs), func(i int) bool { return cs[i].start >= old })
		if j < len(cs) && cs[j].start == old && s.delims[cs[j].delim] == delim {
			join = j
			return true
		}
		return false
	}
	// the old table, clipped: a terminator new to this scan is appended to
	// a copy, so s — which may be edited again another way — keeps its own
	tab := slices.Clip(s.delims)
	mid, _ := scanFrom(sql, s.d, from, s.delims[cs[r].delim], &tab, rejoin)

	tail := 0
	if join >= 0 {
		tail = len(cs) - join
	}
	chunks := make([]chunk, 0, r+len(mid)+tail)
	chunks = append(chunks, cs[:r]...)
	chunks = append(chunks, mid...)
	if join >= 0 {
		for _, c := range cs[join:] {
			c.start += delta
			c.end += delta
			c.tail += delta
			chunks = append(chunks, c)
		}
	}

	// the statements: those of the chunks before from as they were, the
	// rescanned chunks' made afresh, and those from the rejoin on moved
	old := s.stmts
	before := sort.Search(len(old), func(i int) bool { return old[i].spanStart >= from })
	after := len(old)
	if join >= 0 {
		joinAt := cs[join].start
		after = sort.Search(len(old), func(i int) bool { return old[i].spanStart >= joinAt })
	}
	stmts := make([]Stmt, 0, before+len(mid)+len(old)-after)
	stmts = append(stmts, old[:before]...)
	stmts = s.d.stmtsOf(sql, mid, stmts)
	for _, st := range old[after:] {
		st.Start += delta
		st.End += delta
		st.CodeStart += delta
		st.spanStart += delta
		st.spanEnd += delta
		stmts = append(stmts, st)
	}
	// every Text into sql: the kept ones still point into the old buffer
	for i := range stmts {
		stmts[i].Text = sql[stmts[i].Start:stmts[i].End]
	}
	// none is nil, as from Split: a caller may well ask stmts == nil
	if len(chunks) == 0 {
		chunks = nil
	}
	if len(stmts) == 0 {
		stmts = nil
	}
	return &Splitting{d: s.d, n: len(sql), chunks: chunks, delims: tab, stmts: stmts}
}
