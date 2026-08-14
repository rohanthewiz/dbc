package ui

import (
	"slices"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/rohanthewiz/dbc/model"
)

// armMeta puts the process in a terminal the layer trusts.
func armMeta(t *testing.T) {
	t.Helper()
	t.Setenv("TERM", "xterm-kitty")
	t.Setenv("TERM_PROGRAM", "")
	t.Setenv("KITTY_WINDOW_ID", "")
}

// disarmMeta puts it in one it does not.
func disarmMeta(t *testing.T) {
	t.Helper()
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("TERM_PROGRAM", "Apple_Terminal")
	t.Setenv("KITTY_WINDOW_ID", "")
	t.Setenv("GHOSTTY_RESOURCES_DIR", "")
	t.Setenv("WEZTERM_PANE", "")
}

func pressMeta(a *App, r rune) {
	a.app.QueueEvent(tcell.NewEventKey(tcell.KeyRune, r, tcell.ModMeta))
}

// demoResult stands in for a query having been run, which the export modal
// requires before it will open.
func demoResult() *model.Result {
	return &model.Result{
		Conn:    "demo",
		Columns: []string{"id", "name"},
		Rows:    [][]string{{"1", "mochi"}},
		Raw:     [][]any{{int64(1), "mochi"}},
	}
}

// THE house rule: no verb may be reachable only by ⌘. Each row names the Ctrl
// chord that does the same thing, and pressing that chord must really open
// the same thing — a table entry alone would only be a promise.
func TestNothingIsCommandOnly(t *testing.T) {
	for _, m := range metaAccels() {
		a, _ := newTier1App(t, agentPanes())
		setBuffer(t, a, "SELECT 1", 0)
		// Each row's verb refuses for its own good reason when there is
		// nothing to act on — no result to export, no history to recall —
		// and such a refusal would read here as a missing binding. Give
		// every row something to work with.
		onUI(t, a, func() bool {
			a.lastRes = demoResult()
			a.record("SELECT 1")
			return true
		})

		press(a, m.twin)
		drain(t, a)
		viaCtrl := onUI(t, a, func() string { return frontPage(a) })
		if viaCtrl == "main" {
			t.Errorf("%s: the Ctrl twin opened nothing, so the ⌘ chord is the only way in", m.label)
			continue
		}
		press(a, tcell.KeyEsc)
		drain(t, a)

		armMeta(t)
		pressMeta(a, m.key)
		drain(t, a)
		if viaMeta := onUI(t, a, func() string { return frontPage(a) }); viaMeta != viaCtrl {
			t.Errorf("%s: ⌘%c opened %q but ^%c opened %q",
				m.label, m.key, viaMeta, m.key-32, viaCtrl)
		}
	}
}

// Binding one of these would not duplicate a host feature, it would take it
// away from the user inside this pane.
func TestMetaAccelsAvoidReservedChords(t *testing.T) {
	reserved := metaReserved()
	for _, m := range metaAccels() {
		if slices.Contains(reserved, m.key) {
			t.Errorf("⌘%c is the host's chord and must not be bound", m.key)
		}
	}
}

// The chords must be ones cats will actually forward, or they are inert and
// silently so.
func TestMetaAccelsAreOnTheHostAllowlist(t *testing.T) {
	// cats' CMD_TO_PANE, matched on e.code (cmd/catway/web/index.html).
	forwarded := []rune{'s', 'p', 'e', 'f', 'd', 'g', '/'}
	for _, m := range metaAccels() {
		if !slices.Contains(forwarded, m.key) {
			t.Errorf("⌘%c (%s) is not on cats' CMD_TO_PANE list, so it would never arrive",
				m.key, m.label)
		}
	}
}

// The kitty protocol reports the unshifted codepoint with a shift bit; other
// emitters send the capital. The table names each chord once, so both have to
// fold to the same pair.
func TestMetaChordFoldsBothSpellings(t *testing.T) {
	kitty := tcell.NewEventKey(tcell.KeyRune, 'p', tcell.ModMeta|tcell.ModShift)
	capital := tcell.NewEventKey(tcell.KeyRune, 'P', tcell.ModMeta)

	r1, s1 := metaChord(kitty)
	r2, s2 := metaChord(capital)
	if r1 != 'p' || !s1 {
		t.Errorf("kitty spelling folded to %c/%v", r1, s1)
	}
	if r1 != r2 || s1 != s2 {
		t.Errorf("the two spellings disagree: %c/%v vs %c/%v", r1, s1, r2, s2)
	}

	if r, s := metaChord(tcell.NewEventKey(tcell.KeyRune, 'e', tcell.ModMeta)); r != 'e' || s {
		t.Errorf("an unshifted chord folded to %c/%v", r, s)
	}
}

// A terminal that folds Option into Meta would otherwise turn ⌥e into an
// export dialog.
func TestMetaIsInertOnAnUntrustedTerminal(t *testing.T) {
	disarmMeta(t)
	a := newTestApp(t)
	setBuffer(t, a, "SELECT 1", 0)
	onUI(t, a, func() bool { a.lastRes = demoResult(); return true })

	if onUI(t, a, func() bool { return a.metaAccelArmed() }) {
		t.Fatal("the layer should not arm on a plain terminal")
	}
	pressMeta(a, 'e')
	drain(t, a)
	if got := onUI(t, a, func() string { return frontPage(a) }); got != "main" {
		t.Errorf("⌥e opened %q on a terminal where Meta may mean Option", got)
	}
}

// Inside cats the terminal is xterm-256color, so Tier 1 is what arms it.
func TestTier1ArmsTheLayer(t *testing.T) {
	disarmMeta(t)
	a, _ := newTier1App(t, agentPanes())

	if !onUI(t, a, func() bool { return a.metaAccelArmed() }) {
		t.Error("Tier 1 should arm the layer even on a terminal that does not self-identify")
	}
}

// "⌘S did nothing" is a disappointment; "⌘S typed an s into the editor" is a
// bug. Every armed ⌘ chord is consumed, claimed or not.
func TestUnclaimedCommandChordsAreSwallowed(t *testing.T) {
	armMeta(t)
	a := newTestApp(t)
	setBuffer(t, a, "", 0)

	for _, r := range []rune{'s', 'w', 'c', 'v', 'k'} {
		pressMeta(a, r)
	}
	drain(t, a)

	if got := onUI(t, a, func() string { return a.editor.GetText() }); got != "" {
		t.Errorf("a ⌘ chord typed into the editor: %q", got)
	}
}

// Over a modal a chord must still be swallowed rather than reach the field.
func TestCommandChordsDoNotTypeIntoAModal(t *testing.T) {
	armMeta(t)
	a := newTestApp(t)
	onUI(t, a, func() bool { a.lastRes = demoResult(); return true })

	press(a, tcell.KeyCtrlE)
	drain(t, a)
	if onUI(t, a, func() string { return frontPage(a) }) != "export" {
		t.Fatal("the export modal did not open")
	}

	pressMeta(a, 's') // unclaimed, over a modal
	drain(t, a)
	if got := onUI(t, a, func() string { return frontPage(a) }); got != "export" {
		t.Errorf("a swallowed chord disturbed the modal, front page is %q", got)
	}
}
