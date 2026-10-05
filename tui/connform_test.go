package tui

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/connedit"
	"github.com/rohanthewiz/dbc/workspace"
)

// connFormOf is the open connection form, failing the test when there is none.
func connFormOf(t *testing.T, m *Model) *connFormModal {
	t.Helper()
	cf, ok := m.modal.(*connFormModal)
	if !ok {
		t.Fatalf("no connection form open (modal %T); log:\n%s", m.modal, logText(m))
	}
	return cf
}

// menuRow is the open menu's row whose label starts with prefix.
func menuRow(t *testing.T, m *Model, prefix string) menuItem {
	t.Helper()
	for _, it := range m.menu.items {
		if strings.HasPrefix(it.label, prefix) {
			return it
		}
	}
	t.Fatalf("no menu row %q", prefix)
	return menuItem{}
}

// connRowAt is the screen position of the connections list's row for name.
func connRowAt(t *testing.T, m *Model, name string) (int, int) {
	t.Helper()
	for i, it := range m.conns.items {
		if it.data.(string) == name {
			return m.conns.view.X + 2, m.conns.view.Y + i - m.conns.top
		}
	}
	t.Fatalf("no connection row %q", name)
	return 0, 0
}

// The whole round: add a SQLite connection from the form (test, save,
// connect), edit it — refused while on it, ai_rows alone allowed, renamed
// once off it, its schema pick following — and remove it. The saved file
// is a temp one, never the real ~/.config/dbc.
func TestConnFormAddEditRemove(t *testing.T) {
	m := newTestModel(t)
	dir := t.TempDir()
	m.saved = config.OpenSaved(filepath.Join(dir, "connections.toml"))
	dbFile := filepath.Join(dir, "scratch.db")
	demo := m.ws.Active()

	key(t, m, "ctrl+l")
	key(t, m, "a")
	cf := connFormOf(t, m)
	if !strings.Contains(frame(m).Text(), "Add a connection") {
		t.Fatalf("form not drawn:\n%s", frame(m).Text())
	}
	typeText(t, m, "scratch")

	// pick sqlite by its chip: the host fields give way to a file
	r := cf.drvR[2] // postgres, mysql, sqlite, bytdb
	click(t, m, r.X+1, r.Y)
	if cf.driver != "sqlite" || cf.server() {
		t.Fatalf("driver = %q", cf.driver)
	}
	key(t, m, "tab") // Enter as
	key(t, m, "tab") // File
	if cf.focus != cfFile {
		t.Fatalf("focus = %v, want the file field", cf.focus)
	}
	typeText(t, m, dbFile)

	// a test of a file not there yet is a qualified yes
	key(t, m, "ctrl+t")
	if !strings.Contains(cf.msg, "does not exist yet") {
		t.Fatalf("test said %q", cf.msg)
	}

	key(t, m, "enter") // save
	if m.modal != nil {
		t.Fatalf("form still open: %q", connFormOf(t, m).msg)
	}
	cn, ok := m.cfg.ConnByName("scratch")
	if !ok || !cn.Web || cn.Driver != "sqlite" {
		t.Fatalf("config entry = %+v, %v", cn, ok)
	}
	if sc, found, _ := m.saved.Get("scratch"); !found || !strings.Contains(sc.DSN, "scratch.db") {
		t.Fatalf("saved entry = %+v, %v", sc, found)
	}
	if m.ws.Active() != "scratch" {
		t.Fatalf("active = %q, want the new connection; log:\n%s", m.ws.Active(), logText(m))
	}
	if !strings.Contains(frame(m).Text(), "✎ sqlite") {
		t.Fatalf("no ✎ mark on the added connection:\n%s", frame(m).Text())
	}

	// editing the connection dbc is on: a rename is refused …
	key(t, m, "ctrl+l")
	m.conns.cur = 1
	key(t, m, "e")
	cf = connFormOf(t, m)
	if cf.from != "scratch" || cf.fields[cfFile].Text() != dbFile || cf.asDSN {
		t.Fatalf("edit form = from %q, file %q, asDSN %v", cf.from, cf.fields[cfFile].Text(), cf.asDSN)
	}
	typeText(t, m, "2")
	key(t, m, "enter")
	if !strings.Contains(cf.msg, `dbc is on "scratch"`) {
		t.Fatalf("rename while on it said %q", cf.msg)
	}
	// … but the assistant's row access alone may change
	key(t, m, "backspace")
	cf.focus = cfAIRows
	key(t, m, "space")
	key(t, m, "enter")
	if m.modal != nil {
		t.Fatalf("ai_rows edit refused: %q", cf.msg)
	}
	if cn, _ := m.cfg.ConnByName("scratch"); !cn.AIRows {
		t.Fatal("ai_rows not changed")
	}

	// off it, a rename goes through and takes the schema pick along
	m.schemaPicks["scratch"] = workspace.SchemaPick{Name: "main"}
	drive(t, m, nil, m.setActive(demo))
	key(t, m, "ctrl+l")
	m.conns.cur = 1
	key(t, m, "e")
	typeText(t, m, "2")
	key(t, m, "enter")
	if m.modal != nil {
		t.Fatalf("rename refused: %q", connFormOf(t, m).msg)
	}
	if _, ok := m.cfg.ConnByName("scratch2"); !ok {
		t.Fatalf("not renamed in the config; log:\n%s", logText(m))
	}
	if _, found, _ := m.saved.Get("scratch2"); !found {
		t.Fatal("not renamed in the file")
	}
	if p, ok := m.schemaPicks["scratch2"]; !ok || p.Name != "main" {
		t.Fatalf("schema pick not moved: %+v", m.schemaPicks)
	}

	// the config file's connection: edit and remove say why not
	x, y := connRowAt(t, m, demo)
	rightClick(t, m, x, y)
	if why := menuRow(t, m, "Edit ").why; !strings.Contains(why, "edit the file to change it") {
		t.Fatalf("edit of a file connection: why %q", why)
	}
	m.menu = nil

	// remove, confirmed
	x, y = connRowAt(t, m, "scratch2")
	rightClick(t, m, x, y)
	pickMenu(t, m, "Remove scratch2")
	pickMenu(t, m, "✕ Remove")
	if _, ok := m.cfg.ConnByName("scratch2"); ok {
		t.Fatal("still in the config")
	}
	if _, found, _ := m.saved.Get("scratch2"); found {
		t.Fatal("still in the file")
	}
	for _, it := range m.conns.items {
		if it.data.(string) == "scratch2" {
			t.Fatal("still listed")
		}
	}
}

