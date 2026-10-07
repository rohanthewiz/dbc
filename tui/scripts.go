package tui

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/script"
	"github.com/rohanthewiz/dbc/scripts"
	"github.com/rohanthewiz/dbc/userdata"
)

// The scripts browser (Ctrl+O, ƒ Scripts) and editing a script in $EDITOR.
// It is dbc web's browser (web/static/js/scripts.js) drawn with the list
// widget, over the same store (userdata/scripts.go), examples and templates
// (package scripts) and check (script.Check):
//
//	╭ Scripts · ~/.config/dbc/scripts ────────────────────────────── ✕ ╮
//	│ ⌕ / filters by name and description                     + New    │
//	│                                                                   │
//	│  Scripts                                                          │
//	│  copy_mytable.go   Copy myschema.mytable from ProdDr to dev   2h  │
//	│  Examples · read-only — Enter makes your own copy                 │
//	│  copy_table.go     Copy a Postgres table …               example  │
//	│  Trash (2) ▸                                                      │
//	│ Enter run · e edit · n new · d duplicate · r rename · Del trash … │
//	╰───────────────────────────────────────────────────────────────────╯
//
// KEYS ARE BARE LETTERS, NOT THE WEB'S CHORDS. The web's filter keeps the
// focus, so its commands had to be chords (⇧Enter, Ctrl+Delete), and a
// terminal cannot be trusted to deliver those: ⇧Enter is plain Enter
// outside the kitty keyboard protocol, and Ctrl+Delete varies. So here the
// LIST has the keyboard — Enter run, e edit, n new, d duplicate, r / F2
// rename, Del / x trash, y copy path, t the trash — and the filter is a
// mode, entered with / (as in less and vim) and left with Esc or Tab:
//
//	list ──/ or a click on the field──► filter ──Esc / Tab──► list
//	  │                                   │  typing narrows; ↑↓ still move
//	  └─Enter: the row's action           └─Enter: the row's action
//
// EACH ROW KIND HAS ONE THING ENTER DOES, as on the web: run a script, copy
// an example into your scripts, start a script from a template (offered
// where the scripts would be when there are none), restore a trashed one.
//
// EDITING SUSPENDS THE TUI for $VISUAL, else $EDITOR, else vi
// (tea.ExecProcess hands the terminal over and takes it back). On return
// the script is checked (script.Check: parse, Run's signature, the yaegi
// lint, a compile — never a run), the findings go to the log as
// compiler-style "path:line:col: msg" lines, and the browser reopens on the
// script, so Enter runs what was just written. Every way a script is made
// (new, duplicate, copying an example) writes the file and then edits it,
// as the web opens a new script in a tab.

// scriptRowKind is what a row of the browser is.
type scriptRowKind int

const (
	rowScript    scriptRowKind = iota + 1 // a script in the scripts dir
	rowTemplate                           // a template, offered while there are no scripts
	rowExample                            // a built-in example (read-only)
	rowTrash                              // a trashed script
	rowTrashHead                          // the Trash heading: a click folds it
)

// scriptRow is a row's listItem.data: what picking it acts on.
type scriptRow struct {
	kind  scriptRowKind
	name  string // a script's or example's file name; a trashed script's old name
	tpl   scripts.Template
	trash userdata.TrashInfo
}

// scriptsModal is the browser. It holds what it read from disk when it
// opened (or last refreshed) and narrows that by the filter; every action
// that changes the directory re-reads it.
type scriptsModal struct {
	modalBase
	dir   string // the scripts dir, absolute (config resolves it)
	short string // the same, ~-shortened, for the title

	mine      []userdata.ScriptInfo
	examples  []scripts.Example
	templates []scripts.Template
	trash     []userdata.TrashInfo
	showTrash bool // the Trash section is unfolded

	filter    *editor
	filtering bool // the keyboard is in the filter, not on the list
	lst       *list

	filterRect, newBtn Rect
	hoverNew           bool
	now                func() time.Time // for "2h" ages; a var for tests
}

// openScripts opens the browser, with the cursor on the script called sel
// when there is one (after an edit, a rename, a restore).
func (m *Model) openScripts(sel string) {
	md := &scriptsModal{dir: m.cfg.ScriptsDir, short: config.TildePath(m.cfg.ScriptsDir),
		filter: newEditor(true), lst: newList(), now: time.Now}
	md.filter.placeholder = "/ filters by name and description"
	if err := md.load(); err != nil {
		m.logf(logErr, "scripts: %s", serr.StringFromErr(err))
		return
	}
	md.build()
	md.selectName(sel)
	m.openModal(md)
}

