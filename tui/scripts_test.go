package tui

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/rohanthewiz/dbc/scripts"
	"github.com/rohanthewiz/dbc/userdata"
)

// The scripts browser (scripts.go), driven as a user would: Ctrl+O, keys,
// clicks, the name prompts, and an editor stand-in for $EDITOR.

// scriptsModel is a test model whose scripts dir is a fresh temp dir, with
// the editor stubbed: each "edit" runs edit on the file's path (nil leaves
// it as it is) and is recorded in *edits.
func scriptsModel(t *testing.T, edit func(path string)) (*Model, string, *[]string) {
	t.Helper()
	m := newTestModel(t)
	dir := filepath.Join(t.TempDir(), "scripts")
	m.cfg.ScriptsDir = dir
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "true") // on PATH everywhere, so LookPath passes
	var edits []string
	prev := execEditor
	execEditor = func(c *exec.Cmd, fn tea.ExecCallback) tea.Cmd {
		path := c.Args[len(c.Args)-1]
		return func() tea.Msg {
			edits = append(edits, path)
			if edit != nil {
				edit(path)
			}
			return fn(nil)
		}
	}
	t.Cleanup(func() { execEditor = prev })
	return m, dir, &edits
}

// browser is the open scripts browser, or the test fails.
func browser(t *testing.T, m *Model) *scriptsModal {
	t.Helper()
	sm, ok := m.modal.(*scriptsModal)
	if !ok {
		t.Fatalf("modal = %T, want the scripts browser; log:\n%s", m.modal, logText(m))
	}
	return sm
}

// prompt is the open name prompt, or the test fails.
func prompt(t *testing.T, m *Model) *promptModal {
	t.Helper()
	p, ok := m.modal.(*promptModal)
	if !ok {
		t.Fatalf("modal = %T, want a prompt; log:\n%s", m.modal, logText(m))
	}
	return p
}

// rowLabels lists the browser's rows as "kind:label" ("head:" for
// headings), for comparing sections.
func rowLabels(sm *scriptsModal) []string {
	var out []string
	for _, it := range sm.lst.items {
		kind := "head"
		if r, ok := it.data.(scriptRow); ok && !it.head {
			kind = map[scriptRowKind]string{rowScript: "script", rowTemplate: "template", rowExample: "example", rowTrash: "trash"}[r.kind]
		}
		out = append(out, kind+":"+strings.TrimSpace(it.label))
	}
	return out
}

// cursorTo puts the browser's cursor on the first row whose label is label.
func cursorTo(t *testing.T, sm *scriptsModal, label string) {
	t.Helper()
	for i, it := range sm.lst.items {
		if !it.head && it.label == label {
			sm.lst.cur = i
			return
		}
	}
	t.Fatalf("no row %q in %v", label, rowLabels(sm))
}

func writeScript(t *testing.T, dir, name, text string) {
	t.Helper()
	if _, _, err := userdata.SaveScript(dir, name, text, ""); err != nil {
		t.Fatal(err)
	}
}

const printScript = `// Prints a line.
package main

import "github.com/rohanthewiz/dbc/sdb"

func Run(s *sdb.S) error {
	s.Print("hello from the script")
	return nil
}
`

// An empty dir offers the templates where the scripts would be, then the
// examples; the cursor starts on the first pickable row, never a heading.
func TestScriptsBrowserEmptyDir(t *testing.T) {
	m, dir, _ := scriptsModel(t, nil)
	key(t, m, "ctrl+o")
	sm := browser(t, m)
	got := rowLabels(sm)
	if got[0] != "head:Scripts" || !strings.HasPrefix(got[1], "head:none yet in") || got[2] != "template:Blank script" {
		t.Errorf("rows start %v", got[:3])
	}
	if n := len(scripts.Templates()) + len(scripts.Examples()) + 3; len(got) != n {
		t.Errorf("%d rows, want %d: %v", len(got), n, got)
	}
	if r, _ := sm.row(); r.kind != rowTemplate {
		t.Errorf("cursor on %+v, want the first template", r)
	}
	// the title names the resolved directory (the frame cuts a temp dir's
	// long path, so the title itself is checked)
	if sm.title() != "Scripts · "+dir {
		t.Errorf("title %q", sm.title())
	}
	c := frame(m)
	findText(t, c, "Scripts · ")
	findText(t, c, "Examples · read-only")
	// moving up from the first row stays on it, never on the heading
	key(t, m, "up")
	if r, _ := sm.row(); r.kind != rowTemplate {
		t.Errorf("up moved the cursor onto %+v", r)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("opening the browser must not create the scripts dir")
	}
}

