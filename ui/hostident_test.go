package ui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

func TestHostIdentTitle(t *testing.T) {
	cases := []struct {
		conn, tag, want string
	}{
		// The run leads: a title is read from a tab bar the pane is not in,
		// where the question is "is this still going?".
		{"prod", "query", "query · prod — dbc"},
		{"prod", "", "prod — dbc"},
		{"", "script report.go", "script report.go — dbc"},
		{"", "", "dbc"},
		// Connection names come from the user's TOML and tags carry script
		// filenames: an ESC or BEL would end the escape sequence early and
		// leave the rest to be read as commands.
		{"pr\x1bod", "", "prod — dbc"},
		{"prod", "run\x07x", "runx · prod — dbc"},
		{"  prod  ", "", "prod — dbc"},
	}
	for _, c := range cases {
		if got := hostIdentTitle(c.conn, c.tag); got != c.want {
			t.Errorf("hostIdentTitle(%q, %q) = %q, want %q", c.conn, c.tag, got, c.want)
		}
	}
}

func TestOSC7CwdSeq(t *testing.T) {
	got := osc7CwdSeq("box", "/home/u/my projects/db")
	want := "\x1b]7;file://box/home/u/my%20projects/db\x07"
	if got != want {
		t.Errorf("osc7CwdSeq = %q, want %q", got, want)
	}
	// A control byte in a directory name would truncate the sequence at the
	// BEL rather than corrupt one field of it.
	if seq := osc7CwdSeq("box", "/tmp/a\x07b"); strings.Count(seq, "\x07") != 1 {
		t.Errorf("a control byte survived into the sequence: %q", seq)
	}
}

func TestFileURLPathKeepsSafeCharacters(t *testing.T) {
	if got := fileURLPath("/home/u/db-1_v2.0~x"); got != "/home/u/db-1_v2.0~x" {
		t.Errorf("unreserved characters should pass through, got %q", got)
	}
}

// The title tracks the run, so a glance at a tab bar answers whether the
// query is still going.
func TestTitleFollowsTheRun(t *testing.T) {
	a, screen := newTestAppScreen(t)
	onUI(t, a, func() bool { a.hostIdentSync(); return true })

	if got := screen.GetTitle(); got != "demo — dbc" {
		t.Fatalf("idle title = %q, want %q", got, "demo — dbc")
	}

	setBuffer(t, a, slowQuery, 0)
	press(a, tcell.KeyCtrlR)
	waitFor(t, a, "the title to name the run", func() bool {
		return screen.GetTitle() == "query · demo — dbc"
	})

	press(a, tcell.KeyCtrlK)
	waitFor(t, a, "the title to go back to idle", func() bool {
		return screen.GetTitle() == "demo — dbc"
	})
}

// The change key compares the inputs, not the built string, so the idle path
// allocates nothing on every transition.
func TestTitleIsOnlySetWhenItChanges(t *testing.T) {
	a, _ := newTestAppScreen(t)

	key := onUI(t, a, func() string {
		a.hostIdentSync()
		return a.identKey
	})
	same := onUI(t, a, func() string {
		a.hostIdentSync()
		return a.identKey
	})
	if key != same {
		t.Errorf("an unchanged state produced a new key: %q then %q", key, same)
	}
	if !strings.Contains(key, "demo") {
		t.Errorf("the key should carry the connection, got %q", key)
	}
}

// Nothing may reach /dev/tty under test, and a missing writer must simply
// mean the one-shot sequences are skipped.
func TestHostIdentInitSkipsTheTTYWhenThereIsNone(t *testing.T) {
	a, screen := newTestAppScreen(t)

	var wrote []string
	onUI(t, a, func() bool {
		a.ttyWrite = nil
		a.hostIdentInit() // must not panic, must not emit
		a.ttyWrite = func(seq string) error { wrote = append(wrote, seq); return nil }
		a.hostIdentInit()
		return true
	})

	if len(wrote) != 1 || !strings.HasPrefix(wrote[0], "\x1b]7;file://") {
		t.Errorf("expected exactly one OSC 7 emission, got %q", wrote)
	}
	// tcell saves and restores the terminal's own title; dbc must not push a
	// second copy onto that stack.
	for _, seq := range wrote {
		if strings.Contains(seq, "22;") || strings.Contains(seq, "23;") {
			t.Errorf("dbc emitted a title-stack sequence tcell already owns: %q", seq)
		}
	}
	if screen.GetTitle() == "" {
		t.Error("hostIdentInit should have set the title through tview")
	}
}