// load reads the directory, the trash and the built-ins.
func (sm *scriptsModal) load() error {
	mine, err := userdata.ListScripts(sm.dir)
	if err != nil {
		return err
	}
	sm.mine = mine
	sm.trash = userdata.ListTrash(sm.dir)
	sm.examples = scripts.Examples()
	sm.templates = scripts.Templates()
	return nil
}

// reload re-reads the directory after a change made from the browser,
// keeping the filter and the trash fold, with the cursor on sel if given
// (else as near where it was as the new rows allow).
func (sm *scriptsModal) reload(m *Model, sel string) {
	if err := sm.load(); err != nil {
		m.logf(logErr, "scripts: %s", serr.StringFromErr(err))
	}
	cur := sm.lst.cur
	sm.build()
	sm.lst.cur = cur
	sm.lst.set(sm.lst.items) // clamps, and steps off a heading
	sm.selectName(sel)
}

// selectName puts the cursor on the script row called name, if listed.
func (sm *scriptsModal) selectName(name string) {
	if name == "" {
		return
	}
	for i, it := range sm.lst.items {
		if r, ok := it.data.(scriptRow); ok && r.kind == rowScript && r.name == name {
			sm.lst.cur = i
			sm.lst.ensureVisible()
			return
		}
	}
}

// build lays out the rows for the filter: sections, each with a heading,
// and a note where a section has nothing to show.
func (sm *scriptsModal) build() {
	q := strings.ToLower(strings.TrimSpace(sm.filter.Text()))
	hit := func(name, desc string) bool {
		return q == "" || strings.Contains(strings.ToLower(name), q) || strings.Contains(strings.ToLower(desc), q)
	}
	now := sm.now()
	var items []listItem
	head := func(label string, data any) { items = append(items, listItem{label: label, head: true, data: data}) }

	head("Scripts", nil)
	shown := 0
	for _, s := range sm.mine {
		if hit(s.Name, s.Desc) {
			items = append(items, listItem{label: s.Name, desc: s.Desc, sub: ago(s.Mod, now),
				data: scriptRow{kind: rowScript, name: s.Name}})
			shown++
		}
	}
	switch {
	case len(sm.mine) == 0:
		// an empty directory offers the templates where the scripts would
		// be, rather than a warning in the log
		head("  none yet in "+sm.short+" — start one from a template:", nil)
		for _, t := range sm.templates {
			if hit(t.Title, t.Desc) {
				items = append(items, listItem{label: t.Title, desc: t.Desc, sub: "template",
					data: scriptRow{kind: rowTemplate, name: t.Name, tpl: t}})
			}
		}
	case shown == 0:
		head("  no script matches", nil)
	}

	var ex []listItem
	for _, x := range sm.examples {
		if hit(x.Name, x.Desc) {
			ex = append(ex, listItem{label: x.Name, desc: x.Desc, sub: "example",
				data: scriptRow{kind: rowExample, name: x.Name}})
		}
	}
	if len(ex) > 0 {
		head("Examples · read-only — Enter makes your own copy", nil)
		items = append(items, ex...)
	}

	if len(sm.trash) > 0 {
		fold := "▸ t shows it"
		if sm.showTrash {
			fold = "▾"
		}
		head(fmt.Sprintf("Trash (%d) %s", len(sm.trash), fold), scriptRow{kind: rowTrashHead})
		if sm.showTrash {
			for _, t := range sm.trash {
				if hit(t.Name, "") {
					items = append(items, listItem{label: t.Name, desc: "trashed " + agoWords(t.Trashed, now),
						sub: "Enter restores", muted: true, data: scriptRow{kind: rowTrash, name: t.Name, trash: t}})
				}
			}
		}
	}

	// descriptions start in one column: past the widest name, within reason
	w := 0
	for _, it := range items {
		if !it.head {
			w = max(w, width(it.label))
		}
	}
	sm.lst.descCol = min(w, 32) + 2
	sm.lst.set(items)
}

// row is the row under the cursor, if it is a pickable one.
func (sm *scriptsModal) row() (scriptRow, bool) {
	it, ok := sm.lst.current()
	if !ok {
		return scriptRow{}, false
	}
	r, ok := it.data.(scriptRow)
	return r, ok
}

