package ui

import (
	"errors"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
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
