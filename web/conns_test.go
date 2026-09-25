package web

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rohanthewiz/dbc/config"
)

// connListResp is GET /api/v1/conns and the add/remove responses.
type connListResp struct {
	Conns    []connInfo `json:"conns"`
	Default  string     `json:"default"`
	Warnings []string   `json:"warnings"`
}

// probeResp is POST /api/v1/conns/test.
type probeResp struct {
	OK       bool     `json:"ok"`
	Error    string   `json:"error"`
	Note     string   `json:"note"`
	MS       int64    `json:"ms"`
	Warnings []string `json:"warnings"`
}

func connBody(name, driver, dsn string) string {
	b, _ := json.Marshal(connForm{Name: name, Driver: driver, DSN: dsn})
	return string(b)
}

func (l connListResp) find(name string) (connInfo, bool) {
	for _, c := range l.Conns {
		if c.Name == name {
			return c, true
		}
	}
	return connInfo{}, false
}

func TestConnTest(t *testing.T) {
	e := newTestEnv(t, func(c *config.Config, _ *Options) { c.ConnectTimeout = 2 * time.Second })

	// an in-memory SQLite database: opens, pings, reports the time
	r := decodeData[probeResp](t, e.api("POST", "/api/v1/conns/test",
		connBody("", "sqlite", "file:probe1?mode=memory&cache=shared"), 200))
	if !r.OK || r.Error != "" || r.Note != "" {
		t.Fatalf("memory sqlite: %+v", r)
	}

	// a file that does not exist: reported, and NOT created by the test
	missing := filepath.Join(t.TempDir(), "nope.db")
	r = decodeData[probeResp](t, e.api("POST", "/api/v1/conns/test", connBody("x", "sqlite", "file:"+missing), 200))
	if !r.OK || !strings.Contains(r.Note, "created on first connect") {
		t.Fatalf("missing sqlite file: %+v", r)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("the test created %s (stat err %v)", missing, err)
	}

	// a server that refuses: the test worked, the connect did not — a 200
	// with ok false and the driver's words, which must not carry the
	// password typed into the DSN
	r = decodeData[probeResp](t, e.api("POST", "/api/v1/conns/test",
		connBody("pg", "postgres", "postgres://u:hunter2@127.0.0.1:1/db?sslmode=disable&connect_timeout=2"), 200))
	if r.OK || r.Error == "" {
		t.Fatalf("refused postgres: %+v", r)
	}
	if strings.Contains(r.Error, "hunter2") {
		t.Fatalf("the password leaked into the error: %q", r.Error)
	}

	// an unset ${VAR} is a warning with the answer, not a silent ""
	r = decodeData[probeResp](t, e.api("POST", "/api/v1/conns/test",
		connBody("", "sqlite", "file:probe2?mode=memory&cache=shared${DBC_TEST_SURELY_UNSET}"), 200))
	if !r.OK || len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "DBC_TEST_SURELY_UNSET") {
		t.Fatalf("unset var: %+v", r)
	}

	// forms that could never work are 400s
	for _, body := range []string{
		connBody("", "oracle", "x"),
		connBody("", "sqlite", "   "),
		connBody("a\tb", "sqlite", "file:x?mode=memory"),
	} {
		e.api("POST", "/api/v1/conns/test", body, 400)
	}
}