func (sm *scriptsModal) title() string { return "Scripts · " + sm.short }

// size is most of the screen: descriptions want the width, and the list
// grows with the scripts.
func (sm *scriptsModal) size(w, h int) (int, int) {
	return max(min(w-4, 110), min(72, w-4)), max(12, min(len(sm.lst.items)+5, h*3/4))
}

// footer is the key hint for the mode the keyboard is in.
func (sm *scriptsModal) footer() string {
	if sm.filtering {
		return "typing filters · ↑↓ move · Enter: the row's action · Esc/Tab: back to the list"
	}
	return "Enter run · e edit · n new · d duplicate · r rename · Del trash · y path · t trash · / filter · Esc close"
}

func (sm *scriptsModal) draw(m *Model, s Surface) *caret {
	bg := m.st.panel
	s.Put(1, 0, "⌕", onBg(m.st.accent, bg))
	label := " + New "
	sm.newBtn = chip(s, s.W()-1-width(label), 0, label, pick(sm.hoverNew, m.st.buttonHover, m.st.button))
	field := s.Sub(Rect{3, 0, max(s.W()-width(label)-6, 10), 1})
	sm.filterRect = field.Rect()
	fieldBg := bg
	if sm.filtering {
		fieldBg = m.st.raised
	}
	cx, cy, ok := sm.filter.Draw(field, m.st, fieldBg, [2]int{}, true)
	// the cursor row stays lit while filtering: it is what Enter acts on
	sm.lst.draw(s.Sub(Rect{0, 2, s.W(), s.H() - 3}), m.st, bg, true, "")
	s.Put(1, s.H()-1, truncate(sm.footer(), s.W()-2), onBg(m.st.muted, bg))
	if ok && sm.filtering {
		return &caret{cx, cy}
	}
	return nil
}

func (sm *scriptsModal) key(m *Model, k tea.KeyPressMsg) (tea.Cmd, bool) {
	if sm.filtering {
		return sm.filterKey(m, k)
	}
	switch k.String() {
	case "esc":
		return nil, true
	case "/":
		sm.filtering = true
		return nil, false
	case "enter":
		return sm.act(m, false)
	case "e":
		return sm.act(m, true)
	case "n", "alt+n":
		sm.newMenu(m, sm.newBtn.X, sm.newBtn.Y+1)
		return nil, false
	case "d":
		return sm.duplicate(m)
	case "r", "f2":
		return sm.rename(m)
	case "delete", "x":
		sm.trashIt(m)
		return nil, false
	case "y":
		return sm.copyPath(m), false
	case "t":
		sm.toggleTrash(m)
		return nil, false
	}
	sm.lst.key(k)
	return nil, false
}

// filterKey is the keyboard while it is in the filter.
func (sm *scriptsModal) filterKey(m *Model, k tea.KeyPressMsg) (tea.Cmd, bool) {
	switch k.String() {
	case "esc", "tab", "shift+tab":
		sm.filtering = false
		return nil, false
	case "enter":
		sm.filtering = false
		return sm.act(m, false)
	case "up", "down", "pgup", "pgdown", "ctrl+p", "ctrl+n":
		sm.lst.key(k)
		return nil, false
	}
	before := sm.filter.Text()
	sm.filter.HandleKey(k)
	if sm.filter.Text() != before {
		sm.lst.cur = 0 // the best match is the first row again
		sm.build()
	}
	return nil, false
}

func (sm *scriptsModal) paste(m *Model, s string) {
	sm.filtering = true
	sm.filter.Insert(s)
	sm.lst.cur = 0
	sm.build()
}

func (sm *scriptsModal) click(m *Model, x, y, clicks int, shift bool) (tea.Cmd, bool) {
	switch {
	case m.modalClose().Contains(x, y):
		return nil, true
	case sm.newBtn.Contains(x, y):
		sm.newMenu(m, sm.newBtn.X, sm.newBtn.Y+1)
		return nil, false
	case sm.filterRect.Contains(x, y):
		sm.filtering = true
		sm.filter.Click(x, y, clicks, shift)
		return nil, false
	}
	i := sm.lst.indexAt(x, y)
	if i < 0 {
		return nil, false
	}
	it := sm.lst.items[i]
	if it.head {
		if r, ok := it.data.(scriptRow); ok && r.kind == rowTrashHead {
			sm.toggleTrash(m)
		}
		return nil, false
	}
	sm.lst.cur, sm.filtering = i, false
	if clicks >= 2 {
		// a double-click opens a script for editing, as on the web; on the
		// other rows it does their one thing
		return sm.act(m, true)
	}
	return nil, false
}

