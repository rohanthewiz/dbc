package web

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"

	bytdbdrv "github.com/rohanthewiz/bytdb/stdlib"
	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/userdata"
)

// consoleEnv is a test server with consoles on, in a directory of its own.
func consoleEnv(t *testing.T, tweaks ...func(*config.Config, *Options)) (*testEnv, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "consoles")
	tweaks = append([]func(*config.Config, *Options){func(_ *config.Config, o *Options) { o.ConsolesDir = dir }}, tweaks...)
	return newTestEnv(t, tweaks...), dir
}

// the test server's one connection is an in-memory SQLite database, which
// db.ConsoleTarget keeps per connection under the "memory" host
const demoConsoles = "/api/v1/consoles/memory/demo-sqlite"

// A connect says which database the tab is on, as a set of consoles: the
// page swaps the tab's console on it.
func TestConnSaysTheConsoleDatabase(t *testing.T) {
	e, _ := consoleEnv(t)
	id, s := e.open()
	e.api("POST", "/api/v1/ws/"+id+"/connect", `{"name":"demo-sqlite"}`, 200)
	ev, _ := s.await(t, "conn")
	c := decodeData[connEvent](t, testEnvelope{Data: ev.Data})
	if c.Console == nil || c.Console.Host != "memory" || c.Console.Database != "demo-sqlite" ||
		c.Console.Label != "memory · demo-sqlite" || c.Console.Names == nil {
		t.Fatalf("conn event's console = %+v", c.Console)
	}
	// and so does a reattaching page's state
	st := decodeData[wsState](t, e.api("GET", "/api/v1/ws/"+id, "", 200))
	if st.Console == nil || st.Console.Database != "demo-sqlite" {
		t.Errorf("ws state's console = %+v", st.Console)
	}
}

// With consoles off, nothing names a console and the routes say so.
func TestConsolesOff(t *testing.T) {
	e := newTestEnv(t)
	id, _ := e.connected()
	if st := decodeData[wsState](t, e.api("GET", "/api/v1/ws/"+id, "", 200)); st.Console != nil {
		t.Errorf("consoles off, yet the state names %+v", st.Console)
	}
	e.api("GET", demoConsoles+"/console", "", 404)
}

// A save names the revision it was made from; a file that has moved on
// since is not overwritten, and the answer carries its text instead.
func TestConsoleSaveRevisions(t *testing.T) {
	e, dir := consoleEnv(t)
	path := demoConsoles + "/console"
	save := func(text, base string) consoleSaved {
		b, _ := json.Marshal(consoleSave{Text: text, Base: base})
		return decodeData[consoleSaved](t, e.api("PUT", path+"?win=w1", string(b), 200))
	}

	got := decodeData[consoleText](t, e.api("GET", path, "", 200))
	if got.Text != "" || got.Rev != "" {
		t.Fatalf("a console with no file = %+v", got)
	}
	r1 := save("SELECT 1", "")
	if r1.Conflict || r1.Rev == "" {
		t.Fatalf("first save = %+v", r1)
	}
	file := userdata.ConsolePath(dir, userdata.ConsoleDB{Host: "memory", Database: "demo-sqlite"}, "console")
	if text, _ := userdata.LoadConsole(file); text != "SELECT 1" {
		t.Fatalf("the file holds %q", text)
	}

	// another writer, still on revision "": refused, told what is there
	stale := save("SELECT 'other'", "")
	if !stale.Conflict || stale.Text != "SELECT 1" || stale.Rev != r1.Rev {
		t.Fatalf("a stale save = %+v, want the conflict with SELECT 1", stale)
	}
	if text, _ := userdata.LoadConsole(file); text != "SELECT 1" {
		t.Fatalf("a stale save overwrote the file: %q", text)
	}
	// from the current revision it goes through
	r2 := save("SELECT 2", r1.Rev)
	if r2.Conflict || r2.Rev == r1.Rev {
		t.Fatalf("an up-to-date save = %+v", r2)
	}
	// the text the file already holds is no conflict, whatever the base
	if same := save("SELECT 2", "stale"); same.Conflict || same.Rev != r2.Rev {
		t.Errorf("saving the file's own text = %+v", same)
	}
	// a writer outside dbc web (the TUI) moves the revision on too
	if err := userdata.SaveConsole(file, "SELECT 'tui'"); err != nil {
		t.Fatal(err)
	}
	if after := save("SELECT 3", r2.Rev); !after.Conflict || after.Text != "SELECT 'tui'" {
		t.Errorf("a save over the TUI's write = %+v, want a conflict", after)
	}
}

