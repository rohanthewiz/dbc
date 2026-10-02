package e2e

// The harness: build dbc, give it a HOME of its own, run the TUI in a
// pseudo-terminal, and read what it draws through a VT emulator. The
// checks themselves are in tui_test.go.

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
	"github.com/creack/pty"
)

const (
	// cols × rows is the terminal's size: big enough that every pane has
	// room, as on a laptop's full-screen terminal.
	cols, rows = 140, 45

	// waitLimit bounds every wait for the screen to reach a state. A wait
	// that passes returns as soon as it does.
	waitLimit = 15 * time.Second
)

// env is one run's world: the temporary HOME and the built binary.
type env struct {
	home, bin string
}

// setup builds dbc and writes its config and databases, or skips the test
// unless DBC_TUI_E2E is set.
func setup(t *testing.T) *env {
	t.Helper()
	if os.Getenv("DBC_TUI_E2E") == "" {
		t.Skip("terminal checks of the TUI: set DBC_TUI_E2E=1 to run them")
	}
	e := &env{home: t.TempDir()}
	e.bin = filepath.Join(e.home, "dbc")
	build(t, e.bin)
	e.writeConfig(t)
	e.seed(t)
	return e
}

// build compiles dbc from this checkout (two levels up), so the run always
// exercises the code in the working tree.
func build(t *testing.T, out string) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Dir = root
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building dbc: %v\n%s", err, b)
	}
}

// writeConfig writes HOME/.config/dbc/config.toml: two SQLite files, so
// switching connection has somewhere to go. A config file replaces dbc's
// built-in demos, so the connections are exactly these.
func (e *env) writeConfig(t *testing.T) {
	t.Helper()
	dir := filepath.Join(e.home, ".config", "dbc")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("default_connection = \"lite\"\n")
	conn := func(name, dsn string) {
		fmt.Fprintf(&b, "\n[[connection]]\nname = %q\ndriver = \"sqlite\"\ndsn = %q\n", name, dsn)
	}
	conn("lite", "file:"+filepath.Join(e.home, "lite.db"))
	conn("lite2", "file:"+filepath.Join(e.home, "lite2.db"))
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// seed fills the connections through dbc's own headless mode — the same
// binary, so no SQL driver has to be a dependency of this module.
func (e *env) seed(t *testing.T) {
	t.Helper()
	e.dbc(t, "lite", `CREATE TABLE owners (id INTEGER PRIMARY KEY, name TEXT NOT NULL);
CREATE TABLE cats (id INTEGER PRIMARY KEY, name TEXT NOT NULL, breed TEXT, age INTEGER, owner_id INTEGER REFERENCES owners(id));
INSERT INTO owners (name) VALUES ('Ada'), ('Bo');
INSERT INTO cats (name, breed, age, owner_id) VALUES ('Tom', 'tabby', 3, 1), ('Mia', 'siamese', 5, 2), ('Leo', NULL, 1, 1)`)
	e.dbc(t, "lite2", `CREATE TABLE dogs (id INTEGER PRIMARY KEY, name TEXT); INSERT INTO dogs (name) VALUES ('Rex')`)
}

// dbc runs one headless query on conn and fails the test if it fails.
func (e *env) dbc(t *testing.T, conn, sql string) {
	t.Helper()
	cmd := exec.Command(e.bin, "-c", conn, sql)
	cmd.Dir = e.home
	cmd.Env = e.environ()
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("dbc -c %s: %v\n%s", conn, err, b)
	}
}

// environ is the binary's environment: this process's, with HOME pointing
// at the run's directory (tabs, history and schema picks land there, never
// in the real ones), a terminal type that offers colors and the mouse, and
// without the variables that would reach outside the run.
//
// CATS_*: a dbc that inherits them (a Claude Code session on dbc runs in a
// cats pane) reports to the LIVE pane and dials its control socket.
func (e *env) environ() []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "CATS_") || k == "HOME" || k == "TERM" || k == "DBC_DEMO" {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "HOME="+e.home, "TERM=xterm-256color", "COLORTERM=truecolor")
}

// term is the TUI running in a pseudo-terminal, read through an emulator.
//
//	dbc ──pty──► io.Copy ──► emulator (the screen the test reads)
//	 ▲                          │
//	 └──pty◄── io.Copy ◄────────┘  the emulator's replies to the app's
//	                               queries, and keys and mouse it encodes
//
// Keys and the mouse go through the emulator rather than as raw bytes, so
// they are encoded the way the modes the app switched on ask for (the
// kitty keyboard protocol, SGR mouse reporting), as a real terminal does.
type term struct {
	t    *testing.T
	em   *vt.SafeEmulator
	ptmx *os.File
	cmd  *exec.Cmd
	done chan struct{} // closed when the process has exited
}

