package tui

import (
	"strings"
	"testing"
)

func TestSurfaceClipsToItsRect(t *testing.T) {
	c := NewCanvas(20, 3, Style{})
	s := c.Sub(Rect{5, 1, 6, 1})
	s.Put(-2, 0, "abcdefghijkl", Style{})
	if got := c.Line(1); got != "     cdefgh         " {
		t.Errorf("line = %q", got)
	}
	if c.Line(0) != strings.Repeat(" ", 20) || c.Line(2) != strings.Repeat(" ", 20) {
		t.Error("a surface drew outside its rows")
	}
}

// A wide rune that would straddle a pane's right edge becomes a space rather
// than spilling half a character into the next pane.
func TestWideRuneAtTheEdge(t *testing.T) {
	c := NewCanvas(6, 1, Style{})
	c.Sub(Rect{0, 0, 3, 1}).Put(0, 0, "a猫猫", Style{})
	if got := c.Line(0); got != "a猫    " && got != "a猫   " {
		// "a" + one wide cat (2 cells) fills the 3-cell surface exactly
		t.Errorf("line = %q", got)
	}
	c2 := NewCanvas(6, 1, Style{})
	c2.Sub(Rect{0, 0, 2, 1}).Put(0, 0, "a猫", Style{})
	if got := c2.Line(0); !strings.HasPrefix(got, "a ") {
		t.Errorf("a straddling wide rune should blank, got %q", got)
	}
}

// Overwriting half of a wide cell must not leave the other half orphaned.
func TestOverwritingHalfAWideCell(t *testing.T) {
	c := NewCanvas(4, 1, Style{})
	s := c.Sub(Rect{0, 0, 4, 1})
	s.Put(0, 0, "猫", Style{})
	s.Put(1, 0, "x", Style{})
	if got := c.Line(0); got != " x  " {
		t.Errorf("line = %q", got)
	}
}

func TestControlCharactersCannotTearTheFrame(t *testing.T) {
	c := NewCanvas(8, 1, Style{})
	c.Sub(Rect{0, 0, 8, 1}).Put(0, 0, "a\tb\nc", Style{})
	if got := c.Line(0); got != "a·b·c   " {
		t.Errorf("line = %q", got)
	}
}

// The serializer emits a style only when it changes, and resets each line.
func TestStringEmitsStyleChangesOnly(t *testing.T) {
	red := Style{Fg: RGB(255, 0, 0)}
	c := NewCanvas(4, 1, red)
	out := c.String()
	if n := strings.Count(out, "\x1b[0;38;2;255;0;0m"); n != 1 {
		t.Errorf("style emitted %d times for one run: %q", n, out)
	}
	if !strings.HasSuffix(out, "\x1b[0m") {
		t.Error("each line must end with a reset")
	}
}

func TestTruncate(t *testing.T) {
	cases := map[string]string{"hello": "hello", "hello world": "hello w…", "": ""}
	for in, want := range cases {
		if got := truncate(in, 8); got != want {
			t.Errorf("truncate(%q) = %q, want %q", in, got, want)
		}
	}
	if got := truncate("猫猫猫猫猫", 5); width(got) > 5 {
		t.Errorf("truncate must respect cell width: %q is %d cells", got, width(got))
	}
}

func TestWrap(t *testing.T) {
	got := wrap("the quick brown fox", 9)
	want := []string{"the ", "quick ", "brown fox"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("wrap = %q", got)
	}
	for _, line := range wrap(strings.Repeat("x", 25), 10) {
		if width(line) > 10 {
			t.Errorf("a long word must hard-break, got %q", line)
		}
	}
}
