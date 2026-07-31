package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
)

// slowQuery spins in SQLite long enough that the test can always stop it
// mid-flight. The row limit never kicks in — the recursion produces one row.
const slowQuery = `WITH RECURSIVE c(x) AS (
	SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x < 2000000000
) SELECT count(*) AS n FROM c`

// newTestApp builds the TUI on a simulation screen with its event loop
// running, so tests exercise the same paths as a real session.
func newTestApp(t *testing.T) *App {
	t.Helper()
	a, _ := newTestAppScreen(t)
	return a
}

// newTestAppScreen is newTestApp plus the screen it draws on, for tests that
// inspect what actually landed on the cells.
func newTestAppScreen(t *testing.T) (*App, tcell.SimulationScreen) {
	t.Helper()

	cfg := &config.Config{
		ScriptsDir:        "../scripts",
		MaxRows:           1000,
		DefaultConnection: "demo",
		Demo:              true,
		Connections: []config.Connection{{
			Name: "demo", Driver: "sqlite",
			// a distinct in-memory DB per test keeps them independent
			DSN: "file:" + strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()) +
				"?mode=memory&cache=shared",
		}},
	}
	mgr := db.NewManager(cfg)
	if err := db.SeedDemo(mgr, "demo"); err != nil {
		t.Fatalf("seed demo: %v", err)
	}

	a := &App{cfg: cfg, mgr: mgr, app: tview.NewApplication()}
	a.build()
	a.active = "demo"

	screen := tcell.NewSimulationScreen("UTF-8")
	screen.SetSize(120, 40)
	a.app.SetScreen(screen)
	a.app.EnableMouse(true)

	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := a.app.Run(); err != nil {
			t.Errorf("app.Run: %v", err)
		}
	}()
	t.Cleanup(func() {
		a.app.Stop()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("app did not stop")
		}
		mgr.Close()
	})

	drain(t, a) // wait for the first draw
	return a, screen
}

// onUI runs f on the UI goroutine and returns its value, which keeps tests
// off the App fields the event loop owns.
func onUI[T any](t *testing.T, a *App, f func() T) T {
	t.Helper()
	var v T
	done := make(chan struct{})
	go func() {
		a.app.QueueUpdate(func() { v = f(); close(done) })
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the UI goroutine")
	}
	return v
}

// drain waits for the event loop to work through what has been queued so far.
func drain(t *testing.T, a *App) {
	t.Helper()
	onUI(t, a, func() bool { return true })
}

// waitFor polls a condition evaluated on the UI goroutine.
func waitFor(t *testing.T, a *App, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if onUI(t, a, cond) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func press(a *App, key tcell.Key) {
	a.app.QueueEvent(tcell.NewEventKey(key, 0, tcell.ModNone))
}

// setBuffer loads the editor and puts the cursor at the given byte offset.
func setBuffer(t *testing.T, a *App, text string, cursor int) {
	t.Helper()
	onUI(t, a, func() bool {
		a.editor.SetText(text, false)
		a.editor.Select(cursor, cursor)
		return true
	})
}

func logText(t *testing.T, a *App) string {
	return onUI(t, a, func() string { return a.logView.GetText(true) })
}

func statusText(t *testing.T, a *App) string {
	return onUI(t, a, func() string { return a.status.GetText(true) })
}

const threeStatements = "SELECT 1 AS a;\nSELECT 2 AS b;\nSELECT 3 AS c"

func TestRunStatementUnderCursor(t *testing.T) {
	cases := []struct {
		name   string
		cursor int
		want   string
		col    string
		tag    string
	}{
		{"first", 3, "SELECT 1 AS a", "a", "statement 1/3"},
		{"second", 20, "SELECT 2 AS b", "b", "statement 2/3"},
		{"third", len(threeStatements), "SELECT 3 AS c", "c", "statement 3/3"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := newTestApp(t)
			setBuffer(t, a, threeStatements, c.cursor)

			press(a, tcell.KeyCtrlR)
			waitFor(t, a, "the query to finish", func() bool { return a.lastRes != nil })

			if got := onUI(t, a, func() string { return a.lastRes.Query }); got != c.want {
				t.Errorf("ran %q, want %q", got, c.want)
			}
			if got := onUI(t, a, func() string { return a.lastRes.Columns[0] }); got != c.col {
				t.Errorf("result column %q, want %q", got, c.col)
			}
			if lg := logText(t, a); !strings.Contains(lg, c.tag) {
				t.Errorf("log does not mention %q:\n%s", c.tag, lg)
			}
		})
	}
}

func TestRunSingleStatementBuffer(t *testing.T) {
	a := newTestApp(t)
	setBuffer(t, a, "SELECT name FROM cats ORDER BY id", 0)

	press(a, tcell.KeyCtrlR)
	waitFor(t, a, "the query to finish", func() bool { return a.lastRes != nil })

	if got := onUI(t, a, func() int { return len(a.lastRes.Rows) }); got != 8 {
		t.Errorf("got %d rows, want 8", got)
	}
	// a lone statement is just "query", not "statement 1/1"
	if lg := logText(t, a); !strings.Contains(lg, "running query") {
		t.Errorf("log does not mention a plain query:\n%s", lg)
	}
}

