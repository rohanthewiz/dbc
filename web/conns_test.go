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
