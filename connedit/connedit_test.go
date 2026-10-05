package connedit

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
)

// newEditor is an Editor over a config holding one file connection and a
// saved-connections file in a temp dir (never the real ~/.config/dbc).
func newEditor(t *testing.T) (*Editor, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{Connections: []config.Connection{
		{Name: "filed", Driver: "sqlite", DSN: filepath.Join(dir, "filed.db")},
	}}
	return &Editor{Cfg: cfg, Saved: config.OpenSaved(filepath.Join(dir, "connections.toml")), App: "dbc"}, dir
}

func kindOf(err error) Kind {
	var ce *Error
	if errors.As(err, &ce) {
		return ce.Kind
	}
	return 0
}

func TestCheckRefusesWhatCannotWork(t *testing.T) {
	cases := []struct {
		f    Form
		need bool
		want string
	}{
		{Form{Driver: "sqlite", DSN: "x.db"}, true, "name the connection"},
		{Form{Name: strings.Repeat("n", MaxName+1), Driver: "sqlite", DSN: "x.db"}, true, "at most"},
		{Form{Name: "a\tb", Driver: "sqlite", DSN: "x.db"}, true, "control characters"},
		{Form{Name: "a", Driver: "oracle", DSN: "x"}, true, "pick a driver"},
		{Form{Name: "a", Driver: "sqlite", DSN: "  "}, true, "DSN is empty"},
		{Form{Name: "a", Driver: "sqlite", DSN: "x.db", TLSOpts: config.TLSOpts{TLS: "require"}}, true, "TLS settings are for postgres and mysql"},
		{Form{Name: "a", Driver: "postgres", DSN: "postgres://h/d", TLSOpts: config.TLSOpts{TLS: "bogus"}}, true, "unknown tls mode"},
	}
	for _, c := range cases {
		err := c.f.Check(c.need)
		if err == nil || !strings.Contains(err.Error(), c.want) || kindOf(err) != Invalid {
			t.Errorf("Check(%+v) = %v, want Invalid %q", c.f, err, c.want)
		}
	}
	// a test may run before a name is typed
	f := Form{Driver: "sqlite", DSN: " x.db "}
	if err := f.Check(false); err != nil || f.DSN != "x.db" {
		t.Fatalf("Check(false) = %v, DSN %q", err, f.DSN)
	}
}

func TestAddKeepsItInConfigAndFile(t *testing.T) {
	e, dir := newEditor(t)
	path := filepath.Join(dir, "new.db")
	warns, err := e.Add(Form{Name: " scratch ", Driver: "sqlite", Parts: &db.DSNParts{File: path}, AIRows: true})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if len(warns) != 0 {
		t.Errorf("warnings: %v", warns)
	}
	cn, ok := e.Cfg.ConnByName("scratch")
	if !ok || !cn.Web || !cn.AIRows || cn.Driver != "sqlite" || !strings.Contains(cn.DSN, "new.db") {
		t.Fatalf("config entry = %+v, %v", cn, ok)
	}
	sc, found, err := e.Saved.Get("scratch")
	if err != nil || !found || !strings.Contains(sc.DSN, "new.db") {
		t.Fatalf("saved entry = %+v, %v, %v", sc, found, err)
	}

	// the same name again is a clash the config catches
	_, err = e.Add(Form{Name: "scratch", Driver: "sqlite", DSN: path})
	if kindOf(err) != Conflict || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second Add = %v, want Conflict", err)
	}
}

// A name the file has but this config does not — another dbc added it since
// this one read the file — is refused, and the config change undone.
func TestAddUndoesConfigWhenFileClashes(t *testing.T) {
	e, dir := newEditor(t)
	if err := e.Saved.Add(config.SavedConn{Name: "other", Driver: "sqlite", DSN: "o.db"}); err != nil {
		t.Fatal(err)
	}
	_, err := e.Add(Form{Name: "other", Driver: "sqlite", DSN: filepath.Join(dir, "o.db")})
	if kindOf(err) != Conflict || !strings.Contains(err.Error(), "added by another dbc") {
		t.Fatalf("Add = %v", err)
	}
	if _, ok := e.Cfg.ConnByName("other"); ok {
		t.Fatal("the config kept a connection the file refused")
	}
}