func TestConnAddConnectRemove(t *testing.T) {
	e := newTestEnv(t)
	id, s := e.connected()

	t.Setenv("DBC_TEST_MEMNAME", "addtest")
	env := e.api("POST", "/api/v1/conns", connBody(" scratch ", "sqlite",
		"file:${DBC_TEST_MEMNAME}?mode=memory&cache=shared"), 200)
	if strings.Contains(string(env.Data), "dsn") || strings.Contains(string(env.Data), "addtest") {
		t.Fatalf("the add response carries the DSN: %s", env.Data)
	}
	got := decodeData[connListResp](t, env)
	if c, ok := got.find("scratch"); !ok || !c.Saved || c.Driver != "sqlite" {
		t.Fatalf("after add: %+v", got)
	}
	if c, _ := got.find("demo-sqlite"); c.Saved {
		t.Fatal("the config's connection is marked saved")
	}
	// every window hears of it, so every sidebar redraws
	ev, _ := s.await(t, "conns")
	if ev.WS != "" || !strings.Contains(string(ev.Data), `"scratch"`) {
		t.Fatalf("conns event = %+v", ev)
	}
	// the running config has it, the DSN expanded; the store has it as typed
	cc, ok := e.srv.cfg.ConnByName("scratch")
	if !ok || !cc.Web || cc.DSN != "file:addtest?mode=memory&cache=shared" {
		t.Fatalf("config entry = %+v", cc)
	}
	saved, _ := e.srv.store.Conns()
	if len(saved) != 1 || saved[0].DSN != "file:${DBC_TEST_MEMNAME}?mode=memory&cache=shared" {
		t.Fatalf("store = %+v", saved)
	}

	// a name in use — the config's or a saved one — is a 409
	e.api("POST", "/api/v1/conns", connBody("demo-sqlite", "sqlite", "file:x?mode=memory"), 409)
	e.api("POST", "/api/v1/conns", connBody("scratch", "sqlite", "file:x?mode=memory"), 409)
	e.api("POST", "/api/v1/conns", connBody("", "sqlite", "file:x?mode=memory"), 400)

	// a tab can use it like any other
	e.api("POST", "/api/v1/ws/"+id+"/connect", `{"name":"scratch"}`, 200)
	cev, _ := s.await(t, "conn")
	c := decodeData[connEvent](t, testEnvelope{Data: cev.Data})
	if c.Active != "scratch" || c.Failed {
		t.Fatalf("connect to the added one: %+v", c)
	}

	// ...and while one is on it, it stays
	res := e.api("DELETE", "/api/v1/conns/scratch", "", 409)
	if !strings.Contains(res.Error, "1 query tab is") {
		t.Fatalf("refusal = %q", res.Error)
	}
	e.api("POST", "/api/v1/ws/"+id+"/connect", `{"name":"demo-sqlite"}`, 200)
	s.await(t, "conn")

	got = decodeData[connListResp](t, e.api("DELETE", "/api/v1/conns/scratch", "", 200))
	if _, ok := got.find("scratch"); ok {
		t.Fatalf("still listed after remove: %+v", got)
	}
	if _, ok := e.srv.cfg.ConnByName("scratch"); ok {
		t.Fatal("still in the config after remove")
	}
	if saved, _ = e.srv.store.Conns(); len(saved) != 0 {
		t.Fatalf("still in the store after remove: %+v", saved)
	}

	// the file's connections, and names that are not there, are not removable
	res = e.api("DELETE", "/api/v1/conns/demo-sqlite", "", 400)
	if !strings.Contains(res.Error, "config file") {
		t.Fatalf("config conn refusal = %q", res.Error)
	}
	e.api("DELETE", "/api/v1/conns/scratch", "", 404)
}

// A name with a space and a slash survives the trip through the DELETE path.
func TestConnRemoveEscapedName(t *testing.T) {
	e := newTestEnv(t)
	e.api("POST", "/api/v1/conns", connBody("my db/2", "sqlite", "file:esc?mode=memory&cache=shared"), 200)
	e.api("DELETE", "/api/v1/conns/my%20db%2F2", "", 200)
	if _, ok := e.srv.cfg.ConnByName("my db/2"); ok {
		t.Fatal("not removed")
	}
}

