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
	"github.com/rohanthewiz/dbc/db"
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
	// the running config has it, the DSN expanded; the saved list as typed
	cc, ok := e.srv.cfg.ConnByName("scratch")
	if !ok || !cc.Web || cc.DSN != "file:addtest?mode=memory&cache=shared" {
		t.Fatalf("config entry = %+v", cc)
	}
	saved, _ := e.srv.saved.List()
	if len(saved) != 1 || saved[0].DSN != "file:${DBC_TEST_MEMNAME}?mode=memory&cache=shared" {
		t.Fatalf("saved = %+v", saved)
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
	if saved, _ = e.srv.saved.List(); len(saved) != 0 {
		t.Fatalf("still saved after remove: %+v", saved)
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

// Connections an older dbc web kept in web.bytdb move to the saved-
// connections file at startup, and the moved ones join the running config.
func TestStoreConnsMoveAtStartup(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenStore(filepath.Join(dir, "web.bytdb"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	saved := config.OpenSaved(filepath.Join(dir, "connections.toml"))
	// moved by an earlier start that stopped before deleting the row: the
	// file's entry stands, and the row is only dropped
	if err = saved.Add(config.SavedConn{Name: "twice", Driver: "sqlite", DSN: "file:filever?mode=memory"}); err != nil {
		t.Fatal(err)
	}

	t.Setenv("DBC_TEST_MERGE", "merged")
	now := time.Now().UTC().Truncate(time.Second)
	for i, sc := range []SavedConn{
		{Name: "kept", Driver: "sqlite", DSN: "file:${DBC_TEST_MERGE}?mode=memory&cache=shared", AIRows: true},
		{Name: "demo-sqlite", Driver: "sqlite", DSN: "file:clash?mode=memory"}, // the config file's name now
		{Name: "odd", Driver: "oracle", DSN: "x"},
		{Name: "twice", Driver: "sqlite", DSN: "file:rowver?mode=memory"},
	} {
		sc.Added = now.Add(time.Duration(i) * time.Second)
		if err = st.SaveConn(sc); err != nil {
			t.Fatal(err)
		}
	}

	e := newTestEnv(t, func(_ *config.Config, o *Options) { o.Store, o.Conns = st, saved })

	cc, ok := e.srv.cfg.ConnByName("kept")
	if !ok || !cc.Web || !cc.AIRows || cc.DSN != "file:merged?mode=memory&cache=shared" {
		t.Fatalf("kept = %+v (found %v)", cc, ok)
	}
	if cc, _ = e.srv.cfg.ConnByName("demo-sqlite"); cc.Web {
		t.Fatal("a moved connection displaced the config file's")
	}
	w := strings.Join(e.srv.cfg.Warnings, "\n")
	for _, want := range []string{`"demo-sqlite" skipped`, `"odd" skipped`} {
		if !strings.Contains(w, want) {
			t.Fatalf("warnings %q lack %q", w, want)
		}
	}
	// every row is in the file — the skipped ones too, for the user to
	// rename or fix — in the order added, with the file's "twice" kept as
	// it was and the DSNs as typed
	file, err := saved.List()
	if err != nil {
		t.Fatal(err)
	}
	got := fmt.Sprint(func() (n []string) {
		for _, c := range file {
			n = append(n, c.Name)
		}
		return
	}())
	if got != "[twice kept demo-sqlite odd]" {
		t.Fatalf("file after the move = %s", got)
	}
	if file[0].DSN != "file:filever?mode=memory" || file[1].DSN != "file:${DBC_TEST_MERGE}?mode=memory&cache=shared" ||
		!file[1].Added.Equal(now) || !file[1].AIRows {
		t.Fatalf("file entries = %+v", file)
	}
	// ...and none is left in the store, so the next start has nothing to move
	if rows, _ := st.Conns(); len(rows) != 0 {
		t.Fatalf("store after the move = %+v", rows)
	}
	// the sidebar order is the config file's, then the moved ones
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

// A connection another dbc web added to the file since this one started is
// refused by the file, and the running config is left as it was.
func TestConnAddClashInFile(t *testing.T) {
	saved := config.OpenSaved(filepath.Join(t.TempDir(), "connections.toml"))
	e := newTestEnv(t, func(_ *config.Config, o *Options) { o.Conns = saved })
	if err := saved.Add(config.SavedConn{Name: "other", Driver: "sqlite", DSN: "file:o?mode=memory"}); err != nil {
		t.Fatal(err)
	}
	res := e.api("POST", "/api/v1/conns", connBody("other", "sqlite", "file:x?mode=memory"), 409)
	if !strings.Contains(res.Error, "another dbc web") {
		t.Fatalf("clash in the file = %q", res.Error)
	}
	if _, ok := e.srv.cfg.ConnByName("other"); ok {
		t.Fatal("the refused connection stayed in the config")
	}
}

// editBody is a PUT /api/v1/conns/:name body.
func editBody(name, driver, dsn string, aiRows bool) string {
	b, _ := json.Marshal(connForm{Name: name, Driver: driver, DSN: dsn, AIRows: aiRows})
	return string(b)
}

// An edit, on both kinds of store: the memory-only ones keep their maps and
// lists; the persistent ones write connections.toml and run RetagTabs' SQL
// on bytdb.
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
		saved := config.OpenSaved(filepath.Join(t.TempDir(), "connections.toml"))
		tweaks = append(tweaks, func(_ *config.Config, o *Options) { o.Store, o.Conns = st, saved })
	}
	e := newTestEnv(t, tweaks...)
	id, s := e.connected()

	t.Setenv("DBC_TEST_EDITNAME", "edit1")
	typed := "file:${DBC_TEST_EDITNAME}?mode=memory&cache=shared"
	e.api("POST", "/api/v1/conns", connBody("scratch", "sqlite", typed), 200)
	e.api("POST", "/api/v1/conns", connBody("later", "sqlite", "file:later?mode=memory&cache=shared"), 200)
	s.await(t, "conns")
	s.await(t, "conns")
	before, _, _ := e.srv.saved.Get("scratch")
	// a saved query tab not shown in any window, noted as on "scratch"
	if err := e.srv.store.SaveTab(Tab{ID: "bg", Title: "Q", Conn: "scratch"}); err != nil {
		t.Fatal(err)
	}
	// schema picks saved by some window, maybe closed since: the
	// connection's own, one of its other databases', and another's
	if err := e.srv.store.SetLayout(map[string]string{
		"tableSchema.scratch":           "=main",
		"tableSchema.scratch/analytics": "=mart",
		"tableSchema.later":             "",
	}); err != nil {
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
	// the config has the new entry with the same expanded DSN; the saved
	// list has the DSN as typed and the original Added; the store has the
	// saved tab moved
	cc, ok := e.srv.cfg.ConnByName("renamed")
	if !ok || !cc.Web || !cc.AIRows || cc.DSN != "file:edit1?mode=memory&cache=shared" {
		t.Fatalf("config entry = %+v", cc)
	}
	after, found, _ := e.srv.saved.Get("renamed")
	if !found || after.DSN != typed || !after.Added.Equal(before.Added) || !after.AIRows {
		t.Fatalf("store entry = %+v (before %+v)", after, before)
	}
	if _, found, _ = e.srv.saved.Get("scratch"); found {
		t.Fatal("the old name is still saved")
	}
	tabs, _ := e.srv.store.Tabs()
	for _, tb := range tabs {
		if tb.ID == "bg" && tb.Conn != "renamed" {
			t.Fatalf("saved tab still on %q", tb.Conn)
		}
	}
	// ...and the schema picks, deleted under the old names (not blanked:
	// "" is the "every schema" pick)
	lay, _ := e.srv.store.Layout()
	for k, want := range map[string]string{
		"tableSchema.renamed": "=main", "tableSchema.renamed/analytics": "=mart", "tableSchema.later": "",
	} {
		if v, ok := lay[k]; !ok || v != want {
			t.Errorf("layout %s = %q, %v; want %q", k, v, ok, want)
		}
	}
	for _, k := range []string{"tableSchema.scratch", "tableSchema.scratch/analytics"} {
		if v, ok := lay[k]; ok {
			t.Errorf("layout %s still saved (%q)", k, v)
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
	if sc, _, _ := e.srv.saved.Get("renamed"); sc.DSN != "file:edit2?mode=memory&cache=shared" {
		t.Fatalf("new DSN not saved: %+v", sc)
	}
	e.api("POST", "/api/v1/ws/"+id+"/connect", `{"name":"renamed"}`, 200)
	cev, _ := s.await(t, "conn")
	if c := decodeData[connEvent](t, testEnvelope{Data: cev.Data}); c.Active != "renamed" || c.Failed {
		t.Fatalf("connect after edit: %+v", c)
	}
}

// partsResp is GET /api/v1/conns/:name/parts.
type partsResp struct {
	Parts       *db.DSNParts   `json:"parts"`
	HasPassword bool           `json:"has_password"`
	Reason      string         `json:"reason"`
	TLS         config.TLSOpts `json:"tls"`
}

// TestConnFieldsAndTLS: a connection entered as fields is stored as the DSN
// they make, with its TLS settings as typed; the edit form gets the fields
// back without the password, and keeps the password when told to.
func TestConnFieldsAndTLS(t *testing.T) {
	e := newTestEnv(t)
	e.connected()

	// an embedded engine, from fields: the file and its options
	e.api("POST", "/api/v1/conns", `{"name":"mem","driver":"sqlite",
		"parts":{"file":"fieldsdb","options":"mode=memory cache=shared"}}`, 200)
	if cc, _ := e.srv.cfg.ConnByName("mem"); cc.DSN != "file:fieldsdb?mode=memory&cache=shared" {
		t.Fatalf("sqlite from fields: %q", cc.DSN)
	}

	// a server engine with TLS: a relative tls_ca resolves against the
	// saved file's directory, as it will at the next start
	env := e.api("POST", "/api/v1/conns", `{"name":"pg","driver":"postgres",
		"parts":{"host":"db.internal","port":"5432","user":"app","password":"hunter2","database":"app"},
		"tls":"verify-full","tls_ca":"certs/ca.pem"}`, 200)
	if strings.Contains(string(env.Data), "hunter2") {
		t.Fatalf("the add response carries the password: %s", env.Data)
	}
	list := decodeData[connListResp](t, env)
	if c, _ := list.find("pg"); c.TLS != "verify-full" || c.TLSCA == "" {
		t.Fatalf("the list lacks the TLS settings: %+v", c)
	}
	cc, _ := e.srv.cfg.ConnByName("pg")
	if want := "host=db.internal port=5432 user=app password='hunter2' dbname=app"; cc.DSN != want {
		t.Fatalf("pg DSN = %q, want %q", cc.DSN, want)
	}
	if want := filepath.Join(config.SavedDir(), "certs", "ca.pem"); config.SavedDir() != "" && cc.TLSCA != want {
		t.Fatalf("tls_ca = %q, want %q", cc.TLSCA, want)
	}
	sc, _, _ := e.srv.saved.Get("pg")
	if sc.TLSCA != "certs/ca.pem" || sc.TLS != "verify-full" {
		t.Fatalf("saved TLS not as typed: %+v", sc.TLSOpts)
	}

	// the edit form's fields: everything but the password
	env = e.api("GET", "/api/v1/conns/pg/parts", "", 200)
	if strings.Contains(string(env.Data), "hunter2") {
		t.Fatalf("parts carry the password: %s", env.Data)
	}
	pr := decodeData[partsResp](t, env)
	if pr.Parts == nil || !pr.HasPassword || pr.Parts.Host != "db.internal" || pr.Parts.User != "app" {
		t.Fatalf("parts = %+v", pr)
	}
	// TLS as typed, not as resolved: an edit saves back what was written
	if pr.TLS.TLSCA != "certs/ca.pem" || pr.TLS.TLS != "verify-full" {
		t.Fatalf("parts' TLS = %+v", pr.TLS)
	}

	// an edit in fields with the password left empty keeps the stored one;
	// the TLS mode changes with it
	e.api("PUT", "/api/v1/conns/pg", `{"name":"pg","driver":"postgres","keep_password":true,
		"parts":{"host":"db2.internal","user":"app","database":"app"},"tls":"require"}`, 200)
	cc, _ = e.srv.cfg.ConnByName("pg")
	if !strings.Contains(cc.DSN, "host=db2.internal") || !strings.Contains(cc.DSN, "password='hunter2'") ||
		cc.TLS != "require" || cc.TLSCA != "" {
		t.Fatalf("after the edit: %q %+v", cc.DSN, cc.TLSOpts)
	}

	// a ${VAR} password is not a secret: the form gets it back
	e.api("POST", "/api/v1/conns", `{"name":"env","driver":"mysql",
		"parts":{"host":"h","user":"u","password":"${MYSQL_PASS}","database":"d"}}`, 200)
	pr = decodeData[partsResp](t, e.api("GET", "/api/v1/conns/env/parts", "", 200))
	if pr.HasPassword || pr.Parts.Password != "${MYSQL_PASS}" {
		t.Fatalf("env parts = %+v", pr)
	}

	// a DSN the fields cannot hold: parts null, and why
	e.api("POST", "/api/v1/conns", connBody("multi", "postgres", "postgres://u@h1,h2/app"), 200)
	pr = decodeData[partsResp](t, e.api("GET", "/api/v1/conns/multi/parts", "", 200))
	if pr.Parts != nil || pr.Reason == "" {
		t.Fatalf("multi-host parts = %+v", pr)
	}
	e.api("GET", "/api/v1/conns/nosuch/parts", "", 404)

	// a CA that cannot be read fails the test before any dial, naming it
	r := decodeData[probeResp](t, e.api("POST", "/api/v1/conns/test", `{"driver":"postgres",
		"parts":{"host":"127.0.0.1","port":"1"},"tls":"verify-full","tls_ca":"/surely/missing/ca.pem"}`, 200))
	if r.OK || !strings.Contains(r.Error, "CA file") {
		t.Fatalf("missing CA: %+v", r)
	}

	// refused forms, each with words for the field at fault
	for _, tc := range []struct {
		body, want string
		status     int
	}{
		{`{"driver":"postgres","parts":{"port":"5432"}}`, "host", 400},
		{`{"driver":"postgres","parts":{"host":"h","port":"x"}}`, "port", 400},
		{`{"driver":"mysql","dsn":"u@tcp(h)/d","tls":"verify_full"}`, "unknown tls mode", 400},
		{`{"driver":"mysql","dsn":"u@tcp(h)/d","tls_ca":"ca.pem"}`, "need a tls mode", 400},
		{`{"driver":"sqlite","dsn":"file:x?mode=memory","tls":"require"}`, "postgres and mysql", 400},
		{`{"driver":"postgres","parts":{"host":"h"},"keep_password":true,"from":"nosuch"}`, "no saved connection", 404},
	} {
		res := e.api("POST", "/api/v1/conns/test", tc.body, tc.status)
		if !strings.Contains(res.Error, tc.want) {
			t.Errorf("%s: error %q, want it to mention %q", tc.body, res.Error, tc.want)
		}
	}
}
