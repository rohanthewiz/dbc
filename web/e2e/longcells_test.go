package e2e

import (
	"strings"
	"testing"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"
)

// longValuesSQL has two long multi-line documents ("é" and a newline
// repeated: 100,000 and 300,000 characters) around a NULL — web/grid_test.go's
// longSQL, the N-277 scenario of a column of large values.
const longValuesSQL = `SELECT id,
  CASE id WHEN 2 THEN NULL ELSE replace(hex(zeroblob(50000 * id)), '00', 'é' || char(10)) END AS doc,
  'short' AS s
FROM (SELECT 1 AS id UNION ALL SELECT 2 UNION ALL SELECT 3)`

// longValues: a column of large documents reaches the page as heads — the
// server sends a value's first 500 characters and lists the cells it cut
// (resultPage.Cut) — so a page is a few KB, not the ~450 KB of values,
// and the grid flattens and escapes only those heads per scroll frame.
// A cut cell still ends in "…". Inspect asks for the value under the
// cursor whole (GET …/cell), under the sort the grid shows, and its copy
// button copies all of it; a NULL needs no request (N-277).
func longValues(t *testing.T, _ *env, p *rod.Page) {
	before := gridSeq(t, p)
	eval(t, p, `(s) => dbc.editor.setText(s)`, longValuesSQL)
	p.MustElement("#run").MustClick()
	waitResult(t, p, before, "id", "doc", "s")

	// what crossed the wire for the first page: the resource timing of the
	// latest /result fetch (same origin, so its body size is readable)
	size := evalNum(t, p, `() => {
	  const r = performance.getEntriesByType("resource").filter((e) => e.name.includes("/result?"));
	  return r.length ? r[r.length - 1].decodedBodySize : -1;
	}`)
	if size < 0 || size > 8<<10 {
		t.Fatalf("the first page was %v bytes; the documents should arrive clipped to a few KB", size)
	}

	// the first row's doc: a flattened head of 500 characters, then "…"
	head := evalStr(t, p, `() => document.querySelectorAll("#grid .gb .gr")[0].querySelectorAll(".gc")[1].textContent`)
	if n := len([]rune(head)); n != 501 || !strings.HasSuffix(head, "…") || !strings.HasPrefix(head, "é↵é↵") {
		t.Fatalf("doc cell = %d characters %.20q…, want 500 flattened ones and a closing …", n, head)
	}

	// Inspect on it: the whole value, fetched
	p.MustElement("#grid").MustFocus()
	p.Keyboard.MustType(input.ArrowRight)
	waitFor(t, p, "the cursor on row 1's doc", `() => { const c = dbc.grid.view().cur; return c.row === 0 && c.col === 1; }`)
	reqs(t, p)
	p.Keyboard.MustType(input.Enter)
	waitFor(t, p, "Inspect showing all 100,000 characters",
		`() => { const m = document.querySelector(".modal .inspect pre"); return !!m && m.textContent.length === 100000; }`)
	if !hasReq(reqs(t, p), "GET", "/cell") {
		t.Error("Inspect on a cut value made no …/cell request")
	}
	p.Keyboard.MustType(input.Escape)
	waitFor(t, p, "Inspect closed", `() => !document.querySelector(".modal .inspect")`)

	// sorted by id, descending: display row 0 is id 3's 300,000 characters,
	// so the request must carry the sort along with the display row
	for _, want := range []string{"asc", "desc"} {
		clickAt(t, p, `#grid .gh .hc[data-c="0"]`, proto.InputMouseButtonLeft)
		waitFor(t, p, "sorted by id "+want, `(desc) => { const v = dbc.grid.view(); return v.sort === 0 && v.desc === desc; }`, want == "desc")
	}
	p.MustElement("#grid").MustFocus()
	waitFor(t, p, "the cursor on row 1's doc", `() => { const c = dbc.grid.view().cur; return c.row === 0 && c.col === 1; }`)
	p.Keyboard.MustType(input.Enter)
	waitFor(t, p, "Inspect showing id 3's 300,000 characters",
		`() => { const m = document.querySelector(".modal .inspect pre"); return !!m && m.textContent.length === 300000; }`)
	// its copy button takes the whole value, not the head the page had
	setClipboard(t, p, "sentinel")
	p.Keyboard.MustType(input.KeyY)
	waitFor(t, p, "all 300,000 characters copied", `() => navigator.clipboard.readText().then((s) => s.length === 300000)`)
	p.Keyboard.MustType(input.Escape)
	waitFor(t, p, "Inspect closed", `() => !document.querySelector(".modal .inspect")`)

	// id 2's NULL, one row down: shown without asking the server
	p.Keyboard.MustType(input.ArrowDown)
	waitFor(t, p, "the cursor on the NULL", `() => dbc.grid.view().cur.row === 1`)
	reqs(t, p)
	p.Keyboard.MustType(input.Enter)
	waitFor(t, p, "Inspect showing NULL", `() => { const m = document.querySelector(".modal .inspect pre.null"); return !!m && m.textContent === "NULL"; }`)
	if hasReq(reqs(t, p), "GET", "/cell") {
		t.Error("Inspect on a NULL asked the server for it")
	}
	p.Keyboard.MustType(input.Escape)
	waitFor(t, p, "Inspect closed", `() => !document.querySelector(".modal .inspect")`)
}
