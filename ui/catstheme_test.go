package ui

import (
	"time"

	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/rohanthewiz/dbc/cats"
	"github.com/rohanthewiz/dbc/theme"
)

// hostColors is a plausible cats palette: the seven core keys, the two
// surfaces, and one translucent key of the kind cats really does emit.
func hostColors() map[string]string {
	return map[string]string{
		"bg": "#101418", "fg": "#e6e6e6", "muted": "#8a94a0", "line": "#2a3138",
		"accent": "#5aa9e6", "warn": "#e0b050", "err": "#e06c75",
		"panel": "#161b21", "panel2": "#1d242b",
		"sel-fill": "rgba(90,169,230,0.30)", // dropped: not a hex color
		"ok":       "#7bd88f",               // ignored: dbc's success tone is its accent
	}
}

// restorePalette puts the built-in palette back, because setPalette writes
// package-level state that would otherwise leak into the next test.
func restorePalette(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { setPalette(theme.Default()) })
}

func TestCatsHostPaletteMapsTheHostColors(t *testing.T) {
	p, ok := catsHostPalette(hostColors())
	if !ok {
		t.Fatal("a complete host theme should produce a palette")
	}
	cases := []struct{ name, got, want string }{
		{"Bg", p.Bg, "#101418"}, {"Fg", p.Fg, "#e6e6e6"},
		{"Muted", p.Muted, "#8a94a0"}, {"Line", p.Line, "#2a3138"},
		{"Accent", p.Accent, "#5aa9e6"}, {"Warn", p.Warn, "#e0b050"},
		{"Err", p.Err, "#e06c75"},
		{"Panel", p.Panel, "#161b21"}, {"Panel2", p.Panel2, "#1d242b"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	// The selection is computed, not taken: the host's own is translucent.
	if want := theme.Blend("#5aa9e6", "#101418", catsSelAlpha); p.Sel != want {
		t.Errorf("Sel = %q, want the accent blended over bg at %.2f (%q)",
			p.Sel, catsSelAlpha, want)
	}
}

// A half-translated theme looks like a rendering bug, and the built-in
// palette is a perfectly good answer — so one bad core key abandons the lot.
func TestCatsHostPaletteRefusesAnIncompleteTheme(t *testing.T) {
	for _, missing := range []string{"bg", "fg", "muted", "line", "accent", "warn", "err"} {
		colors := hostColors()
		delete(colors, missing)
		if _, ok := catsHostPalette(colors); ok {
			t.Errorf("a theme missing %q should be refused entirely", missing)
		}

		colors = hostColors()
		colors[missing] = "rgba(1,2,3,0.5)"
		if _, ok := catsHostPalette(colors); ok {
			t.Errorf("a non-hex %q should be refused entirely", missing)
		}
	}
	if _, ok := catsHostPalette(nil); ok {
		t.Error("no colors at all should be refused")
	}
}

// The two surfaces are optional: without them the palette is flatter, but the
// depth ordering still holds.
func TestCatsHostPaletteFallsBackInwardForSurfaces(t *testing.T) {
	colors := hostColors()
	delete(colors, "panel")
	delete(colors, "panel2")

	p, ok := catsHostPalette(colors)
	if !ok {
		t.Fatal("missing surfaces should still produce a palette")
	}
	if p.Panel != p.Bg {
		t.Errorf("Panel should fall back to Bg, got %q", p.Panel)
	}
	if p.Panel2 != p.Panel {
		t.Errorf("Panel2 should fall back to Panel, got %q", p.Panel2)
	}
}

// setPalette drives the tags as well as the colors, including the key hints
// that are written in the accent.
func TestSetPaletteRebuildsTheTags(t *testing.T) {
	restorePalette(t)

	p, _ := catsHostPalette(hostColors())
	setPalette(p)

	if tagAccent != "[#5aa9e6]" {
		t.Errorf("tagAccent = %q, want the host accent", tagAccent)
	}
	if tagOk != tagAccent {
		t.Error("dbc's success tone is its accent by design")
	}
	if colBg != tcell.GetColor("#101418") {
		t.Errorf("colBg did not follow the palette")
	}
	if want := "[#5aa9e6]"; keyHints[:len(want)] != want {
		t.Errorf("keyHints were not rebuilt in the new accent: %q", keyHints[:20])
	}
}

// onUIDraw runs f on the UI goroutine and waits until the screen has been
// repainted. onUI deliberately does not redraw — it uses QueueUpdate — so a
// test that changes what the screen should LOOK like has to ask for the
// repaint that proves it.
//
// The trailing drain is the part that is easy to miss: QueueUpdateDraw runs
// the draw AFTER the closure returns, so a test that read the cells as soon
// as its closure finished would be looking at the frame before the change.
func onUIDraw(t *testing.T, a *App, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { a.app.QueueUpdateDraw(func() { f(); close(done) }) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the UI goroutine")
	}
	drain(t, a) // queued behind the draw, so it returns only once that is done
}

// backgrounds returns every background color currently on the screen.
func backgrounds(screen tcell.SimulationScreen) map[tcell.Color]bool {
	cells, w, h := screen.GetContents()
	seen := map[tcell.Color]bool{}
	for y := range h {
		for x := range w {
			_, bg, _ := cells[y*w+x].Style.Decompose()
			seen[bg] = true
		}
	}
	return seen
}

// The visible half: a theme arriving over the stream repaints widgets that
// were built with the old palette. This is what the whole const→var refactor
// exists for, so it is checked on the cells rather than on the variables.
func TestCatsThemeArrivedRepaints(t *testing.T) {
	restorePalette(t)
	a, screen := newTestAppScreen(t)

	if !backgrounds(screen)[tcell.GetColor(theme.Bg)] {
		t.Fatal("the built-in background was not on screen to begin with")
	}

	onUIDraw(t, a, func() {
		a.catsThemeArrived(cats.ThemeChangedEvent{Name: "host", Colors: hostColors()})
	})

	seen := backgrounds(screen)
	if !seen[tcell.GetColor("#101418")] {
		t.Error("no cell picked up the host background — restyle did not reach the widgets")
	}
	if seen[tcell.GetColor(theme.Bg)] {
		t.Error("the old background is still on screen — the repaint was partial")
	}
}

// cats broadcasts its theme after any config change, including ones that
// touched no color, and a repaint that changes nothing still costs a redraw.
func TestCatsThemeArrivedIgnoresAnIdenticalPalette(t *testing.T) {
	restorePalette(t)
	a := newTestApp(t)

	logged := func() string { return onUI(t, a, func() string { return a.logView.GetText(true) }) }

	onUI(t, a, func() bool {
		a.catsThemeArrived(cats.ThemeChangedEvent{Name: "host", Colors: hostColors()})
		return true
	})
	first := logged()

	onUI(t, a, func() bool {
		a.catsThemeArrived(cats.ThemeChangedEvent{Name: "host", Colors: hostColors()})
		return true
	})
	if second := logged(); second != first {
		t.Error("an unchanged palette was re-applied; the same-palette check did not hold")
	}
}

// A theme dbc cannot use must leave the current one alone rather than
// half-apply.
func TestCatsThemeArrivedIgnoresAnUnusableTheme(t *testing.T) {
	restorePalette(t)
	a := newTestApp(t)

	before := palette
	onUI(t, a, func() bool {
		a.catsThemeArrived(cats.ThemeChangedEvent{Colors: map[string]string{"bg": "nope"}})
		return true
	})
	if palette != before {
		t.Error("an unusable theme changed the palette")
	}
}

// Outside cats the startup fetch must do nothing at all — no dial, no delay.
func TestCatsThemeAtStartupIsInertOutsideCats(t *testing.T) {
	restorePalette(t)
	t.Setenv(cats.EnvMarker, "")
	t.Setenv(cats.EnvPaneID, "")
	t.Setenv(cats.EnvControlSocket, "")

	before := palette
	catsThemeAtStartup()
	if palette != before {
		t.Error("the startup fetch changed the palette outside cats")
	}
}
