package tui

import (
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/rohanthewiz/dbc/cats"
	"github.com/rohanthewiz/dbc/clip"
	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
)

// The test harness drives the Model the way Bubble Tea would, but
// synchronously: a message goes to Update, the command that comes back is
// run, and whatever message it produces goes back to Update, until nothing
// is left. Tests therefore read like a user session — press, click, drag —
// and then assert on the model or on the rendered frame.

var dbSeq atomic.Int64

// sent queues what m.send delivered during a command.
var sent []tea.Msg

// clipLog records what reached the (stubbed) clipboard in the current test.
var clipLog []clip.Content

// lastClip returns the most recent clipboard write.
func lastClip(t *testing.T) clip.Content {
	t.Helper()
	if len(clipLog) == 0 {
		t.Fatal("nothing was copied")
	}
	return clipLog[len(clipLog)-1]
}

// logText is the log pane as plain text.
func logText(m *Model) string { return m.logp.Text() }

// newTestModel builds a model over a fresh, seeded in-memory SQLite demo
// database, sized 120×40, with persistence off and the clipboard stubbed.
func newTestModel(t *testing.T) *Model {
	t.Helper()
	// Tests may themselves be running inside a cats pane (the developer's
	// terminal often is one). Without this, every test run would report
	// "dbc" states to that live pane and dial the real host's socket.
	for _, k := range []string{cats.EnvMarker, cats.EnvPaneID, cats.EnvControlSocket, cats.EnvHookSocket} {
		t.Setenv(k, "")
	}
	return newTestModelInHost(t)
}

