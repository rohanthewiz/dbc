package tui

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/model"
	"github.com/rohanthewiz/dbc/workspace"
)

// manyPrints is a script that prints n numbered lines.
func manyPrints(n int) string {
	return fmt.Sprintf(`// Prints %d lines.
package main

import (
	"fmt"

	"github.com/rohanthewiz/dbc/sdb"
)

func Run(s *sdb.S) error {
	for i := 0; i < %d; i++ {
		s.Print(fmt.Sprintf("line %%d", i))
	}
	return nil
}
`, n, n)
}

// writeTemp writes a script into a temp dir and returns its path.
func writeTemp(tb testing.TB, name, src string) string {
	tb.Helper()
	path := filepath.Join(tb.TempDir(), name)
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		tb.Fatal(err)
	}
	return path
}

// countPrintsMsgs counts the wake-ups among what m.send delivered.
func countPrintsMsgs(msgs []tea.Msg) int {
	n := 0
	for _, msg := range msgs {
		if _, ok := msg.(printsMsg); ok {
			n++
		}
	}
	return n
}

// Lines queued while the event loop is busy cost one wake-up between
// them, not a message each, and land in the log in order on the next
// message — whatever that message is.
func TestScriptPrintsAreBatched(t *testing.T) {
	m := newTestModel(t)
	sent = nil
	k := m.active().key
	for i := range 1500 {
		m.scriptPrinted(k, &workspace.ScriptPrint{Text: fmt.Sprintf("batched %d", i), Conn: config.DemoSQLite})
	}
	if n := countPrintsMsgs(sent); n != 1 || len(sent) != 1 {
		t.Fatalf("1,500 lines sent %d messages (%d wake-ups), want one wake-up", len(sent), n)
	}
	sent = nil
	// any message drains the queue first: here a key, not the wake-up
	drive(t, m, keyMsg("ctrl+l"))
	lines := strings.Split(strings.TrimSpace(logText(m)), "\n")
	at := -1
	for i, l := range lines {
		if strings.HasSuffix(l, " batched 0") {
			at = i
			break
		}
	}
	if at < 0 || len(lines)-at < 1500 {
		t.Fatalf("the batch is not in the log:\n%s", logText(m))
	}
	for i := range 1500 {
		if !strings.HasSuffix(lines[at+i], fmt.Sprintf(" batched %d", i)) {
			t.Fatalf("log line %d = %q, want batched %d", at+i, lines[at+i], i)
		}
	}
	if q := m.prints.take(); len(q) != 0 {
		t.Errorf("%d lines left queued", len(q))
	}
}

// Once printBacklog lines wait, each further push wakes the loop again —
// the caller then waits in Program.Send, so a script cannot outrun a busy
// loop without bound.
func TestScriptPrintsBackpressure(t *testing.T) {
	var q printQueue
	ev := &workspace.ScriptPrint{Text: "x"}
	if w := q.push(1, ev); w != wakePost {
		t.Fatalf("the first line: %v, want a posted wake-up", w)
	}
	for i := 2; i < printBacklog; i++ {
		if w := q.push(1, ev); w != wakeNone {
			t.Fatalf("line %d asked for a wake-up (%v) with one on its way", i, w)
		}
	}
	for range 3 {
		if w := q.push(1, ev); w != wakeWait {
			t.Fatalf("a line past the backlog: %v, want a send that waits", w)
		}
	}
	if got := len(q.take()); got != printBacklog+2 {
		t.Fatalf("took %d", got)
	}
	if w := q.push(1, ev); w != wakePost {
		t.Fatalf("after a drain the first line: %v, want a posted wake-up", w)
	}
}

// A real script's lines all land, in order and above its "completed" note,
// even when it prints more than the log keeps. (2,300 lines: past the
// backlog, each line wakes the loop again, and the test harness, which
// never blocks in send, counts each wake-up as a command; it allows 500.)
func TestScriptPrintsLandBeforeItsEnd(t *testing.T) {
	const n = 2300
	m := newTestModel(t)
	path := writeTemp(t, "many.go", manyPrints(n))
	drive(t, m, nil, m.runScript(path))
	lines := strings.Split(strings.TrimSpace(logText(m)), "\n")
	last, done := -1, -1
	for i, l := range lines {
		switch {
		case strings.HasSuffix(l, fmt.Sprintf(" line %d", n-1)):
			last = i
		case strings.Contains(l, "script many.go completed"):
			done = i
		}
	}
	if last < 0 || done < 0 || last > done {
		t.Fatalf("last print at %d, completed at %d:\n%s", last, done, strings.Join(lines[max(0, len(lines)-5):], "\n"))
	}
	// the log keeps its last logMaxLines lines: everything above the last
	// print is the prints before it, consecutive, to the top of the log
	for i := last; i >= 0; i-- {
		if want := fmt.Sprintf(" line %d", n-1-(last-i)); !strings.HasSuffix(lines[i], want) {
			t.Fatalf("log line %d = %q, want …%s", i, lines[i], want)
		}
	}
	if last < logMaxLines-5 {
		t.Errorf("only %d prints kept", last+1)
	}
}