// A save is told to every window, so one showing the console can load it.
func TestConsoleSaveBroadcast(t *testing.T) {
	e, _ := consoleEnv(t)
	_, s := e.open()
	b, _ := json.Marshal(consoleSave{Text: "SELECT 'shared'"})
	e.api("PUT", demoConsoles+"/console?win=elsewhere", string(b), 200)
	ev, _ := s.await(t, "console")
	var got consoleEvent
	if err := json.Unmarshal(ev.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Host != "memory" || got.Database != "demo-sqlite" || got.Name != "console" ||
		got.Text != "SELECT 'shared'" || got.Win != "elsewhere" || got.Rev == "" {
		t.Errorf("console event = %+v", got)
	}
}

// Nothing from the browser reaches the filesystem but one file in the
// consoles tree.
func TestConsolePathsChecked(t *testing.T) {
	e, _ := consoleEnv(t)
	for _, p := range []string{
		"/api/v1/consoles/.x/demo-sqlite/console",
		"/api/v1/consoles/memory/demo-sqlite/.hidden",
		"/api/v1/consoles/memory/demo-sqlite/a%2Fb",
	} {
		e.api("GET", p, "", 400)
	}
	e.api("POST", demoConsoles+"/console/rename", `{"to":"../up"}`, 400)
}

// Renames and deletes are told to every window, with the list as it is.
func TestConsoleRenameAndDelete(t *testing.T) {
	e, _ := consoleEnv(t)
	_, s := e.open()
	for _, n := range []string{"console", "console-2"} {
		b, _ := json.Marshal(consoleSave{Text: "SELECT '" + n + "'"})
		e.api("PUT", demoConsoles+"/"+n, string(b), 200)
	}
	if got := decodeData[map[string][]string](t, e.api("GET", demoConsoles, "", 200))["names"]; !slices.Equal(got, []string{"console", "console-2"}) {
		t.Fatalf("names = %v", got)
	}

	e.api("POST", demoConsoles+"/console-2/rename", `{"to":"console"}`, 409)
	e.api("POST", demoConsoles+"/console-2/rename", `{"to":"reports"}`, 200)
	ev, _ := s.await(t, "consoles")
	var ce consolesEvent
	_ = json.Unmarshal(ev.Data, &ce)
	if ce.Renamed == nil || ce.Renamed.From != "console-2" || ce.Renamed.To != "reports" ||
		!slices.Equal(ce.Names, []string{"console", "reports"}) {
		t.Fatalf("rename event = %+v", ce)
	}
	if got := decodeData[consoleText](t, e.api("GET", demoConsoles+"/reports", "", 200)); got.Text != "SELECT 'console-2'" {
		t.Errorf("renamed console holds %q", got.Text)
	}

	e.api("DELETE", demoConsoles+"/reports", "", 200)
	ev, _ = s.await(t, "consoles")
	ce = consolesEvent{}
	_ = json.Unmarshal(ev.Data, &ce)
	if ce.Deleted != "reports" || !slices.Equal(ce.Names, []string{"console"}) {
		t.Errorf("delete event = %+v", ce)
	}
}

// A tab saves the console it shows, and the saved tabs come back with their
// connection's console database, for the page to show before connecting.
func TestTabRemembersItsConsole(t *testing.T) {
	e, _ := consoleEnv(t)
	win, _ := e.openWin()
	e.api("PUT", "/api/v1/tabs/k1?win="+win, `{"title":"Q","conn":"demo-sqlite","buffer":"x","console":"console-2"}`, 200)
	tabs := decodeData[[]savedTab](t, e.api("GET", "/api/v1/tabs", "", 200))
	if len(tabs) != 1 || tabs[0].Console != "console-2" || tabs[0].ConsoleDB == nil ||
		tabs[0].ConsoleDB.Database != "demo-sqlite" {
		t.Fatalf("saved tabs = %+v", tabs)
	}
}

// At the first start with consoles, saved tabs' buffers move into consoles
// of their databases — one each, never over an existing file — and only
// once.
func TestTabBuffersMoveIntoConsoles(t *testing.T) {
	st, _ := OpenStore("")
	for _, tb := range []Tab{
		{ID: "1", Title: "A", Conn: "demo-sqlite", Buffer: "SELECT 'a'"},
		{ID: "2", Title: "B", Conn: "demo-sqlite", Buffer: "SELECT 'b'"},
		{ID: "3", Title: "C", Conn: "", Buffer: "SELECT 'no conn'"},
		{ID: "4", Title: "D", Conn: "demo-sqlite", Buffer: ""},
	} {
		if err := st.SaveTab(tb); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(t.TempDir(), "consoles")
	d := userdata.ConsoleDB{Host: "memory", Database: "demo-sqlite"}
	// a console the TUI already wrote
	if err := userdata.SaveConsole(userdata.ConsolePath(dir, d, "console"), "SELECT 'tui'"); err != nil {
		t.Fatal(err)
	}
	e, _ := consoleEnv(t, func(_ *config.Config, o *Options) { o.Store, o.ConsolesDir = st, dir })

	byID := map[string]Tab{}
	tabs, _ := st.Tabs()
	for _, tb := range tabs {
		byID[tb.ID] = tb
	}
	if byID["1"].Console != "console-2" || byID["2"].Console != "console-3" {
		t.Fatalf("consoles given: 1=%q 2=%q", byID["1"].Console, byID["2"].Console)
	}
	if byID["3"].Console != "" || byID["4"].Console != "" {
		t.Errorf("a tab with no connection, or no text, was given a console: 3=%q 4=%q", byID["3"].Console, byID["4"].Console)
	}
	for name, want := range map[string]string{"console": "SELECT 'tui'", "console-2": "SELECT 'a'", "console-3": "SELECT 'b'"} {
		if text, _ := userdata.LoadConsole(userdata.ConsolePath(dir, d, name)); text != want {
			t.Errorf("%s holds %q, want %q", name, text, want)
		}
	}
	if byID["1"].Buffer != "SELECT 'a'" {
		t.Error("the tab's own buffer was not kept")
	}

	// once: a second start finds the marker and moves nothing
	_ = st.SaveTab(Tab{ID: "5", Title: "E", Conn: "demo-sqlite", Buffer: "SELECT 'e'"})
	if warns := e.srv.moveTabBuffers(); len(warns) != 0 {
		t.Fatal(warns)
	}
	tabs, _ = st.Tabs()
	for _, tb := range tabs {
		if tb.ID == "5" && tb.Console != "" {
			t.Errorf("a second start moved tab 5 into %q", tb.Console)
		}
	}
}

// The file store keeps a tab's console beside it, and forgets it with it.
func TestStoreTabConsole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web.bytdb")
	st, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.SaveTab(Tab{ID: "k", Title: "Q", Conn: "c", Buffer: "b", Console: "reports"}); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	if st, err = OpenStore(path); err != nil { // reopened: the schema is created again, idempotently
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	tabs, err := st.Tabs()
	if err != nil || len(tabs) != 1 || tabs[0].Console != "reports" {
		t.Fatalf("tabs = %+v, %v", tabs, err)
	}
	if err = st.DeleteTab("k"); err != nil {
		t.Fatal(err)
	}
	if err = st.SaveTab(Tab{ID: "k", Title: "Q", Conn: "c"}); err != nil {
		t.Fatal(err)
	}
	if tabs, _ = st.Tabs(); tabs[0].Console != "" {
		t.Errorf("a deleted tab's console came back: %q", tabs[0].Console)
	}
}

// A store from before tabs.console — the console names in a tab_consoles
// table of their own — opens with the names moved onto their tabs and the
// old table gone; a name whose tab was deleted goes with it.
func TestStoreMigratesTabConsoles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web.bytdb")
	old, err := sql.Open(bytdbdrv.DriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE tabs (id TEXT PRIMARY KEY, title TEXT NOT NULL, conn TEXT NOT NULL,
			buffer TEXT NOT NULL, updated TIMESTAMPTZ NOT NULL)`,
		`CREATE TABLE tab_consoles (id TEXT PRIMARY KEY, console TEXT NOT NULL)`,
		`INSERT INTO tabs VALUES ('a', 'A', 'c', 'b', now()), ('b', 'B', 'c', 'b', now())`,
		`INSERT INTO tab_consoles VALUES ('a', 'reports'), ('gone', 'stale')`,
	} {
		if _, err = old.Exec(q); err != nil {
			_ = old.Close()
			t.Fatalf("%s: %v", q, err)
		}
	}
	_ = old.Close()

	st, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	tabs, err := st.Tabs()
	if err != nil || len(tabs) != 2 || tabs[0].Console != "reports" || tabs[1].Console != "" {
		t.Fatalf("tabs = %+v, %v", tabs, err)
	}
	var n int
	if err = st.db.QueryRow(`SELECT count(*) FROM information_schema.tables
		WHERE table_name = 'tab_consoles'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("tab_consoles still there: count %d, %v", n, err)
	}
}