// New from a template: the name prompt offers the template's name with the
// stem selected, the file is written with the connection filled in, the
// editor runs on it, and its check lands in the log as path:line:col lines
// before the browser comes back on the script.
func TestScriptsBrowserNewFromTemplate(t *testing.T) {
	broken := "package main\n\nimport \"github.com/rohanthewiz/dbc/sdb\"\n\nfunc Run(s *sdb.S) error {\n\treturn undefinedThing\n}\n"
	m, dir, edits := scriptsModel(t, func(path string) {
		if err := os.WriteFile(path, []byte(broken), 0o600); err != nil {
			t.Error(err)
		}
	})
	key(t, m, "ctrl+o")
	cursorTo(t, browser(t, m), "Query and show")
	key(t, m, "enter")
	p := prompt(t, m)
	if p.field.Text() != "query.go" {
		t.Errorf("offered %q", p.field.Text())
	}
	typeText(t, m, "nightly") // replaces the selected stem, keeps .go
	if p.field.Text() != "nightly.go" {
		t.Fatalf("field %q", p.field.Text())
	}
	var created string
	execEditorWas := execEditor
	execEditor = func(c *exec.Cmd, fn tea.ExecCallback) tea.Cmd {
		// what the editor is handed: the file as created from the template
		bs, _ := os.ReadFile(c.Args[len(c.Args)-1])
		created = string(bs)
		return execEditorWas(c, fn)
	}
	key(t, m, "enter")
	if len(*edits) != 1 || (*edits)[0] != filepath.Join(dir, "nightly.go") {
		t.Fatalf("edits = %v", *edits)
	}
	if !strings.Contains(created, `"demo-sqlite"`) {
		t.Errorf("the template should name the active connection:\n%s", created)
	}
	log := logText(m)
	if !strings.Contains(log, "nightly.go saved — the check found 1 error(s)") ||
		!strings.Contains(log, "nightly.go:6:9: undefined: undefinedThing") {
		t.Errorf("log:\n%s", log)
	}
	sm := browser(t, m)
	if r, _ := sm.row(); r.kind != rowScript || r.name != "nightly.go" {
		t.Errorf("browser back on %+v", r)
	}
}

// A name that is taken, or not a script name, is said under the field and
// the prompt stays; Esc goes back to the browser.
func TestScriptsBrowserNamePrompt(t *testing.T) {
	m, dir, edits := scriptsModel(t, nil)
	writeScript(t, dir, "a.go", printScript)
	key(t, m, "ctrl+o")
	key(t, m, "n")
	if m.menu == nil || !strings.Contains(m.menu.items[0].label, "new script from") {
		t.Fatalf("n should open the template menu, menu = %+v", m.menu)
	}
	key(t, m, "enter") // the first template: Blank script
	p := prompt(t, m)
	if p.field.Text() != "script.go" {
		t.Errorf("blank offers %q", p.field.Text())
	}
	typeText(t, m, "a")
	key(t, m, "enter")
	if p.errMsg != "a.go exists — pick another name" || m.modal != p {
		t.Errorf("taken: err %q, modal %T", p.errMsg, m.modal)
	}
	p.field.SetText("../x")
	key(t, m, "enter")
	if !strings.HasPrefix(p.errMsg, "not a script name") {
		t.Errorf("bad name: err %q", p.errMsg)
	}
	key(t, m, "esc")
	browser(t, m)
	if len(*edits) != 0 {
		t.Errorf("nothing was created, yet the editor ran: %v", *edits)
	}
}

// Enter runs a script; its s.Print reaches the log.
func TestScriptsBrowserRuns(t *testing.T) {
	m, dir, _ := scriptsModel(t, nil)
	writeScript(t, dir, "hello.go", printScript)
	key(t, m, "ctrl+o")
	if r, _ := browser(t, m).row(); r.name != "hello.go" {
		t.Fatalf("cursor on %+v", r)
	}
	key(t, m, "enter")
	if m.modal != nil {
		t.Errorf("the browser should close on a run")
	}
	if log := logText(m); !strings.Contains(log, "hello from the script") || !strings.Contains(log, "script hello.go completed") {
		t.Errorf("log:\n%s", log)
	}
}