// start runs dbc's TUI with args in a cols×rows pseudo-terminal.
func (e *env) start(t *testing.T, args ...string) *term {
	t.Helper()
	cmd := exec.Command(e.bin, args...)
	cmd.Dir = e.home
	cmd.Env = e.environ()
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: rows, Cols: cols})
	if err != nil {
		t.Fatalf("starting dbc in a pty: %v", err)
	}
	u := &term{t: t, em: vt.NewSafeEmulator(cols, rows), ptmx: ptmx, cmd: cmd, done: make(chan struct{})}
	go func() { _, _ = io.Copy(u.em, ptmx) }()
	go func() { _, _ = io.Copy(ptmx, u.em) }()
	go func() { _ = cmd.Wait(); close(u.done) }()
	t.Cleanup(func() {
		select {
		case <-u.done:
		default:
			_ = cmd.Process.Kill()
			<-u.done
		}
		_ = ptmx.Close()
		_ = u.em.Close()
	})
	return u
}

// screen is the terminal's text, one line per row, styles stripped.
func (u *term) screen() []string {
	return strings.Split(ansi.Strip(u.em.Render()), "\n")
}

// waitFor waits until the screen holds every one of want, failing the test
// with the screen as it was when the wait gave up.
func (u *term) waitFor(want ...string) {
	u.t.Helper()
	u.waitUntil(strings.Join(want, ", "), func(s string) bool {
		for _, w := range want {
			if !strings.Contains(s, w) {
				return false
			}
		}
		return true
	})
}

// waitGone waits until the screen no longer holds text.
func (u *term) waitGone(text string) {
	u.t.Helper()
	u.waitUntil("no "+text, func(s string) bool { return !strings.Contains(s, text) })
}

func (u *term) waitUntil(what string, ok func(string) bool) {
	u.t.Helper()
	deadline := time.Now().Add(waitLimit)
	for {
		s := strings.Join(u.screen(), "\n")
		if ok(s) {
			return
		}
		if time.Now().After(deadline) {
			u.t.Fatalf("waited %s for %s; the screen:\n%s", waitLimit, what, s)
		}
		time.Sleep(40 * time.Millisecond)
	}
}

// find returns the cell where text first appears on screen (0-based column
// and row, in cells), failing the test when it is not there.
func (u *term) find(text string) (x, y int) {
	u.t.Helper()
	for row, line := range u.screen() {
		if i := strings.Index(line, text); i >= 0 {
			// the index is in bytes; the mouse wants cells
			return ansi.StringWidth(line[:i]), row
		}
	}
	u.t.Fatalf("%q is not on the screen:\n%s", text, strings.Join(u.screen(), "\n"))
	return 0, 0
}

// lineWith returns the first screen line holding text, or "".
func (u *term) lineWith(text string) string {
	for _, line := range u.screen() {
		if strings.Contains(line, text) {
			return line
		}
	}
	return ""
}

// key presses one key: a rune (with mods) or a special key code
// (uv.KeyEnter, uv.KeyTab …).
func (u *term) key(code rune, mod ...uv.KeyMod) {
	var m uv.KeyMod
	for _, x := range mod {
		m |= x
	}
	k := uv.KeyPressEvent{Code: code, Mod: m}
	if m == 0 && code >= ' ' && code < uv.KeyExtended {
		k.Text = string(code)
	}
	u.em.SendKey(k)
	time.Sleep(20 * time.Millisecond) // a keystroke's worth: the app reads them one by one
}

// ctrl presses Ctrl plus a letter.
func (u *term) ctrl(r rune) { u.key(r, uv.ModCtrl) }

// typeText types s a key at a time, as a person does, so the editor's
// per-key behavior (completion opening on the second letter) runs.
func (u *term) typeText(s string) {
	for _, r := range s {
		u.key(r)
	}
}

// click presses and releases the left button on the cell where text
// appears, offset by dx cells.
func (u *term) click(text string, dx int) {
	u.t.Helper()
	x, y := u.find(text)
	u.clickAt(x+dx, y)
}

func (u *term) clickAt(x, y int) {
	u.em.SendMouse(vt.MouseClick{X: x, Y: y, Button: vt.MouseLeft})
	u.em.SendMouse(vt.MouseRelease{X: x, Y: y, Button: vt.MouseLeft})
	time.Sleep(30 * time.Millisecond)
}

// quit asks dbc to quit with Ctrl+Q and waits for the process to end.
func (u *term) quit() {
	u.t.Helper()
	u.ctrl('q')
	select {
	case <-u.done:
	case <-time.After(waitLimit):
		u.t.Fatalf("dbc did not exit on Ctrl+Q; the screen:\n%s", strings.Join(u.screen(), "\n"))
	}
}
