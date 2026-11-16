package tui

import (
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/rivo/uniseg"

	"github.com/rohanthewiz/dbc/model"
)

// A grid cell reads only the head of its value (N-277): cellText and
// cellTextWidth must draw and measure exactly what the full-value versions
// did — truncate(flatten(v), w) with truncate measuring v whole first —
// while reading a bounded prefix.

// truncateRef is truncate as it was before N-277: measure all of s, then
// walk graphemes to w-1 cells and add "…". The reference the one-pass
// version must agree with.
func truncateRef(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if width(s) <= w {
		return s
	}
	var b strings.Builder
	used, state := 0, -1
	for s != "" {
		var g string
		var gw int
		g, s, gw, state = uniseg.FirstGraphemeClusterInString(s, state)
		if used+gw > w-1 {
			break
		}
		b.WriteString(g)
		used += gw
	}
	b.WriteString("…")
	return b.String()
}

// cellPieces are what the random values are built from: ASCII, JSON
// punctuation, wide CJK, emoji (one code point, a flag, a three-person ZWJ
// family), a precomposed and a combining accent, and every character
// flatten rewrites (LF, CRLF, a lone CR, a tab).
var cellPieces = []string{
	"a", "Z", " ", "{", "\"", ":", "中", "猫", "😀", "🇯🇵", "👨‍👩‍👧",
	"é", "é", "\n", "\r\n", "\r", "\t",
}

// randomValue is n pieces drawn from cellPieces.
func randomValue(r *rand.Rand, n int) string {
	var b strings.Builder
	for range n {
		b.WriteString(cellPieces[r.IntN(len(cellPieces))])
	}
	return b.String()
}

// The one-pass truncate agrees with the measure-first one on every width,
// including the edges: 0, 1, a wide character straddling w, and a value
// that exactly fits.
func TestTruncateOnePassMatchesMeasureFirst(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for i := range 3000 {
		s := randomValue(r, r.IntN(40))
		for _, w := range []int{0, 1, 2, 3, r.IntN(60), width(s), width(s) - 1, width(s) + 1} {
			if got, want := truncate(s, w), truncateRef(s, w); got != want {
				t.Fatalf("case %d: truncate(%q, %d) = %q, want %q", i, s, w, got, want)
			}
		}
	}
}

// cutAt with more set puts a "…" after s even when s fits — s was only a
// head — and never draws wider than w.
func TestCutAtMoreAlwaysEllipsizes(t *testing.T) {
	cases := []struct {
		s    string
		w    int
		want string
	}{
		{"abc", 5, "abc…"},  // fits in w-1: all of it, then "…"
		{"abcd", 4, "abc…"}, // fits in w but not w-1: one less
		{"ab中", 4, "ab…"},   // the wide character cannot sit before "…"
		{"", 3, "…"},
		{"abc", 0, ""},
	}
	for _, c := range cases {
		n, dots := cutAt(c.s, c.w, true, false)
		got := c.s[:n]
		if dots {
			got += "…"
		}
		if got != c.want || width(got) > max(c.w, 0) {
			t.Errorf("cutAt(%q, %d, more) = %q, want %q", c.s, c.w, got, c.want)
		}
	}
}

// cellText on a long value — far longer than its head — is what the full
// truncate(flatten(v), w) drew, at every width up to maxUserWidth.
func TestCellTextMatchesTheWholeValue(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	for i := range 400 {
		// up to ~8,000 pieces: well past cellBytes(maxUserWidth) ≈ 6.4 KB,
		// and short ones too, which are never cut
		s := randomValue(r, r.IntN(8000))
		for _, w := range []int{1, 2, 3, maxColWidth, r.IntN(maxUserWidth) + 1, maxUserWidth} {
			if got, want := cellText(s, w), truncateRef(flatten(s), w); got != want {
				t.Fatalf("case %d (%d bytes): cellText(…, %d) = %q, want %q", i, len(s), w, got, want)
			}
		}
		for _, limit := range []int{maxColWidth, maxUserWidth} {
			if got, want := cellTextWidth(s, limit), min(width(flatten(s)), limit); got != want {
				t.Fatalf("case %d (%d bytes): cellTextWidth(…, %d) = %d, want %d", i, len(s), limit, got, want)
			}
		}
	}
}