// / filters by name and description; Esc leaves the filter (keeping it),
// a second Esc closes. Letters typed in the filter are not commands.
func TestScriptsBrowserFilter(t *testing.T) {
	m, dir, _ := scriptsModel(t, nil)
	writeScript(t, dir, "alpha.go", printScript)
	writeScript(t, dir, "beta.go", strings.Replace(printScript, "Prints a line", "Exports the delta", 1))
	key(t, m, "ctrl+o")
	sm := browser(t, m)
	key(t, m, "/")
	typeText(t, m, "delta") // "d" and "e" here type, not duplicate/edit
	if m.modal != sm || !sm.filtering {
		t.Fatalf("modal %T, filtering %v", m.modal, sm.filtering)
	}
	var mine []string
	for _, l := range rowLabels(sm) {
		if strings.HasPrefix(l, "script:") {
			mine = append(mine, l)
		}
	}
	if !slices.Equal(mine, []string{"script:beta.go"}) {
		t.Errorf("filtered scripts = %v (all rows %v)", mine, rowLabels(sm))
	}
	key(t, m, "esc")
	if sm.filtering || sm.filter.Text() != "delta" || m.modal != sm {
		t.Errorf("first Esc: filtering %v, filter %q, modal %T", sm.filtering, sm.filter.Text(), m.modal)
	}
	key(t, m, "esc")
	if m.modal != nil {
		t.Error("second Esc should close")
	}
}

// Rename, trash, restore — and a restore onto a name taken meanwhile asks
// for another.
func TestScriptsBrowserRenameTrashRestore(t *testing.T) {
	m, dir, _ := scriptsModel(t, nil)
	writeScript(t, dir, "old.go", printScript)
	key(t, m, "ctrl+o")
	key(t, m, "r")
	p := prompt(t, m)
	if p.field.Text() != "old.go" {
		t.Errorf("rename offers %q", p.field.Text())
	}
	typeText(t, m, "new")
	key(t, m, "enter")
	sm := browser(t, m)
	if r, _ := sm.row(); r.name != "new.go" {
		t.Errorf("after rename the cursor is on %+v", r)
	}
	if _, err := os.Stat(filepath.Join(dir, "new.go")); err != nil {
		t.Fatal(err)
	}

	key(t, m, "delete")
	if m.modal != sm {
		t.Fatalf("trash should keep the browser open, modal %T", m.modal)
	}
	if _, err := os.Stat(filepath.Join(dir, "new.go")); !os.IsNotExist(err) {
		t.Error("new.go should be in the trash")
	}
	if !slices.Contains(rowLabels(sm), "head:Trash (1) ▸ t shows it") {
		t.Errorf("rows %v", rowLabels(sm))
	}
	key(t, m, "t")
	cursorTo(t, sm, "new.go")
	if r, _ := sm.row(); r.kind != rowTrash {
		t.Fatalf("cursor on %+v", r)
	}

	// someone made a new.go meanwhile: the restore asks for another name
	writeScript(t, dir, "new.go", printScript)
	key(t, m, "enter")
	p = prompt(t, m)
	if p.field.Text() != "new-2.go" {
		t.Errorf("restore offers %q", p.field.Text())
	}
	key(t, m, "enter")
	if r, _ := browser(t, m).row(); r.name != "new-2.go" {
		t.Errorf("after restore the cursor is on %+v", r)
	}
	if len(userdata.ListTrash(dir)) != 0 {
		t.Error("the trash should be empty again")
	}
}

// Cancelling a rename lands back in the browser, on the same script.
func TestScriptsBrowserRenameCancel(t *testing.T) {
	m, dir, _ := scriptsModel(t, nil)
	writeScript(t, dir, "a.go", printScript)
	writeScript(t, dir, "b.go", printScript)
	key(t, m, "ctrl+o")
	cursorTo(t, browser(t, m), "b.go")
	key(t, m, "r")
	key(t, m, "esc")
	if r, _ := browser(t, m).row(); r.name != "b.go" {
		t.Errorf("back on %+v", r)
	}
}

// Enter on an example makes your own copy, byte for byte, then edits it;
// an untouched copy checks clean.
func TestScriptsBrowserCopiesAnExample(t *testing.T) {
	m, dir, edits := scriptsModel(t, nil)
	key(t, m, "ctrl+o")
	cursorTo(t, browser(t, m), "loop_params.go")
	key(t, m, "enter")
	key(t, m, "enter") // accept the offered name
	ex, _ := scripts.ExampleByName("loop_params.go")
	text, _, err := userdata.ReadScript(dir, "loop_params.go")
	if err != nil || text != ex.Text {
		t.Fatalf("copy: %v (equal %v)", err, text == ex.Text)
	}
	if len(*edits) != 1 {
		t.Errorf("edits = %v", *edits)
	}
	if log := logText(m); !strings.Contains(log, "loop_params.go unchanged — checked, no problems") {
		t.Errorf("log:\n%s", log)
	}
	// now listed under Scripts, as well as under Examples
	if labels := rowLabels(browser(t, m)); !slices.Contains(labels, "script:loop_params.go") {
		t.Errorf("rows %v", labels)
	}
}

