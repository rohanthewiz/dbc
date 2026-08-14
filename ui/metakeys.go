package ui

import (
	"os"
	"strings"
	"unicode"

	"github.com/gdamore/tcell/v2"
)

// The ⌘ accelerator layer: a second door onto verbs dbc already has, for
// hands trained on a Mac.
//
// HOW A ⌘ CHORD REACHES A TERMINAL PROGRAM AT ALL. Only through the kitty
// keyboard protocol. tcell asks for it on startup (its enableCsiU string), a
// compliant terminal reports ⌘E as CSI 101;9u — modifier 9 being 1 + kitty's
// super bit — and tcell's CSI-u parser turns that bit into ModMeta. Inside
// cats the same thing happens with cats' emulator in the middle: its front
// end forwards a curated set of ⌘ chords only to panes that asked for the
// protocol, which every dbc pane does.
//
// IT IS A BONUS LAYER AND NOTHING MAY LIVE HERE ALONE. Every row is a second
// way to reach a verb that already has a Ctrl chord, and the table carries
// that twin so a test can hold the rule rather than a comment. On a terminal
// that cannot deliver ⌘ at all, nothing is lost.
//
// WHY THE GATE. A terminal configured to send Option as Meta produces ModMeta
// for ⌥e, which would turn a stray accent-composition keystroke into an
// export dialog. So the layer is armed only where ⌘ is known to arrive as
// super: inside cats, or in an emulator that identifies itself as one that
// speaks the protocol. iTerm2 is deliberately excluded — it speaks the
// protocol AND ships the Option-as-Meta setting, and no environment variable
// separates the two configurations. Losing an accelerator there costs
// nothing; a phantom export dialog would.
//
// THE CHORDS ARE CHOSEN FROM WHAT CATS WILL ACTUALLY FORWARD. ⌘E, ⌘P and ⌘G
// are on cats' CMD_TO_PANE allowlist; ⌘W, ⌘T, ⌘N and friends are the
// browser's and never arrive. Adding a row means checking that list first,
// because the failure is silent: an unforwarded chord is simply inert.

// metaAccel is one row of the table.
type metaAccel struct {
	key rune // the chord's letter, always lowercase — see metaChord

	// twin is the Ctrl chord that does the same thing in every terminal.
	// Its presence is the "nothing ⌘-only" rule, enforced by test.
	twin tcell.Key

	label string
	fire  func(*App)
}

// metaAccels is the table. Deliberately short: these are accelerators for the
// verbs a hand reaches for without looking, not a second copy of the keymap.
func metaAccels() []metaAccel {
	return []metaAccel{
		{'e', tcell.KeyCtrlE, "export", (*App).showExportModal},
		{'p', tcell.KeyCtrlP, "history", (*App).showHistoryModal},
		{'g', tcell.KeyCtrlG, "ask an agent", (*App).showAgentModal},
	}
}

// metaReserved are the chords dbc must never bind, because the host has
// already spent them: cats' own command palette, sidebar, paste and font
// size, plus copy/undo which every terminal forwards straight through.
//
// Binding one would not merely duplicate a host feature — it would take it
// away from the user inside this pane.
func metaReserved() []rune { return []rune{'k', 'b', 'v', 'c', 'z', '=', '+', '-', '0'} }

// metaChord folds a ⌘ keystroke into a (lowercase rune, shift) pair.
//
// Two hosts spell the same chord two ways: the kitty protocol reports the
// UNSHIFTED codepoint with the shift bit set, while an emitter reporting the
// produced character sends the capital. Folding both here lets the table name
// each chord once.
func metaChord(ev *tcell.EventKey) (r rune, shift bool) {
	r = ev.Rune()
	shift = ev.Modifiers()&tcell.ModShift != 0
	if unicode.IsUpper(r) {
		shift = true
		r = unicode.ToLower(r)
	}
	return r, shift
}

// metaAccelArmed reports whether ⌘ can be trusted to mean ⌘ here.
func (a *App) metaAccelArmed() bool {
	return a.catsTier1() || metaKittyHost()
}

// metaKittyHost identifies terminals that speak the kitty keyboard protocol
// AND do not ship an Option-as-Meta setting.
//
// The environment is the whole test. A query-and-response handshake would be
// more accurate and would also leave the keymap undecided until a terminal
// answers — or forever, if it does not.
func metaKittyHost() bool {
	term, prog := os.Getenv("TERM"), os.Getenv("TERM_PROGRAM")
	switch {
	case term == "xterm-kitty" || os.Getenv("KITTY_WINDOW_ID") != "":
		return true
	case term == "xterm-ghostty" || strings.EqualFold(prog, "ghostty") ||
		os.Getenv("GHOSTTY_RESOURCES_DIR") != "":
		return true
	case strings.EqualFold(prog, "WezTerm") || os.Getenv("WEZTERM_PANE") != "":
		return true
	}
	return false
}

// metaAccelFire handles a ⌘ chord, reporting whether the event was consumed.
//
// main says the main page has the keyboard; a chord only ACTS there, because
// a modal owns the keyboard while it is up. It is still swallowed over a
// modal, which is the point of the second return path: an unclaimed ⌘ chord
// that fell through would reach the focused widget as a plain rune and type
// its letter into whatever field is open. "⌘S did nothing" is a disappointment;
// "⌘S typed an s into the filename" is a bug.
//
// Reserved chords are swallowed too, and that is deliberate. Reaching dbc at
// all means the host declined to handle it, and there is nothing sensible
// left to do with a bare letter the user never meant to type.
func (a *App) metaAccelFire(ev *tcell.EventKey, main bool) bool {
	if ev.Key() != tcell.KeyRune || ev.Modifiers()&tcell.ModMeta == 0 {
		return false
	}
	if !a.metaAccelArmed() {
		return false // ⌥ may mean Meta here; leave the key exactly as it was
	}
	r, shift := metaChord(ev)
	if main && !shift {
		for _, m := range metaAccels() {
			if m.key == r {
				m.fire(a)
				return true
			}
		}
	}
	return true
}