func TestSavedConnsMergeAtStartup(t *testing.T) {
	st, err := OpenStore(filepath.Join(t.TempDir(), "web.bytdb"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	t.Setenv("DBC_TEST_MERGE", "merged")
	now := time.Now().UTC()
	for i, sc := range []SavedConn{
		{Name: "kept", Driver: "sqlite", DSN: "file:${DBC_TEST_MERGE}?mode=memory&cache=shared", AIRows: true},
		{Name: "demo-sqlite", Driver: "sqlite", DSN: "file:clash?mode=memory"}, // the file's name now
		{Name: "odd", Driver: "oracle", DSN: "x"},
	} {
		sc.Added = now.Add(time.Duration(i) * time.Second)
		if err = st.SaveConn(sc); err != nil {
			t.Fatal(err)
		}
	}

	e := newTestEnv(t, func(_ *config.Config, o *Options) { o.Store = st })

	cc, ok := e.srv.cfg.ConnByName("kept")
	if !ok || !cc.Web || !cc.AIRows || cc.DSN != "file:merged?mode=memory&cache=shared" {
		t.Fatalf("kept = %+v (found %v)", cc, ok)
	}
	if cc, _ = e.srv.cfg.ConnByName("demo-sqlite"); cc.Web {
		t.Fatal("a saved connection displaced the config file's")
	}
	w := strings.Join(e.srv.cfg.Warnings, "\n")
	for _, want := range []string{`"demo-sqlite" skipped`, `"odd" skipped`} {
		if !strings.Contains(w, want) {
			t.Fatalf("warnings %q lack %q", w, want)
		}
	}
	// the skipped ones stay in the store: the user may yet rename the file's
	if saved, _ := st.Conns(); len(saved) != 3 {
		t.Fatalf("store after merge = %+v", saved)
	}
	// and the sidebar order is the file's, then the saved ones in the order added
	l := decodeData[connListResp](t, e.api("GET", "/api/v1/conns", "", 200))
	names := fmt.Sprint(func() (n []string) {
		for _, c := range l.Conns {
			n = append(n, c.Name)
		}
		return
	}())
	if names != "[demo-sqlite kept]" {
		t.Fatalf("list order = %s", names)
	}
}

// editBody is a PUT /api/v1/conns/:name body.
func editBody(name, driver, dsn string, aiRows bool) string {
	b, _ := json.Marshal(connForm{Name: name, Driver: driver, DSN: dsn, AIRows: aiRows})
	return string(b)
}

// An edit, on both kinds of store: the memory-only one keeps its maps, the
// file one runs UpdateConn's SQL (delete, insert, retag tabs) on bytdb.
func TestConnEdit(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		t.Run(fmt.Sprintf("persistent=%v", persistent), func(t *testing.T) {
			testConnEdit(t, persistent)
		})
	}
}

func testConnEdit(t *testing.T, persistent bool) {
	var tweaks []func(*config.Config, *Options)
	if persistent {
		st, err := OpenStore(filepath.Join(t.TempDir(), "web.bytdb"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		tweaks = append(tweaks, func(_ *config.Config, o *Options) { o.Store = st })
	}
	e := newTestEnv(t, tweaks...)
	id, s := e.connected()

	t.Setenv("DBC_TEST_EDITNAME", "edit1")
	typed := "file:${DBC_TEST_EDITNAME}?mode=memory&cache=shared"
	e.api("POST", "/api/v1/conns", connBody("scratch", "sqlite", typed), 200)
	e.api("POST", "/api/v1/conns", connBody("later", "sqlite", "file:later?mode=memory&cache=shared"), 200)
	s.await(t, "conns")
	s.await(t, "conns")
	before, _, _ := e.srv.store.Conn("scratch")
	// a saved query tab not shown in any window, noted as on "scratch"
	if err := e.srv.store.SaveTab(Tab{ID: "bg", Title: "Q", Conn: "scratch"}); err != nil {
		t.Fatal(err)
	}

	// a test from the edit form with the DSN left empty tests the stored one
	r := decodeData[probeResp](t, e.api("POST", "/api/v1/conns/test",
		`{"driver":"sqlite","dsn":"","from":"scratch"}`, 200))
	if !r.OK {
		t.Fatalf("test with the kept DSN: %+v", r)
	}
	// ...but not under another driver, whose DSN it cannot be
	res := e.api("POST", "/api/v1/conns/test", `{"driver":"postgres","dsn":"","from":"scratch"}`, 400)
	if !strings.Contains(res.Error, "type a postgres DSN") {
		t.Fatalf("kept DSN, new driver: %q", res.Error)
	}

	// rename and let the assistant see rows, keeping the DSN
	env := e.api("PUT", "/api/v1/conns/scratch", editBody("renamed", "sqlite", "", true), 200)
	if strings.Contains(string(env.Data), "edit1") || strings.Contains(string(env.Data), "DBC_TEST") {
		t.Fatalf("the edit response carries the DSN: %s", env.Data)
	}
	var got struct {
		connListResp
		Renamed map[string]string `json:"renamed"`
	}
	if err := json.Unmarshal(env.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Renamed["from"] != "scratch" || got.Renamed["to"] != "renamed" {
		t.Fatalf("renamed = %v", got.Renamed)
	}
	if c, ok := got.find("renamed"); !ok || !c.Saved || !c.AIRows {
		t.Fatalf("after edit: %+v", got.Conns)
	}
	if _, ok := got.find("scratch"); ok {
		t.Fatal("the old name is still listed")
	}
	// it keeps its place: after the file's, before the one added later
	if names := fmt.Sprint(got.Conns); !strings.Contains(names, "{demo-sqlite") ||
		strings.Index(names, "renamed") > strings.Index(names, "later") {
		t.Fatalf("order after edit: %s", names)
	}
	// every window hears of the rename
	ev, _ := s.await(t, "conns")
	if !strings.Contains(string(ev.Data), `"renamed":{"from":"scratch","to":"renamed"}`) {
		t.Fatalf("conns event = %s", ev.Data)
	}
	// the config has the new entry with the same expanded DSN; the store
	// has the DSN as typed, the original Added, and the saved tab moved
	cc, ok := e.srv.cfg.ConnByName("renamed")
	if !ok || !cc.Web || !cc.AIRows || cc.DSN != "file:edit1?mode=memory&cache=shared" {
		t.Fatalf("config entry = %+v", cc)
	}
	after, found, _ := e.srv.store.Conn("renamed")
	if !found || after.DSN != typed || !after.Added.Equal(before.Added) || !after.AIRows {
		t.Fatalf("store entry = %+v (before %+v)", after, before)
	}
	if _, found, _ = e.srv.store.Conn("scratch"); found {
		t.Fatal("the old name is still in the store")
	}
	tabs, _ := e.srv.store.Tabs()
	for _, tb := range tabs {
		if tb.ID == "bg" && tb.Conn != "renamed" {
			t.Fatalf("saved tab still on %q", tb.Conn)
		}
	}

	// refusals: a name in use, a new driver with the DSN kept, the file's
	// connection, one that is not there
	e.api("PUT", "/api/v1/conns/renamed", editBody("later", "sqlite", "", true), 409)
	e.api("PUT", "/api/v1/conns/renamed", editBody("demo-sqlite", "sqlite", "", true), 409)
	e.api("PUT", "/api/v1/conns/renamed", editBody("renamed", "mysql", "", true), 400)
	res = e.api("PUT", "/api/v1/conns/demo-sqlite", editBody("x", "sqlite", "", false), 400)
	if !strings.Contains(res.Error, "edit the file to change it") {
		t.Fatalf("config conn refusal = %q", res.Error)
	}
	e.api("PUT", "/api/v1/conns/scratch", editBody("x", "sqlite", "", false), 404)

	// while a tab is on it, a new DSN or name is refused, ai_rows is not
	e.api("POST", "/api/v1/ws/"+id+"/connect", `{"name":"renamed"}`, 200)
	s.await(t, "conn")
	res = e.api("PUT", "/api/v1/conns/renamed",
		editBody("renamed", "sqlite", "file:other?mode=memory&cache=shared", true), 409)
	if !strings.Contains(res.Error, "1 query tab is") {
		t.Fatalf("in-use refusal = %q", res.Error)
	}
	e.api("PUT", "/api/v1/conns/renamed", editBody("again", "sqlite", "", true), 409)
	e.api("PUT", "/api/v1/conns/renamed", editBody("renamed", "sqlite", "", false), 200)
	if cc, _ = e.srv.cfg.ConnByName("renamed"); cc.AIRows {
		t.Fatal("ai_rows did not change while in use")
	}
	// retyping the same DSN is no change either
	e.api("PUT", "/api/v1/conns/renamed", editBody("renamed", "sqlite", typed, false), 200)

	// off it, a new DSN goes through and the next connect uses it
	e.api("POST", "/api/v1/ws/"+id+"/connect", `{"name":"demo-sqlite"}`, 200)
	s.await(t, "conn")
	e.api("PUT", "/api/v1/conns/renamed",
		editBody("renamed", "sqlite", "file:edit2?mode=memory&cache=shared", false), 200)
	if cc, _ = e.srv.cfg.ConnByName("renamed"); cc.DSN != "file:edit2?mode=memory&cache=shared" {
		t.Fatalf("new DSN not in the config: %+v", cc)
	}
	if sc, _, _ := e.srv.store.Conn("renamed"); sc.DSN != "file:edit2?mode=memory&cache=shared" {
		t.Fatalf("new DSN not in the store: %+v", sc)
	}
	e.api("POST", "/api/v1/ws/"+id+"/connect", `{"name":"renamed"}`, 200)
	cev, _ := s.await(t, "conn")
	if c := decodeData[connEvent](t, testEnvelope{Data: cev.Data}); c.Active != "renamed" || c.Failed {
		t.Fatalf("connect after edit: %+v", c)
	}
}