// rightClick opens the menu of what can be done with the row clicked.
func (sm *scriptsModal) rightClick(m *Model, x, y int) tea.Cmd {
	i := sm.lst.indexAt(x, y)
	if i < 0 || sm.lst.items[i].head {
		return nil
	}
	sm.lst.cur, sm.filtering = i, false
	r, _ := sm.row()
	var items []menuItem
	switch r.kind {
	case rowScript:
		items = []menuItem{
			heading(r.name),
			{label: "▶ Run", key: "Enter", act: func(m *Model) tea.Cmd { return sm.closeThen(m, m.runScript(filepath.Join(sm.dir, r.name))) }},
			{label: "✎ Edit in " + editorName(), key: "e", act: func(m *Model) tea.Cmd { return sm.closeThen(m, m.editScript(sm.dir, r.name)) }},
			{label: "Duplicate…", key: "d", act: func(m *Model) tea.Cmd { cmd, _ := sm.duplicate(m); return cmd }},
			{label: "Rename…", key: "r", act: func(m *Model) tea.Cmd { cmd, _ := sm.rename(m); return cmd }},
			{label: "Move to the trash", key: "Del", act: func(m *Model) tea.Cmd { sm.trashIt(m); return nil }},
			{label: "Copy path", key: "y", act: func(m *Model) tea.Cmd { return sm.copyPath(m) }},
		}
	case rowExample:
		items = []menuItem{heading(r.name),
			{label: "⧉ Copy into my scripts…", key: "Enter", act: func(m *Model) tea.Cmd { cmd, _ := sm.act(m, false); return cmd }}}
	case rowTemplate:
		items = []menuItem{heading(r.tpl.Title),
			{label: "+ New script from it…", key: "Enter", act: func(m *Model) tea.Cmd { cmd, _ := sm.act(m, false); return cmd }}}
	case rowTrash:
		items = []menuItem{heading(r.name),
			{label: "↺ Restore", key: "Enter", act: func(m *Model) tea.Cmd { cmd, _ := sm.act(m, false); return cmd }}}
	default:
		return nil
	}
	m.openMenu(x, y, items)
	return nil
}

// closeThen closes the browser (when it is still the open modal) and
// returns cmd: for a menu action that leaves the browser, since a menu's
// action, unlike a key, has no "closed" to report.
func (sm *scriptsModal) closeThen(m *Model, cmd tea.Cmd) tea.Cmd {
	if m.modal == sm {
		m.modal = nil
	}
	return cmd
}

func (sm *scriptsModal) hover(m *Model, x, y int) {
	sm.lst.hover = sm.lst.indexAt(x, y)
	if sm.lst.hover >= 0 && sm.lst.items[sm.lst.hover].head {
		sm.lst.hover = -1
	}
	sm.hoverNew = sm.newBtn.Contains(x, y)
}
func (sm *scriptsModal) wheel(m *Model, x, y, dy int) { sm.lst.scroll(dy) }

// toggleTrash folds or unfolds the Trash section.
func (sm *scriptsModal) toggleTrash(m *Model) {
	if len(sm.trash) == 0 {
		m.log(logMuted, "the trash is empty")
		return
	}
	sm.showTrash = !sm.showTrash
	sm.build()
}

// act does what the row under the cursor is for. edit asks for a script to
// be opened in the editor rather than run; on the other kinds of row both
// do the row's one thing.
func (sm *scriptsModal) act(m *Model, edit bool) (tea.Cmd, bool) {
	r, ok := sm.row()
	if !ok {
		return nil, false
	}
	switch r.kind {
	case rowScript:
		if edit {
			return m.editScript(sm.dir, r.name), true
		}
		return m.runScript(filepath.Join(sm.dir, r.name)), true
	case rowExample:
		ex, found := scripts.ExampleByName(r.name)
		if !found {
			return nil, false
		}
		m.makeScript(sm.dir, r.name, ex.Text, "Copy the example "+r.name)
		return nil, true
	case rowTemplate:
		m.newFromTemplate(sm.dir, r.tpl)
		return nil, true
	case rowTrash:
		m.restoreScript(sm.dir, r.trash)
		return nil, true
	}
	return nil, false
}

