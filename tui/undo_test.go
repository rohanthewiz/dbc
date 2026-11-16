package tui

import (
	"strings"
	"testing"
)

// Undo after N-279: a step holds the rows it replaced, not a copy of the
// buffer, and the stack is capped by bytes as well as by steps.

// Words typed into a 3.5 MB migration, each its own step, hold the line
// typed on: a few hundred bytes a step. Before, each step was a copy of
// the buffer — 14 MB as runes — and the 200 the stack kept came to 2.8 GB.
func TestUndoStepsHoldTheRowsTheyChanged(t *testing.T) {
	e := newEditor(false)
	e.SetText(bigSQL(3_500_000))
	mid := len(e.lines) / 2
	e.cur = pos{mid, 0}
	before := string(e.lines[mid])
	for range 300 {
		e.Insert("w ") // a paste of two runes: a step each
	}
	e.closeStep()
	if len(e.undo) != undoCap {
		t.Fatalf("%d steps kept, want the cap of %d", len(e.undo), undoCap)
	}
	if e.undoBytes > 1<<20 {
		t.Errorf("200 one-line steps hold %d bytes, want well under 1 MB", e.undoBytes)
	}
	for e.Undo() {
	}
	// the 100 oldest steps were dropped by the cap; the rest undo to
	// the line as it stood after them
	if got, want := string(e.lines[mid]), strings.Repeat("w ", 100)+before; got != want {
		t.Errorf("undone to %q…, want %q…", got[:40], want[:40])
	}
	if e.Text() != refText(e) {
		t.Error("the text cache disagrees with the rows after undoing")
	}
}

// Replacing the whole of a 1M-rune buffer, again and again, keeps only as
// many steps as fit undoBytesCap (each holds the 4 MB the replacement
// took out), and undoing them all lands on the text before the oldest one
// kept.
func TestUndoByteCapDropsTheOldest(t *testing.T) {
	const n = 1 << 20
	letters := "abcdefghijkl"
	e := newEditor(false)
	e.SetText(strings.Repeat("x", n))
	for _, l := range letters {
		e.SelectAll()
		e.Insert(strings.Repeat(string(l), n))
	}
	e.closeStep()
	k := len(e.undo)
	if e.undoBytes > undoBytesCap {
		t.Errorf("the stack holds %d bytes, past the cap of %d", e.undoBytes, undoBytesCap)
	}
	if k < 2 || k >= len(letters) {
		t.Fatalf("%d of %d steps kept, want the cap to have dropped some and kept several", k, len(letters))
	}
	for e.Undo() {
	}
	if want := strings.Repeat(string(letters[len(letters)-k-1]), n); e.Text() != want {
		t.Errorf("undoing all %d kept steps gave %q…, want %q…", k, e.Text()[:8], want[:8])
	}
}

// One step bigger than the cap is still kept, alone: the last edit can
// always be undone. The next step then pushes it out.
func TestUndoKeepsANewestStepPastTheCap(t *testing.T) {
	big := strings.Repeat("z", undoBytesCap/4+1024) // past the cap as runes
	e := newEditor(false)
	e.SetText(big)
	e.SelectAll()
	e.Insert("y")
	if !e.Undo() || e.Text() != big {
		t.Fatalf("the step past the cap was not undone (text %d bytes)", len(e.Text()))
	}
	e.Redo()
	e.Insert("1")
	e.lastAt = e.lastAt.AddDate(0, 0, -1)
	e.Insert("2") // closes the "1" step, past the big one
	e.closeStep()
	if len(e.undo) != 2 || e.undoBytes > undoBytesCap {
		t.Errorf("%d steps, %d bytes: want the big step dropped for the two after it", len(e.undo), e.undoBytes)
	}
}

// Typing straight after SetText is a step of its own. It used to merge
// into the step before — which SetText had cleared — when it came within
// the coalescing window of earlier typing, and could not be undone.
func TestTypingAfterSetTextCanBeUndone(t *testing.T) {
	e := newEditor(false)
	e.Insert("a")
	e.SetText("SELECT 1")
	e.cur = pos{0, 8}
	e.Insert("0")
	if !e.Undo() || e.Text() != "SELECT 1" {
		t.Errorf("undo after typing on a loaded text gave %q", e.Text())
	}
}

// A View closes the open step, so a parked console's history holds rows,
// not the snapshot of a whole buffer; Restore puts its size back.
func TestViewClosesTheOpenStep(t *testing.T) {
	e := newEditor(false)
	e.SetText(bigSQL(200_000))
	e.Insert("x")
	v := e.View()
	if len(v.undo) != 1 || v.undo[0].base != nil || len(v.undo[0].old) != 1 {
		t.Fatalf("parked step = %+v rows, base %v: want one row, closed", len(v.undo[0].old), v.undo[0].base != nil)
	}
	e.SetText("other")
	e.Restore(v, v.text)
	if e.undoBytes != v.undo[0].size {
		t.Errorf("restored stack counts %d bytes, its step %d", e.undoBytes, v.undo[0].size)
	}
	if !e.Undo() || e.Text() != bigSQL(200_000) {
		t.Error("the parked step did not undo after Restore")
	}
}