func TestRunSelection(t *testing.T) {
	a := newTestApp(t)
	// select "SELECT 2 AS b" out of the middle of the buffer
	start := strings.Index(threeStatements, "SELECT 2")
	onUI(t, a, func() bool {
		a.editor.SetText(threeStatements, false)
		a.editor.Select(start, start+len("SELECT 2 AS b"))
		return true
	})

	press(a, tcell.KeyCtrlR)
	waitFor(t, a, "the query to finish", func() bool { return a.lastRes != nil })

	if got := onUI(t, a, func() string { return a.lastRes.Query }); got != "SELECT 2 AS b" {
		t.Errorf("ran %q, want the selected statement", got)
	}
	if !strings.Contains(logText(t, a), "running selection") {
		t.Errorf("log does not mention the selection:\n%s", logText(t, a))
	}
}

func TestCtrlKCancelsQuery(t *testing.T) {
	a := newTestApp(t)
	setBuffer(t, a, slowQuery, 0)

	press(a, tcell.KeyCtrlR)
	waitFor(t, a, "the query to start", func() bool { return a.busy.Load() })

	press(a, tcell.KeyCtrlK)
	waitFor(t, a, "the query to stop", func() bool { return !a.busy.Load() })

	// a canceled query must not publish a result
	if onUI(t, a, func() bool { return a.lastRes != nil }) {
		t.Error("canceled query left a result behind")
	}
	if lg := logText(t, a); !strings.Contains(lg, "stopped") {
		t.Errorf("log does not report the stop:\n%s", lg)
	}
	if st := statusText(t, a); !strings.Contains(st, "stopped") {
		t.Errorf("status does not report the stop: %q", st)
	}
}

func TestStopButtonCancelsQuery(t *testing.T) {
	a := newTestApp(t)
	setBuffer(t, a, slowQuery, 0)

	// hidden while idle
	if w := onUI(t, a, func() int { _, _, w, _ := a.stopBtn.GetRect(); return w }); w != 0 {
		t.Fatalf("Stop button is %d columns wide while idle, want 0", w)
	}

	press(a, tcell.KeyCtrlR)
	waitFor(t, a, "the query to start", func() bool { return a.busy.Load() })

	// the elapsed-time ticker redraws, which lays the button out
	waitFor(t, a, "the Stop button to appear", func() bool {
		_, _, w, _ := a.stopBtn.GetRect()
		return w == stopBtnWidth
	})
	rect := onUI(t, a, func() [4]int {
		x, y, w, h := a.stopBtn.GetRect()
		return [4]int{x, y, w, h}
	})
	x, y := rect[0], rect[1]

	// click it: press, then release at the same spot
	a.app.QueueEvent(tcell.NewEventMouse(x+1, y, tcell.ButtonPrimary, tcell.ModNone))
	a.app.QueueEvent(tcell.NewEventMouse(x+1, y, tcell.ButtonNone, tcell.ModNone))

	waitFor(t, a, "the query to stop", func() bool { return !a.busy.Load() })
	if lg := logText(t, a); !strings.Contains(lg, "stopped") {
		t.Errorf("log does not report the stop:\n%s", lg)
	}
	// and it is hidden again
	waitFor(t, a, "the Stop button to disappear", func() bool {
		_, _, w, _ := a.stopBtn.GetRect()
		return w == 0
	})
}

func TestCtrlKCancelsScript(t *testing.T) {
	a := newTestApp(t)
	onUI(t, a, func() bool { a.runScript("testdata/slow_script.go"); return true })
	waitFor(t, a, "the script to start", func() bool { return a.busy.Load() })

	press(a, tcell.KeyCtrlK)
	waitFor(t, a, "the script to stop", func() bool { return !a.busy.Load() })

	lg := logText(t, a)
	if !strings.Contains(lg, "stopped") {
		t.Errorf("log does not report the stop:\n%s", lg)
	}
	// the script recognized the cancellation through sdb.IsCanceled, which
	// also proves the symbol is reachable from the interpreter
	if !strings.Contains(lg, "canceled cleanly") {
		t.Errorf("script did not see the cancellation:\n%s", lg)
	}
}

func TestBusyRefusesASecondRun(t *testing.T) {
	a := newTestApp(t)
	setBuffer(t, a, slowQuery, 0)

	press(a, tcell.KeyCtrlR)
	waitFor(t, a, "the query to start", func() bool { return a.busy.Load() })

	press(a, tcell.KeyCtrlR)
	waitFor(t, a, "the refusal", func() bool {
		return strings.Contains(a.logView.GetText(true), "busy")
	})

	press(a, tcell.KeyCtrlK)
	waitFor(t, a, "the query to stop", func() bool { return !a.busy.Load() })

	// the slot is free again: a quick query now runs to completion
	setBuffer(t, a, "SELECT 1 AS a", 0)
	press(a, tcell.KeyCtrlR)
	waitFor(t, a, "the second query to finish", func() bool { return a.lastRes != nil })
}

func TestCancelWithNothingRunning(t *testing.T) {
	a := newTestApp(t)
	press(a, tcell.KeyCtrlK)
	drain(t, a)
	if lg := logText(t, a); !strings.Contains(lg, "nothing is running") {
		t.Errorf("log does not say nothing is running:\n%s", lg)
	}
}