// newMenu offers the templates, as the web's "+ New ▾" does.
func (sm *scriptsModal) newMenu(m *Model, x, y int) {
	items := []menuItem{heading("new script from")}
	for _, t := range sm.templates {
		items = append(items, menuItem{label: t.Title, act: func(m *Model) tea.Cmd {
			m.newFromTemplate(sm.dir, t)
			return nil
		}})
	}
	m.openMenu(x, y, items)
}

// scriptOnly is the script row under the cursor, or a log line saying the
// action is for scripts.
func (sm *scriptsModal) scriptOnly(m *Model, verb string) (string, bool) {
	r, ok := sm.row()
	if !ok {
		return "", false
	}
	if r.kind == rowExample && verb == "duplicate" {
		return r.name, true
	}
	if r.kind != rowScript {
		m.logf(logWarn, "only a script of yours can be %s — this row is %s", verbPast(verb), rowKindWords(r.kind))
		return "", false
	}
	return r.name, true
}

func verbPast(verb string) string {
	switch verb {
	case "duplicate":
		return "duplicated"
	case "rename":
		return "renamed"
	case "trash":
		return "trashed"
	case "copy":
		return "copied"
	}
	return verb + "ed"
}

func rowKindWords(k scriptRowKind) string {
	switch k {
	case rowExample:
		return "a built-in example (Enter makes your own copy)"
	case rowTemplate:
		return "a template (Enter starts a script from it)"
	case rowTrash:
		return "in the trash (Enter restores it)"
	}
	return "not a script"
}

// duplicate copies the script (or example) under the cursor under a new
// name, then edits the copy.
func (sm *scriptsModal) duplicate(m *Model) (tea.Cmd, bool) {
	name, ok := sm.scriptOnly(m, "duplicate")
	if !ok {
		return nil, false
	}
	if r, _ := sm.row(); r.kind == rowExample {
		return sm.act(m, false)
	}
	text, _, err := userdata.ReadScript(sm.dir, name)
	if err != nil {
		m.logf(logErr, "could not read %s: %s", name, serr.StringFromErr(err))
		return nil, false
	}
	m.makeScript(sm.dir, name, text, "Duplicate "+name)
	return nil, false // the prompt has replaced the browser
}

// rename asks for the script's new name. Cancelling returns to the browser.
func (sm *scriptsModal) rename(m *Model) (tea.Cmd, bool) {
	name, ok := sm.scriptOnly(m, "rename")
	if !ok {
		return nil, false
	}
	p := m.openPromptCmd("Rename "+name, scriptNameHint, "Rename", name, func(m *Model, typed string) (tea.Cmd, error) {
		to := goName(typed)
		if to == name {
			m.openScripts(name)
			return nil, nil
		}
		if err := userdata.RenameScript(sm.dir, name, to); err != nil {
			return nil, scriptNameErr(err, to)
		}
		m.logf(logOk, "renamed %s to %s", name, to)
		m.openScripts(to)
		return nil, nil
	})
	selectStem(p.field)
	p.cancel = func(m *Model) { m.openScripts(name) }
	return nil, false
}

// trashIt moves the script under the cursor to the trash. Nothing is
// asked: the trash is listed in the browser and Enter there restores.
func (sm *scriptsModal) trashIt(m *Model) {
	name, ok := sm.scriptOnly(m, "trash")
	if !ok {
		return
	}
	if _, err := userdata.TrashScript(sm.dir, name); err != nil {
		m.logf(logErr, "could not trash %s: %s", name, serr.StringFromErr(err))
		return
	}
	m.logf(logInfo, "moved %s to the trash — Ctrl+O, t shows the Trash, Enter there restores it", name)
	sm.reload(m, "")
}

// copyPath copies the script's absolute path.
func (sm *scriptsModal) copyPath(m *Model) tea.Cmd {
	name, ok := sm.scriptOnly(m, "copy")
	if !ok {
		return nil
	}
	return m.copyString(filepath.Join(sm.dir, name), "the script's path")
}

// ---------------------------------------------------------------------------
// Making, restoring and editing scripts
// ---------------------------------------------------------------------------

const scriptNameHint = "letters, digits, '.', '-' and '_', ending in .go"

// goName makes what was typed a script name: trimmed, with ".go" added when
// left off. userdata.ValidScriptName has the last word.
func goName(s string) string {
	s = strings.TrimSpace(s)
	if s != "" && !strings.HasSuffix(strings.ToLower(s), ".go") {
		s += ".go"
	}
	return s
}