// A value whose head runs out before the cell does — letters each stacked
// with many combining marks, far more bytes than cells — still draws as a
// prefix of the value ending in "…", within w cells.
func TestCellTextPathologicalHeadStillMarksTheCut(t *testing.T) {
	s := strings.Repeat("e"+strings.Repeat("́", 20), 2000) // 41 bytes a cell
	got := cellText(s, 30)
	if !strings.HasSuffix(got, "…") || width(got) > 30 {
		t.Fatalf("cellText = %q (%d cells), want a head ending in …", got, width(got))
	}
	if head := strings.TrimSuffix(got, "…"); !strings.HasPrefix(s, head) {
		t.Errorf("cellText %q is not a prefix of the value", head)
	}
}

// valueHead never splits a multi-byte character.
func TestValueHeadCutsOnARuneStart(t *testing.T) {
	s := "ab中cd" // 中 is bytes 2–4
	for n := 0; n <= len(s)+1; n++ {
		head, more := valueHead(s, n)
		if !strings.HasPrefix(s, head) || strings.ContainsRune(head, '�') || (more != (n < len(s))) {
			t.Errorf("valueHead(%q, %d) = %q, %v", s, n, head, more)
		}
		if n >= 2 && n < 5 && head != "ab" {
			t.Errorf("valueHead(%q, %d) = %q, want ab (中 not split)", s, n, head)
		}
	}
}

// longCells is a result whose text columns each hold ~100 KB multi-line
// JSON-ish documents: the N-277 scenario.
func longCells(rows int) *model.Result {
	doc := "{\n" + strings.Repeat("  \"key\": \"some value here\",\n", 3500) + "}" // ~100 KB, many newlines
	res := &model.Result{Columns: []string{"id", "doc", "body", "notes"}}
	for i := range rows {
		id := itoa(i + 1)
		res.Rows = append(res.Rows, []string{id, doc, doc, doc})
		res.Raw = append(res.Raw, []any{int64(i + 1), doc, doc, doc})
	}
	return res
}

// A grid of 100 KB cells sizes and draws as one of short cells would:
// auto width at the cap, a Fit at maxUserWidth, each cell one flattened
// line ending in "…".
func TestGridLongCellsDrawFlattenedAndCut(t *testing.T) {
	g := newGrid()
	g.SetResult(longCells(30), 0)
	if g.widths[1] != maxColWidth {
		t.Errorf("auto width = %d, want the cap %d", g.widths[1], maxColWidth)
	}
	c := drawGrid(g, 160, 20)
	line := c.Line(1)
	if !strings.Contains(line, "{↵  \"key\": \"some value here\",↵  \"key\": …") {
		t.Errorf("first row = %q, want the document flattened and cut at %d cells", line, maxColWidth)
	}
	g.Fit(1)
	if g.colWidth(1) != maxUserWidth {
		t.Errorf("fit width = %d, want %d", g.colWidth(1), maxUserWidth)
	}
	g.flip = true
	if line := drawGrid(g, 160, 20).Text(); !strings.Contains(line, "{↵  \"key\"") {
		t.Errorf("transposed grid lost the cell text:\n%s", line)
	}
}

// BenchmarkGridFrameLongCells is one frame of a grid showing 100 KB cells —
// what a mouse move over it costs. Before N-277 every visible cell was
// flattened (a copy) and measured whole.
func BenchmarkGridFrameLongCells(b *testing.B) {
	g := newGrid()
	g.SetResult(longCells(60), 0)
	for b.Loop() {
		drawGrid(g, 200, 50)
	}
}

// BenchmarkGridArrivalLongCells is sizing a 500-row result of 100 KB cells
// as it lands — every text column's first widthSample values measured.
func BenchmarkGridArrivalLongCells(b *testing.B) {
	res := longCells(500)
	for b.Loop() {
		newGrid().SetResult(res, 0)
	}
}