// newTestModelInHost is newTestModel without clearing the cats environment,
// for tests that point it at a fake host first.
func newTestModelInHost(t *testing.T) *Model {
	t.Helper()
	name := "demo"
	dsn := fmt.Sprintf("file:tuitest%d?mode=memory&cache=shared", dbSeq.Add(1))
	cfg := &config.Config{
		ScriptsDir: "testdata", MaxRows: 1000, MaxDisplayRows: 2000,
		AIContextRows:     config.DefaultAIContextRows,
		DefaultConnection: name,
		Connections:       []config.Connection{{Name: name, Driver: "sqlite", DSN: dsn}},
	}
	mgr := db.NewManager(cfg)
	t.Cleanup(mgr.Close)
	if err := db.SeedDemo(mgr, name); err != nil {
		t.Fatalf("seed: %v", err)
	}
	clipLog = nil
	prev := clipWrite
	clipWrite = func(c clip.Content) (bool, error) { clipLog = append(clipLog, c); return c.HTML != "", nil }
	t.Cleanup(func() { clipWrite = prev })

	m := New(cfg, mgr, Options{NoPersist: true})
	t.Cleanup(m.shutdown)
	// m.send is how a running script reaches the model mid-run; queue what
	// it sends so drive can deliver it, as Program.Send would.
	m.send = func(msg tea.Msg) { sent = append(sent, msg) }
	m.editor.SetText("SELECT id, name, breed, age, adopted FROM cats ORDER BY age")
	drive(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	drive(t, m, nil, m.Init())
	return m
}

// drive feeds msg to Update and runs every command that results, feeding
// their messages back in, until the queue is empty. Tick messages are
// dropped rather than re-armed, so a run's elapsed-time ticker cannot keep
// the loop alive.
func drive(t *testing.T, m *Model, msg tea.Msg, cmds ...tea.Cmd) {
	t.Helper()
	// Bubble Tea calls View after every Update, and the frame it draws is
	// what the next mouse event is hit-tested against; render the same way,
	// or a click would be resolved against a layout from several steps ago.
	update := func(msg tea.Msg) tea.Cmd {
		_, c := m.Update(msg)
		m.render()
		return c
	}
	if _, isChat := msg.(chatEventMsg); isChat {
		// the pump's re-arm would block until the agent speaks again, which
		// it may never do; pumpChat re-arms on its own terms
		update(msg)
		return
	}
	if msg != nil {
		cmds = append(cmds, update(msg))
	}
	for guard := 0; len(cmds) > 0; guard++ {
		if guard > 500 {
			t.Fatal("drive: command loop did not settle")
		}
		c := cmds[0]
		cmds = cmds[1:]
		if c == nil {
			continue
		}
		out := c()
		for len(sent) > 0 { // what the command sent while it ran
			next := sent[0]
			sent = sent[1:]
			cmds = append(cmds, update(next))
		}
		switch out := out.(type) {
		case nil:
		case tea.BatchMsg:
			cmds = append(cmds, out...)
		case tickMsg:
		case chatEventMsg:
			// the assistant's event pump re-arms itself forever; tests that
			// use it drive it explicitly
			update(out)
		default:
			cmds = append(cmds, update(out))
		}
	}
}

// frame renders the current view as plain text.
func frame(m *Model) *Canvas {
	c, _ := m.render()
	return c
}

// key presses a key given as Bubble Tea names it ("ctrl+r", "enter", "a").
func key(t *testing.T, m *Model, name string) {
	t.Helper()
	drive(t, m, keyMsg(name))
}

func keyMsg(name string) tea.KeyPressMsg {
	k := tea.KeyPressMsg{}
	parts := strings.Split(name, "+")
	last := parts[len(parts)-1]
	for _, p := range parts[:len(parts)-1] {
		switch p {
		case "ctrl":
			k.Mod |= tea.ModCtrl
		case "alt":
			k.Mod |= tea.ModAlt
		case "shift":
			k.Mod |= tea.ModShift
		case "super":
			k.Mod |= tea.ModSuper
		}
	}
	switch last {
	case "enter":
		k.Code = tea.KeyEnter
	case "esc":
		k.Code = tea.KeyEscape
	case "tab":
		k.Code = tea.KeyTab
	case "up":
		k.Code = tea.KeyUp
	case "down":
		k.Code = tea.KeyDown
	case "left":
		k.Code = tea.KeyLeft
	case "right":
		k.Code = tea.KeyRight
	case "home":
		k.Code = tea.KeyHome
	case "end":
		k.Code = tea.KeyEnd
	case "backspace":
		k.Code = tea.KeyBackspace
	case "delete":
		k.Code = tea.KeyDelete
	case "pgup":
		k.Code = tea.KeyPgUp
	case "pgdown":
		k.Code = tea.KeyPgDown
	case "space":
		k.Code, k.Text = tea.KeySpace, " "
	default:
		r := []rune(last)[0]
		k.Code = r
		if k.Mod&(tea.ModCtrl|tea.ModAlt|tea.ModSuper) == 0 {
			k.Text = last
			if k.Mod&tea.ModShift != 0 {
				k.Text = strings.ToUpper(last)
			}
		}
	}
	return k
}

// typeText types a string one key at a time.
func typeText(t *testing.T, m *Model, s string) {
	t.Helper()
	for _, r := range s {
		drive(t, m, tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

// click presses and releases the left button at (x, y).
func click(t *testing.T, m *Model, x, y int) {
	t.Helper()
	drive(t, m, tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	drive(t, m, tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
}

// rightClick presses the right button at (x, y).
func rightClick(t *testing.T, m *Model, x, y int) {
	t.Helper()
	drive(t, m, tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseRight})
}

// findText returns the position of the first occurrence of s in the frame.
func findText(t *testing.T, c *Canvas, s string) (int, int) {
	t.Helper()
	for y := 0; y < c.H; y++ {
		line := []rune(c.Line(y))
		if i := strings.Index(string(line), s); i >= 0 {
			// convert the byte index to a cell column
			return width(string(line)[:i]), y
		}
	}
	t.Fatalf("%q not on screen:\n%s", s, c.Text())
	return 0, 0
}

// TestDumpFrame prints a rendered frame when DBC_TUI_DUMP is set, for
// eyeballing the layout during development.
func TestDumpFrame(t *testing.T) {
	if os.Getenv("DBC_TUI_DUMP") == "" {
		t.Skip("set DBC_TUI_DUMP=1 to print a frame")
	}
	m := newTestModel(t)
	key(t, m, "ctrl+r")
	fmt.Println(frame(m).Text())
}