// freeName is base, or base-2.go, base-3.go … — the first not in taken
// (the web's freeName).
func freeName(base string, taken []string) string {
	stem := strings.TrimSuffix(base, ".go")
	n := stem + ".go"
	for i := 2; slices.Contains(taken, n); i++ {
		n = fmt.Sprintf("%s-%d.go", stem, i)
	}
	return n
}

// selectStem selects a "name.go" field's stem, so typing replaces the name
// and keeps the extension (the web's prompt does the same).
func selectStem(ed *editor) {
	if t := ed.Text(); strings.HasSuffix(t, ".go") && !strings.Contains(t, "\n") {
		ed.anc, ed.cur, ed.sel = pos{0, 0}, pos{0, len([]rune(t)) - 3}, true
	}
}

// scriptNameErr is a store error as the prompt shows it, under the field:
// short, and saying what to do.
func scriptNameErr(err error, name string) error {
	switch {
	case errors.Is(err, userdata.ErrScriptExists):
		return fmt.Errorf("%s exists — pick another name", name)
	case errors.Is(err, userdata.ErrBadScriptName) || !userdata.ValidScriptName(name):
		return errors.New("not a script name: " + scriptNameHint)
	}
	return errors.New(serr.StringFromErr(err))
}

// newFromTemplate starts a script from template t, filled with the active
// connection (then the default, then the rest: config.ConnOrder, as the web
// fills them).
func (m *Model) newFromTemplate(dir string, t scripts.Template) {
	text, ok := scripts.Fill(t.Name, m.cfg.ConnOrder(m.ws.Active()))
	if !ok {
		m.logf(logErr, "no template %s", t.Name)
		return
	}
	suggest := t.Name + ".go"
	if t.Name == "blank" {
		suggest = "script.go"
	}
	m.makeScript(dir, suggest, text, "New script · "+t.Title)
}

// makeScript asks for a name (offered: suggest, made free), writes text as
// that new script, then edits it. A name taken meanwhile, or not a script
// name, is said under the field and asked again; cancelling goes back to
// the browser.
func (m *Model) makeScript(dir, suggest, text, title string) {
	var taken []string
	if infos, err := userdata.ListScripts(dir); err == nil {
		for _, in := range infos {
			taken = append(taken, in.Name)
		}
	}
	p := m.openPromptCmd(title, scriptNameHint, "Create", freeName(suggest, taken), func(m *Model, typed string) (tea.Cmd, error) {
		name := goName(typed)
		if !userdata.ValidScriptName(name) {
			return nil, scriptNameErr(userdata.ErrBadScriptName, name)
		}
		if _, _, err := userdata.SaveScript(dir, name, text, ""); err != nil {
			return nil, scriptNameErr(err, name)
		}
		m.logf(logOk, "created %s in %s", name, config.TildePath(dir))
		return m.editScript(dir, name), nil
	})
	selectStem(p.field)
	p.cancel = func(m *Model) { m.openScripts("") }
}

// restoreScript puts a trashed script back under its old name, or asks for
// another when that name has been taken since.
func (m *Model) restoreScript(dir string, t userdata.TrashInfo) {
	name, err := userdata.RestoreScript(dir, t.ID, "")
	if err == nil {
		m.logf(logOk, "restored %s", name)
		m.openScripts(name)
		return
	}
	if !errors.Is(err, userdata.ErrScriptExists) {
		m.logf(logErr, "could not restore %s: %s", t.Name, serr.StringFromErr(err))
		return
	}
	var taken []string
	if infos, lerr := userdata.ListScripts(dir); lerr == nil {
		for _, in := range infos {
			taken = append(taken, in.Name)
		}
	}
	p := m.openPromptCmd("Restore "+t.Name+" as…", "a script named "+t.Name+" exists — restore this one under another name",
		"Restore", freeName(t.Name, taken), func(m *Model, typed string) (tea.Cmd, error) {
			to := goName(typed)
			got, err := userdata.RestoreScript(dir, t.ID, to)
			if err != nil {
				return nil, scriptNameErr(err, to)
			}
			m.logf(logOk, "restored %s as %s", t.Name, got)
			m.openScripts(got)
			return nil, nil
		})
	selectStem(p.field)
	p.cancel = func(m *Model) { m.openScripts("") }
}

