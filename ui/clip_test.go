package ui

import (
	"errors"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/rohanthewiz/dbc/clip"
)

// failLocalClipboard makes the local tool unavailable, which is what SSH and
// a bare container look like. The fallback is unreachable while pbcopy works,
// so this is the only way to exercise it.
func failLocalClipboard(t *testing.T) {
	t.Helper()
	prev := sysClipWrite
	sysClipWrite = func(string) error { return errors.New("exec: pbcopy: not found") }
	t.Cleanup(func() { sysClipWrite = prev })
}

// captureLocalClipboard records what the local tool was given.
func captureLocalClipboard(t *testing.T, got *string) {
	t.Helper()
	prev := sysClipWrite
	sysClipWrite = func(s string) error { *got = s; return nil }
	t.Cleanup(func() { sysClipWrite = prev })
}

// A working local tool has actually succeeded, so it goes first and nothing
// is sent to the terminal.
func TestClipWritePrefersTheLocalTool(t *testing.T) {
	a, screen := newTestAppScreen(t)

	var local string
	captureLocalClipboard(t, &local)

	dest, err := onUI2(t, a, func() (string, error) { return a.clipWrite("hello") })
	if err != nil {
		t.Fatalf("clipWrite: %v", err)
	}
	if local != "hello" {
		t.Errorf("the local tool got %q", local)
	}
	if dest != "clipboard" {
		t.Errorf("dest = %q", dest)
	}
	if data := screen.GetClipboardData(); len(data) != 0 {
		t.Errorf("nothing should have gone to the terminal, got %q", data)
	}
}

// The point of the whole file: a copy still reaches the user's machine when
// the local tool is missing, because the terminal is on that machine.
func TestClipWriteFallsBackToTheTerminal(t *testing.T) {
	a, screen := newTestAppScreen(t)
	failLocalClipboard(t)

	dest, err := onUI2(t, a, func() (string, error) { return a.clipWrite("over ssh") })
	if err != nil {
		t.Fatalf("the fallback should succeed, got %v", err)
	}
	if !strings.Contains(dest, "terminal") {
		t.Errorf("dest should name the terminal, got %q", dest)
	}
	if got := string(screen.GetClipboardData()); got != "over ssh" {
		t.Errorf("the terminal received %q", got)
	}
}

// OSC 52 is write-and-hope and terminals silently drop oversized sequences.
// Past the cap the honest answer is the failure the local tool already gave.
func TestClipWriteRefusesToTruncate(t *testing.T) {
	a, screen := newTestAppScreen(t)
	failLocalClipboard(t)

	big := strings.Repeat("x", clipMax+1)
	_, err := onUI2(t, a, func() (string, error) { return a.clipWrite(big) })
	if err == nil {
		t.Error("an oversized copy should report the failure, not silently truncate")
	}
	if len(screen.GetClipboardData()) != 0 {
		t.Error("an oversized copy must not be sent at all")
	}
}

// With no terminal there is nothing to fall back to, and the local error is
// what the user needs to see.
func TestClipWriteWithoutAScreenReportsTheLocalError(t *testing.T) {
	a := newTestApp(t)
	failLocalClipboard(t)

	_, err := onUI2(t, a, func() (string, error) {
		a.scr = nil
		return a.clipWrite("x")
	})
	if err == nil || !strings.Contains(err.Error(), "pbcopy") {
		t.Errorf("expected the local tool's error, got %v", err)
	}
}

// The copy keys go through the same path, so y works over SSH too.
func TestCopySelectionUsesTheFallback(t *testing.T) {
	a, screen := newTestAppScreen(t)

	setBuffer(t, a, "SELECT id, name FROM cats ORDER BY id", 0)
	press(a, tcell.KeyCtrlR)
	waitFor(t, a, "the query to finish", func() bool { return a.lastRes != nil })

	failLocalClipboard(t)
	press(a, tcell.KeyTab) // focus the results table
	drain(t, a)
	pressRune(a, 'Y') // whole row
	waitFor(t, a, "the row to reach the terminal clipboard", func() bool {
		return len(screen.GetClipboardData()) > 0
	})

	if got := string(screen.GetClipboardData()); !strings.Contains(got, "\t") {
		t.Errorf("a whole row should be tab separated, got %q", got)
	}
}

// onUI2 is onUI for a function returning two values.
func onUI2[A any, B any](t *testing.T, a *App, f func() (A, B)) (A, B) {
	t.Helper()
	type pair struct {
		a A
		b B
	}
	p := onUI(t, a, func() pair {
		x, y := f()
		return pair{x, y}
	})
	return p.a, p.b
}

// The screen handle comes from tview's before-draw hook, not from dbc handing
// tview a screen of its own. That matters beyond tidiness: pre-creating the
// screen meant calling EnableMouse on one whose Init error SetScreen silently
// swallows, which panics on a nil writer instead of returning "cannot open
// terminal". Found by launching the real binary where /dev/tty is not
// available.
func TestScreenHandleComesFromTheDrawHook(t *testing.T) {
	a, screen := newTestAppScreen(t)

	onUIDraw(t, a, func() { a.scr = nil })
	if got := onUI(t, a, func() tcell.Screen { return a.scr }); got == nil {
		t.Fatal("the before-draw hook did not capture the screen")
	} else if got != screen {
		t.Error("the captured screen is not the one being drawn on")
	}
}

// stubRichClipboard stands in for the local rich writer.
func stubRichClipboard(t *testing.T, rich bool, err error, got *clip.Content) {
	t.Helper()
	prev := sysClipWriteRich
	sysClipWriteRich = func(c clip.Content) (bool, error) { *got = c; return rich, err }
	t.Cleanup(func() { sysClipWriteRich = prev })
}

// The log line must say HOW an HTML copy landed, since the user is about to
// paste into a chat app and a table and a page of tags look very different.
func TestClipWriteContentNamesHowItLanded(t *testing.T) {
	c := clip.Content{Text: "<table/>", HTML: "<table/>"}
	cases := []struct {
		name     string
		rich     bool
		err      error
		wantDest string
		wantTerm string
	}{
		{"rich writer worked", true, nil, "as a table", ""},
		{"plain only", false, nil, "as plain text", ""},
		{"no local clipboard", false, errors.New("no pbcopy"), "via the terminal, as HTML source", "<table/>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, screen := newTestAppScreen(t)
			var got clip.Content
			stubRichClipboard(t, tc.rich, tc.err, &got)
			dest, err := onUI2(t, a, func() (string, error) { return a.clipWriteContent(c) })
			if err != nil {
				t.Fatalf("clipWriteContent: %v", err)
			}
			if !strings.Contains(dest, tc.wantDest) {
				t.Errorf("dest = %q, want it to mention %q", dest, tc.wantDest)
			}
			if got != c {
				t.Errorf("the rich writer got %+v", got)
			}
			if term := string(screen.GetClipboardData()); term != tc.wantTerm {
				t.Errorf("terminal got %q, want %q", term, tc.wantTerm)
			}
		})
	}
}

// A copy with no HTML is the plain path, unchanged.
func TestClipWriteContentPlainUsesClipWrite(t *testing.T) {
	a, _ := newTestAppScreen(t)
	var local string
	captureLocalClipboard(t, &local)
	var rich clip.Content
	stubRichClipboard(t, true, nil, &rich)
	if _, err := onUI2(t, a, func() (string, error) {
		return a.clipWriteContent(clip.Content{Text: "a,b"})
	}); err != nil {
		t.Fatal(err)
	}
	if local != "a,b" || rich != (clip.Content{}) {
		t.Errorf("plain copy went local=%q rich=%+v", local, rich)
	}
}