func TestEditRules(t *testing.T) {
	e, dir := newEditor(t)
	path := filepath.Join(dir, "a.db")
	if _, err := e.Add(Form{Name: "a", Driver: "sqlite", DSN: path}); err != nil {
		t.Fatal(err)
	}

	// the file's connections are the file's
	if _, err := e.Edit("filed", Form{Name: "filed", Driver: "sqlite", DSN: "x.db"}, nil); kindOf(err) != Invalid ||
		!strings.Contains(err.Error(), "edit the file to change it") {
		t.Fatalf("edit of a file connection = %v", err)
	}
	if _, err := e.Edit("nope", Form{Name: "nope", Driver: "sqlite", DSN: "x"}, nil); kindOf(err) != NotFound {
		t.Fatalf("edit of a missing connection = %v", err)
	}

	busy := errors.New("in use")
	asked := 0
	inUse := func(string) error { asked++; return busy }

	// ai_rows alone goes through while in use, and keeps the DSN left empty
	res, err := e.Edit("a", Form{Name: "a", Driver: "sqlite", AIRows: true}, inUse)
	if err != nil || res.Reconnects || res.Renamed || asked != 0 {
		t.Fatalf("ai_rows edit = %+v, %v (asked %d)", res, err, asked)
	}
	if cn, _ := e.Cfg.ConnByName("a"); !cn.AIRows || cn.DSN != path {
		t.Fatalf("after ai_rows edit: %+v", cn)
	}

	// a rename reconnects, so it asks — and the refusal stands
	if _, err = e.Edit("a", Form{Name: "b", Driver: "sqlite"}, inUse); !errors.Is(err, busy) || asked != 1 {
		t.Fatalf("rename while in use = %v (asked %d)", err, asked)
	}

	// a kept DSN goes only with its own driver
	if _, err = e.Edit("a", Form{Name: "a", Driver: "postgres"}, nil); kindOf(err) != Invalid ||
		!strings.Contains(err.Error(), "type a postgres DSN") {
		t.Fatalf("driver change with a kept DSN = %v", err)
	}

	// a rename when free moves the entry in place, in both
	res, err = e.Edit("a", Form{Name: "b", Driver: "sqlite"}, nil)
	if err != nil || !res.Renamed || !res.Reconnects {
		t.Fatalf("rename = %+v, %v", res, err)
	}
	if _, ok := e.Cfg.ConnByName("a"); ok {
		t.Fatal("old name still in the config")
	}
	if sc, found, _ := e.Saved.Get("b"); !found || sc.DSN != path {
		t.Fatalf("saved after rename: %+v %v", sc, found)
	}
	// renaming onto a taken name is a clash
	if _, err = e.Edit("b", Form{Name: "filed", Driver: "sqlite"}, nil); kindOf(err) != Conflict {
		t.Fatalf("rename onto a taken name = %v", err)
	}
}

// KeepPassword in fields takes the password from the stored DSN.
func TestEditKeepsPasswordFromFields(t *testing.T) {
	e, _ := newEditor(t)
	if _, err := e.Add(Form{Name: "pg", Driver: "postgres", Parts: &db.DSNParts{
		Host: "h", Port: "5432", User: "u", Password: "s3cret", Database: "d"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Edit("pg", Form{Name: "pg", Driver: "postgres", KeepPassword: true, Parts: &db.DSNParts{
		Host: "h2", Port: "5432", User: "u", Database: "d"}}, nil); err != nil {
		t.Fatal(err)
	}
	sc, _, _ := e.Saved.Get("pg")
	p, err := db.SplitDSN(sc.Driver, sc.DSN)
	if err != nil || p.Password != "s3cret" || p.Host != "h2" {
		t.Fatalf("stored after edit: %q → %+v, %v", sc.DSN, p, err)
	}
}

func TestDeleteRules(t *testing.T) {
	e, dir := newEditor(t)
	if _, err := e.Add(Form{Name: "a", Driver: "sqlite", DSN: filepath.Join(dir, "a.db")}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Delete("filed", nil); kindOf(err) != Invalid || !strings.Contains(err.Error(), "remove it") {
		t.Fatalf("delete of a file connection = %v", err)
	}
	busy := errors.New("in use")
	if removed, err := e.Delete("a", func(string) error { return busy }); removed || !errors.Is(err, busy) {
		t.Fatalf("delete while in use = %v, %v", removed, err)
	}
	if removed, err := e.Delete("a", nil); !removed || err != nil {
		t.Fatalf("delete = %v, %v", removed, err)
	}
	if _, ok := e.Cfg.ConnByName("a"); ok {
		t.Fatal("still in the config")
	}
	if _, found, _ := e.Saved.Get("a"); found {
		t.Fatal("still in the file")
	}
}

func TestTest(t *testing.T) {
	e, dir := newEditor(t)
	// a file that is not there yet is a qualified yes, and nothing is made
	missing := filepath.Join(dir, "later.db")
	res, err := e.Test(context.Background(), Form{Driver: "sqlite", DSN: missing}, "")
	if err != nil || !res.OK || !strings.Contains(res.Note, "does not exist yet") {
		t.Fatalf("Test(missing file) = %+v, %v", res, err)
	}
	// an existing one connects
	res, err = e.Test(context.Background(), Form{Driver: "sqlite", DSN: filepath.Join(dir, "filed.db")}, "")
	if err != nil || res.ProbeErr != nil {
		t.Fatalf("Test(file) = %+v, %v", res, err)
	}
	// a form that could never work is the call's error, not the result's
	if _, err = e.Test(context.Background(), Form{Driver: "nope", DSN: "x"}, ""); kindOf(err) != Invalid {
		t.Fatalf("Test(bad driver) = %v", err)
	}
}

func TestUnsavedWarning(t *testing.T) {
	e := &Editor{Cfg: &config.Config{}, Saved: config.OpenSaved(""), App: "dbc"}
	if w := e.UnsavedWarning("connection"); !strings.Contains(w, "until dbc stops") {
		t.Fatalf("warning = %q", w)
	}
	e.Saved = config.OpenSaved(filepath.Join(t.TempDir(), "c.toml"))
	if w := e.UnsavedWarning("connection"); w != "" {
		t.Fatalf("persistent store warned %q", w)
	}
}