// A background tab's lines go to its connection's log as they come, not
// onto the tab's pending list to be replayed on its return; a closed tab's
// lines go nowhere.
func TestBackgroundScriptPrintsSkipThePendingList(t *testing.T) {
	m := newTestModel(t)
	bg := m.active().key
	key(t, m, "alt+t") // tab 2 on screen, same connection
	m.scriptPrinted(bg, &workspace.ScriptPrint{Text: "from the background", Conn: config.DemoSQLite})
	drive(t, m, printsMsg{})
	if len(m.tabs[0].pending) != 0 {
		t.Errorf("the print was queued on its tab: %d pending", len(m.tabs[0].pending))
	}
	if !strings.Contains(logText(m), "from the background") {
		t.Errorf("not in the connection's log:\n%s", logText(m))
	}
	// a print delivered as a message of its own does the same
	drive(t, m, tabMsg{key: bg, msg: &workspace.ScriptPrint{Text: "as a message", Conn: config.DemoSQLite}})
	if len(m.tabs[0].pending) != 0 || !strings.Contains(logText(m), "as a message") {
		t.Errorf("pending %d, log:\n%s", len(m.tabs[0].pending), logText(m))
	}

	key(t, m, "alt+1")
	key(t, m, "alt+w") // closes tab 1
	m.scriptPrinted(bg, &workspace.ScriptPrint{Text: "after the close", Conn: config.DemoSQLite})
	drive(t, m, printsMsg{})
	if strings.Contains(logText(m), "after the close") {
		t.Error("a closed tab's print reached the log")
	}
}

// A background tab keeps only the last of a run of shows: each replay
// would only put its result in the grid over the one before. A show of
// nothing is not queued, and a show behind another event keeps both.
func TestBackgroundScriptShowsKeepTheLast(t *testing.T) {
	m := newTestModel(t)
	bg := m.active().key
	key(t, m, "alt+t")
	show := func(name string) {
		var r *model.Result
		if name != "" {
			r = &model.Result{Columns: []string{name}, Rows: [][]string{{"1"}}}
		}
		drive(t, m, tabMsg{key: bg, msg: &workspace.ScriptShow{Result: r, Conn: config.DemoSQLite}})
	}
	pendingShows := func() (names []string) {
		for _, msg := range m.tabs[0].pending {
			if s, ok := msg.(*workspace.ScriptShow); ok {
				names = append(names, s.Result.Columns[0])
			}
		}
		return names
	}
	for i := range 50 {
		show(fmt.Sprint("c", i))
	}
	show("")
	if got := pendingShows(); len(got) != 1 || got[0] != "c49" || len(m.tabs[0].pending) != 1 {
		t.Fatalf("pending shows %v (%d pending), want [c49]", got, len(m.tabs[0].pending))
	}
	drive(t, m, tabMsg{key: bg, msg: &workspace.ExplainDone{Stale: true}})
	show("after")
	if got := pendingShows(); len(got) != 2 || got[1] != "after" {
		t.Fatalf("pending shows %v, want [c49 after]", got)
	}
}

// runWatch runs a Model in a real Program and says when a run has landed.
// done is touched only in Update, on the event loop: the benchmark hands
// it over inside the message that starts the run.
type runWatch struct {
	m    *Model
	done chan struct{}
}

// startScript asks the event loop to run a script; done is closed when
// its RunDone has been handled.
type startScript struct {
	path string
	done chan struct{}
}

func (w *runWatch) Init() tea.Cmd  { return nil }
func (w *runWatch) View() tea.View { return w.m.View() }

func (w *runWatch) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case startScript:
		w.done = msg.done
		return w, w.m.runScript(msg.path)
	case tabMsg:
		_, cmd := w.m.Update(msg)
		if _, ok := msg.msg.(*workspace.RunDone); ok && w.done != nil {
			close(w.done)
			w.done = nil
		}
		return w, cmd
	}
	_, cmd := w.m.Update(msg)
	return w, cmd
}

// BenchmarkScriptPrints runs a script printing 2,000 lines in a real
// Bubble Tea program (output discarded), over a full log, and waits for
// its run to land. Before N-278 each line was a message and a frame, and
// the script waited for both.
func BenchmarkScriptPrints(b *testing.B) {
	m := newTestModel(b)
	fillLog(m)
	path := writeTemp(b, "many.go", manyPrints(2000))
	// the size as an option: output that is no terminal reports 0×0 when
	// the program starts, which would replace a size sent before it
	p := tea.NewProgram(&runWatch{m: m}, tea.WithInput(nil), tea.WithOutput(io.Discard),
		tea.WithWindowSize(200, 50))
	m.send, m.post = p.Send, m.sendLater // the program's, not the harness's
	ran := make(chan error, 1)
	go func() { _, err := p.Run(); ran <- err }()
	for b.Loop() {
		done := make(chan struct{})
		p.Send(startScript{path: path, done: done})
		<-done
	}
	p.Quit()
	if err := <-ran; err != nil {
		b.Fatal(err)
	}
	// the program has ended: the model is this goroutine's to read
	if m.w != 200 || m.h != 50 {
		b.Fatalf("drawn at %d×%d", m.w, m.h)
	}
	if log := logText(m); !strings.Contains(log, " line 1999\n") {
		b.Fatalf("the script's lines are not in the log:\n%s", log[max(0, len(log)-600):])
	}
}