// Rename, trash and the rest are for scripts of yours: on an example they
// say so instead of doing nothing.
func TestScriptsBrowserScriptOnlyActions(t *testing.T) {
	m, _, _ := scriptsModel(t, nil)
	key(t, m, "ctrl+o")
	cursorTo(t, browser(t, m), "copy_table.go")
	key(t, m, "r")
	key(t, m, "delete")
	browser(t, m)
	if log := logText(m); strings.Count(log, "only a script of yours can be") != 2 {
		t.Errorf("log:\n%s", log)
	}
}

// With no editor on PATH the log says how to set one, and nothing is
// suspended.
func TestScriptsBrowserNoEditor(t *testing.T) {
	m, dir, edits := scriptsModel(t, nil)
	t.Setenv("EDITOR", "no-such-editor-dbc-test")
	writeScript(t, dir, "a.go", printScript)
	key(t, m, "ctrl+o")
	key(t, m, "e")
	if len(*edits) != 0 || !strings.Contains(logText(m), `no editor "no-such-editor-dbc-test" on PATH`) {
		t.Errorf("edits %v; log:\n%s", *edits, logText(m))
	}
}

// $VISUAL wins over $EDITOR, and either may carry arguments.
func TestEditorLine(t *testing.T) {
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "")
	if got := editorLine(); !slices.Equal(got, []string{"vi"}) {
		t.Errorf("default %v", got)
	}
	t.Setenv("EDITOR", "code -w")
	if got := editorLine(); !slices.Equal(got, []string{"code", "-w"}) {
		t.Errorf("EDITOR %v", got)
	}
	t.Setenv("VISUAL", "nvim")
	if got := editorLine(); !slices.Equal(got, []string{"nvim"}) {
		t.Errorf("VISUAL %v", got)
	}
}

// A double-click on a script edits it; a click on the Trash heading folds
// it open.
func TestScriptsBrowserMouse(t *testing.T) {
	m, dir, edits := scriptsModel(t, nil)
	writeScript(t, dir, "a.go", printScript)
	writeScript(t, dir, "gone.go", printScript)
	if _, err := userdata.TrashScript(dir, "gone.go"); err != nil {
		t.Fatal(err)
	}
	key(t, m, "ctrl+o")
	c := frame(m)
	x, y := findText(t, c, "Trash (1)")
	click(t, m, x, y)
	if !slices.Contains(rowLabels(browser(t, m)), "trash:gone.go") {
		t.Errorf("rows %v", rowLabels(browser(t, m)))
	}
	x, y = findText(t, frame(m), "a.go")
	drive(t, m, tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	drive(t, m, tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
	drive(t, m, tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	if len(*edits) != 1 || filepath.Base((*edits)[0]) != "a.go" {
		t.Errorf("double-click: edits %v", *edits)
	}
}

func TestAgo(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	for d, want := range map[time.Duration]string{
		10 * time.Second: "now", 5 * time.Minute: "5m", 3 * time.Hour: "3h",
		50 * time.Hour: "2d", 40 * 24 * time.Hour: "Aug 27",
		400 * 24 * time.Hour: "Sep 1 2025",
	} {
		if got := ago(now.Add(-d), now); got != want {
			t.Errorf("ago(-%v) = %q, want %q", d, got, want)
		}
	}
}

func TestFreeName(t *testing.T) {
	if got := freeName("a.go", []string{"a.go", "a-2.go"}); got != "a-3.go" {
		t.Errorf("got %q", got)
	}
	if got := freeName("b.go", []string{"a.go"}); got != "b.go" {
		t.Errorf("got %q", got)
	}
}

// The list's cursor steps over headings in both directions.
func TestListSkipsHeadings(t *testing.T) {
	l := newList()
	l.set([]listItem{{label: "H", head: true}, {label: "a"}, {label: "H2", head: true}, {label: "b"}})
	if l.cur != 1 {
		t.Fatalf("set: cur %d", l.cur)
	}
	l.move(1)
	if l.cur != 3 {
		t.Errorf("down: cur %d", l.cur)
	}
	l.move(-1)
	if l.cur != 1 {
		t.Errorf("up: cur %d", l.cur)
	}
	l.move(-1)
	if l.cur != 1 {
		t.Errorf("up at the top: cur %d", l.cur)
	}
}