// execEditor is tea.ExecProcess, as a var so tests edit the file in place of
// a real editor (the harness has no terminal to hand over).
var execEditor = tea.ExecProcess

// scriptEditedMsg is the editor's exit, back in Update.
type scriptEditedMsg struct {
	dir, name string
	before    string // the script's revision when the editor started
	err       error  // the editor's own failure (it could not start, or exited non-zero)
}

// editorLine is the user's editor command: $VISUAL, else $EDITOR, else vi.
// It may carry arguments ("code -w", "subl -w"), split on spaces; a path
// with spaces in it needs a wrapper script.
func editorLine() []string {
	for _, v := range []string{"VISUAL", "EDITOR"} {
		if f := strings.Fields(os.Getenv(v)); len(f) > 0 {
			return f
		}
	}
	return []string{"vi"}
}

// editorName is the editor's name for a menu label ("vi", "nvim", "code").
func editorName() string { return filepath.Base(editorLine()[0]) }

// editScript suspends the TUI and opens the script in the user's editor.
// On return scriptEdited checks it. An editor that is not installed is
// said in the log without suspending anything.
func (m *Model) editScript(dir, name string) tea.Cmd {
	path := filepath.Join(dir, name)
	_, before, err := userdata.ReadScript(dir, name)
	if err != nil {
		m.logf(logErr, "could not open %s: %s", name, serr.StringFromErr(err))
		return nil
	}
	line := editorLine()
	bin, err := exec.LookPath(line[0])
	if err != nil {
		m.logf(logErr, "no editor %q on PATH — set $VISUAL or $EDITOR (e.g. EDITOR=nvim, or \"code -w\")", line[0])
		return nil
	}
	m.logf(logMuted, "editing %s in %s — dbc comes back when it exits", config.TildePath(path), filepath.Base(line[0]))
	cmd := exec.Command(bin, append(line[1:], path)...)
	return execEditor(cmd, func(err error) tea.Msg {
		return scriptEditedMsg{dir: dir, name: name, before: before, err: err}
	})
}

// scriptDiagMax caps the check's lines in the log: yaegi stops at its
// first compile error anyway, so more than this is lint or a parse cascade.
const scriptDiagMax = 20

// scriptEdited checks the script the editor left behind, logs what the
// check found as "path:line:col: msg" (the form a terminal's editor can
// jump to), and reopens the browser on it.
func (m *Model) scriptEdited(msg scriptEditedMsg) tea.Cmd {
	path := filepath.Join(msg.dir, msg.name)
	if msg.err != nil {
		m.logf(logWarn, "the editor exited with %v", msg.err)
	}
	text, rev, err := userdata.ReadScript(msg.dir, msg.name)
	if err != nil {
		m.logf(logWarn, "%s is gone: %s", msg.name, serr.StringFromErr(err))
		m.openScripts("")
		return nil
	}
	diags := script.Check(msg.name, text)
	changed := "saved"
	if rev == msg.before {
		changed = "unchanged"
	}
	switch {
	case len(diags) == 0:
		m.logf(logOk, "%s %s — checked, no problems (Enter runs it)", msg.name, changed)
	default:
		errs := 0
		for _, d := range diags {
			if d.Severity == script.SevError {
				errs++
			}
		}
		kind := logWarn
		if errs > 0 {
			kind = logErr
		}
		m.logf(kind, "%s %s — the check found %d error(s), %d warning(s):", msg.name, changed, errs, len(diags)-errs)
		shown := config.TildePath(path)
		for i, d := range diags {
			if i == scriptDiagMax {
				m.logf(logMuted, "… and %d more (dbc script --check %s lists them all)", len(diags)-i, msg.name)
				break
			}
			k := logWarn
			if d.Severity == script.SevError {
				k = logErr
			}
			m.logf(k, "%s:%s", shown, d.String())
		}
	}
	m.openScripts(msg.name)
	return nil
}

// ago is a short age for a list's right column: now, 5m, 3h, 2d, then a
// date.
func ago(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	case t.Year() == now.Year():
		return t.Format("Jan 2")
	}
	return t.Format("Jan 2 2006")
}

// agoWords is ago in a sentence: "just now", "3h ago", "on Jan 2".
func agoWords(t, now time.Time) string {
	a := ago(t, now)
	switch {
	case a == "now":
		return "just now"
	case now.Sub(t) < 30*24*time.Hour:
		return a + " ago"
	}
	return "on " + a
}