// A connection dbc is on cannot be removed: the menu row says why.
func TestConnFormRemoveRefusedWhileOn(t *testing.T) {
	m := newTestModel(t)
	dir := t.TempDir()
	m.saved = config.OpenSaved(filepath.Join(dir, "connections.toml"))
	if _, err := m.connEditor().Add(connFormFor("mine", filepath.Join(dir, "m.db"))); err != nil {
		t.Fatal(err)
	}
	m.refreshConns()
	drive(t, m, nil, m.setActive("mine"))
	x, y := connRowAt(t, m, "mine")
	rightClick(t, m, x, y)
	if why := menuRow(t, m, "Remove mine").why; !strings.Contains(why, `dbc is on "mine"`) {
		t.Fatalf("remove while on it: why %q", why)
	}
}

// Fields in DSN mode, and a form that could never work, answer in the form.
func TestConnFormRefusalStaysInForm(t *testing.T) {
	m := newTestModel(t)
	m.saved = config.OpenSaved(filepath.Join(t.TempDir(), "connections.toml"))
	key(t, m, "ctrl+l")
	typeText(t, m, "+")
	cf := connFormOf(t, m)
	key(t, m, "enter") // nothing filled in: the fields are built first, as in dbc web
	if !strings.Contains(cf.msg, "host") || m.modal == nil {
		t.Fatalf("msg = %q", cf.msg)
	}
	// Enter as DSN: the field and its example show
	typeText(t, m, "x")
	cf.focus = cfMode
	key(t, m, "right")
	if !cf.asDSN || !strings.Contains(frame(m).Text(), "postgres://user:${PGPASS}") {
		t.Fatalf("DSN mode not drawn:\n%s", frame(m).Text())
	}
	key(t, m, "enter")
	if cf.msg != "the DSN is empty" {
		t.Fatalf("msg = %q", cf.msg)
	}
	key(t, m, "esc")
	if m.modal != nil {
		t.Fatal("Esc did not close the form")
	}
}

func connFormFor(name, file string) connedit.Form {
	return connedit.Form{Name: name, Driver: "sqlite", DSN: file}
}

// An edit fills the fields from the stored DSN but never its password; an
// untouched form keeps the stored DSN whole, a touched one keeps the
// password, and the password field draws as dots.
func TestConnFormEditKeepsWhatItWasNotGiven(t *testing.T) {
	cf := newConnForm()
	cf.fill(config.SavedConn{Name: "pg", Driver: "postgres",
		DSN: "postgres://u:s3cret@db.example.com:5432/app?sslmode=disable"})
	if cf.asDSN || cf.fields[cfHost].Text() != "db.example.com" || cf.fields[cfPassword].Text() != "" || !cf.hasPassword {
		t.Fatalf("filled: asDSN %v host %q password %q has %v", cf.asDSN,
			cf.fields[cfHost].Text(), cf.fields[cfPassword].Text(), cf.hasPassword)
	}
	if f := cf.form(); f.Parts != nil || f.DSN != "" {
		t.Fatalf("untouched form sent %+v, want an empty DSN (keep it)", f)
	}
	cf.set(cfHost, "db2.example.com")
	f := cf.form()
	if f.Parts == nil || f.Parts.Host != "db2.example.com" || !f.KeepPassword {
		t.Fatalf("touched form sent %+v", f)
	}

	m := newTestModel(t)
	cf.set(cfPassword, "hunter2")
	m.openModal(cf)
	if txt := frame(m).Text(); strings.Contains(txt, "hunter2") || !strings.Contains(txt, "•••••••") {
		t.Fatalf("password not masked:\n%s", txt)
	}
}
