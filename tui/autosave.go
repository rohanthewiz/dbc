package tui

import (
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/userdata"
)

// Autosave (N-254). The editors' text used to be written only on a clean
// quit, a console swap, or a tab opened or closed. A closed terminal
// window, an ssh drop, a kill — anything but ^Q — lost every line typed
// since launch, with the tabs and the layout. Now:
//
//   - a pause in typing saves: the first edit arms a timer; when it fires
//     after autosaveIdle with no edit since, every tab whose editor changed
//     is saved, and the layout with them. Steady typing is saved at least
//     every autosaveMax, so a long stretch of it is not all at risk.
//   - SIGHUP (the terminal going away) saves everything, then quits through
//     the usual path (hangup). SIGINT and SIGTERM already quit cleanly
//     through Bubble Tea, and Run saves on the way out.
//   - quitting saves before it stops anything (Run), so the up-to-30s wait
//     for a job to roll back cannot strand the buffers behind a second ^C.
//
//	edit ──► arm (once) ──autosaveIdle──► due?
//	                                        ├─ typed within autosaveIdle, and the first
//	                                        │  unsaved edit is under autosaveMax old
//	                                        │     └─► re-arm for the rest of the pause
//	                                        └─ else: saveEdited + saveLayout
//
// Only with persistence on (consoleDir set): under Options.NoPersist, as in
// the tests, nothing is saved and no timer runs.

// autosaveIdle and autosaveMax are vars so a test can shorten them.
var (
	autosaveIdle = 2 * time.Second
	autosaveMax  = 20 * time.Second
)

// autosaveMsg is the autosave timer firing.
type autosaveMsg struct{}

// hangupMsg is SIGHUP, sent by Run's signal handler.
type hangupMsg struct{}

// autosaveState is the Model's autosave bookkeeping.
type autosaveState struct {
	ed     *editor   // the editor last looked at, and the edit version it was at
	ver    int       //
	armed  bool      // a timer is pending
	first  time.Time // the first edit not yet saved (zero: none)
	last   time.Time // the latest edit
	warned bool      // a failed save was logged; said once, not every pause
}

// noteEdits runs after every message (Update): it notices an edit of the
// editor on screen and arms the timer. A different editor on screen is a
// tab switch, not an edit: it is only noted, and a tab left with unsaved
// edits is still saved when the timer fires (saveEdited walks every tab).
func (m *Model) noteEdits() tea.Cmd {
	if m.consoleDir == "" || m.editor == nil {
		return nil
	}
	a := &m.autosave
	if m.editor != a.ed {
		a.ed, a.ver = m.editor, m.editor.version
		return nil
	}
	if m.editor.version == a.ver {
		return nil
	}
	a.ver = m.editor.version
	a.last = time.Now()
	if a.first.IsZero() {
		a.first = a.last
	}
	if a.armed {
		return nil
	}
	a.armed = true
	return autosaveAfter(autosaveIdle)
}

func autosaveAfter(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return autosaveMsg{} })
}

// autosaveDue is the timer firing: it saves after a pause, or waits out the
// rest of one while the typing goes on (but not past autosaveMax).
func (m *Model) autosaveDue() tea.Cmd {
	a := &m.autosave
	a.armed = false
	if a.first.IsZero() {
		return nil
	}
	if idle := time.Since(a.last); idle < autosaveIdle && time.Since(a.first) < autosaveMax {
		a.armed = true
		return autosaveAfter(autosaveIdle - idle)
	}
	a.first = time.Time{}
	m.saveEdited()
	m.saveLayout()
	return nil
}

// saveEdited saves the editor of every tab whose text changed since it was
// last saved. The tab on screen goes through saveConsole; a parked tab is
// saved from its own fields rather than loaded and parked again (which
// forEachTab does), since a load closes the completion popup of the tab on
// screen — a save on a typing pause must not. A parked tab with no console
// of its own is left for its return: the editor buffer file it would use
// belongs to the tab on screen.
func (m *Model) saveEdited() {
	for i, t := range m.tabs {
		var err error
		if i == m.curTab {
			if m.editor.version == m.editor.savedVer {
				continue
			}
			if err = m.saveConsole(); err == nil {
				m.editor.savedVer = m.editor.version
			}
		} else {
			if t.editor == nil || t.console == "" || t.editor.version == t.editor.savedVer {
				continue
			}
			text := t.editor.Text()
			if text != t.consoleText {
				err = userdata.SaveConsole(t.console, text)
			}
			if err == nil {
				t.consoleText, t.editor.savedVer = text, t.editor.version
			}
		}
		if err != nil && !m.autosave.warned {
			// once: a full disk would otherwise say so at every pause
			m.autosave.warned = true
			m.logf(logWarn, "could not save the editor's SQL (it is kept here; ^Q tries again): %s", serr.StringFromErr(err))
		}
	}
}

// hangup is SIGHUP: the terminal is gone. Every tab's text and the layout
// are saved first, here on the model's goroutine — the quit that follows
// stops runs and may wait on a rollback, and nothing must wait behind it —
// then it quits as ^Q would.
func (m *Model) hangup() tea.Cmd {
	m.saveAll()
	return m.quitCmd()
}

// saveAll saves the layout and every tab's editor, whether or not it was
// edited: Run's last act on the way out, and SIGHUP's first.
func (m *Model) saveAll() []error {
	m.saveLayout()
	var errs []error
	m.forEachTab(func() {
		if err := m.saveConsole(); err != nil {
			errs = append(errs, err)
		}
	})
	return errs
}
