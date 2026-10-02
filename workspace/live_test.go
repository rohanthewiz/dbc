package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
)

// The live workspace tests drive the workspace's rules against real
// servers: the ones the SQLite tests above can only approximate, because an
// in-memory SQLite connection is never cut by a server, never killed by an
// administrator, and stops a statement by itself. They are opt-in, with the
// same DSN variables as db's live tests, e.g.
//
//	DBC_LIVE_PG_DSN='postgres://postgres:pw@127.0.0.1:5432/dbc?sslmode=disable' \
//	DBC_LIVE_MYSQL_DSN='root:pw@tcp(127.0.0.1:3306)/dbc' \
//	go test ./workspace -run Live -v
//
// Point them at a throwaway database. They create and drop their own
// objects, all named dbc_live_ws* (a table, two Postgres schemas and a
// Postgres database), apart from db's dbc_live_* ones so that the two
// packages' live tests can run side by side, as `go test ./...` runs them.
//
// Each test goes through the workspace the way a UI does — start a request,
// run its Job, read the event and the state it landed — and checks with a
// second Manager (obs) what the server itself says: which connection a run
// was on, whether it is still there, whether a statement still executes.
// The workspace's word for it ("stopped", "session lost") is only half of
// the rule; the other half is what the server did.

// liveEngine is what these tests need to know about one server. The
// statements are db's live_session_test.go ones, for the same reasons.
type liveEngine struct {
	env, driver string
	// backend returns the server's id for the connection it runs on.
	backend string
	// listed counts the connections with the id given to %s: 1 while the
	// server holds it, 0 once it has let it go.
	listed string
	// kill ends the connection with the id given to %s, from another one.
	kill string
	// heavy keeps the server busy far longer than any test waits, and busy
	// counts the connections with the id given to %s still executing a
	// statement. MySQL's heavy is CPU work rather than SLEEP, which looks
	// at its socket every few seconds and so stops by itself.
	heavy, busy string
}

var liveEngines = []liveEngine{
	{
		env: "DBC_LIVE_PG_DSN", driver: "postgres",
		backend: "SELECT pg_backend_pid()",
		listed:  "SELECT count(*) FROM pg_stat_activity WHERE pid = %s",
		kill:    "SELECT pg_terminate_backend(%s)",
		heavy:   "SELECT pg_sleep(60)",
		busy:    "SELECT count(*) FROM pg_stat_activity WHERE pid = %s AND state = 'active'",
	},
	{
		env: "DBC_LIVE_MYSQL_DSN", driver: "mysql",
		backend: "SELECT CONNECTION_ID()",
		listed:  "SELECT count(*) FROM information_schema.processlist WHERE id = %s",
		kill:    "KILL %s",
		heavy:   "SELECT BENCHMARK(4000000000, MD5('dbc'))",
		busy:    "SELECT count(*) FROM information_schema.processlist WHERE id = %s AND command = 'Query'",
	},
}

// forLive runs fn once per engine, as a subtest named for its driver; each
// skips on its own when its DSN is unset.
func forLive(t *testing.T, fn func(t *testing.T, e liveEngine)) {
	for _, e := range liveEngines {
		t.Run(e.driver, func(t *testing.T) { fn(t, e) })
	}
}

// pgOnly is forLive for what only Postgres has: databases and schemas to
// navigate (db.Navigable).
func pgOnly(t *testing.T, fn func(t *testing.T, e liveEngine)) {
	t.Run("postgres", func(t *testing.T) { fn(t, liveEngines[0]) })
}

// liveTable is the table the tests read and write: dbc_live_ws, with the
// rows (1, 1), (2, 2) and (3, 3). It is made fresh for each test (a test
// that writes to it leaves no trace in the next), and dropped afterwards.
const liveTable = "dbc_live_ws"

// liveWorkspace is a workspace over the one connection "live" on e's server,
// connected as a UI's startup would — or a skip when e's DSN is unset. extra
// adds connections beside it, by name, on the same DSN. obs is a second
// Manager on the same server, for looking at it from outside the
// workspace's pools.
func liveWorkspace(t *testing.T, e liveEngine, extra ...string) (w *Workspace, obs *db.Manager) {
	t.Helper()
	dsn := os.Getenv(e.env)
	if dsn == "" {
		t.Skipf("set %s to run against a live %s", e.env, e.driver)
	}
	conns := []config.Connection{{Name: "live", Driver: e.driver, DSN: dsn}}
	for _, name := range extra {
		conns = append(conns, config.Connection{Name: name, Driver: e.driver, DSN: dsn})
	}
	cfg := &config.Config{
		MaxRows: 1000, MaxDisplayRows: 2000, AIContextRows: config.DefaultAIContextRows,
		DefaultConnection: "live", Connections: conns,
	}

	obs = db.NewManager(cfg)
	t.Cleanup(obs.Close)
	obsExec(t, obs,
		`DROP TABLE IF EXISTS `+liveTable,
		`CREATE TABLE `+liveTable+` (id int PRIMARY KEY, n int)`,
		`INSERT INTO `+liveTable+` VALUES (1, 1), (2, 2), (3, 3)`)
	t.Cleanup(func() { _, _ = obs.Run("live", `DROP TABLE IF EXISTS `+liveTable) })

	mgr := db.NewManager(cfg)
	// Cleanups run last-in first-out: the workspace closes (rolling back
	// its session) before the Manager under it, and both before the table
	// is dropped — a session still holding a row lock would block the DROP.
	t.Cleanup(mgr.Close)
	w = New(cfg, mgr, nil, Options{})
	t.Cleanup(w.Close)
	ev := w.Connect("live").Job().(*Connected)
	if ev.Err != nil || w.Catalog() == nil {
		t.Fatalf("connect: %+v", ev)
	}
	return w, obs
}

// obsExec runs setup statements on obs's "live", one at a time, so a
// failure names its line.
func obsExec(t *testing.T, obs *db.Manager, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if _, err := obs.Run("live", s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

// obsVal reads one value through obs.
func obsVal(t *testing.T, obs *db.Manager, q string) string {
	t.Helper()
	res, err := obs.Run("live", q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	if len(res.Rows) != 1 || len(res.Rows[0]) != 1 {
		t.Fatalf("%s: want one value, got %v", q, res.Rows)
	}
	return res.Rows[0][0]
}

// wsVal runs a one-value query through the workspace, as a user's Ctrl+R,
// and returns the value; the run must succeed.
func wsVal(t *testing.T, w *Workspace, q string) string {
	t.Helper()
	ev := run(t, w, q)
	if ev.Err != nil || ev.Result == nil || len(ev.Result.Rows) != 1 {
		t.Fatalf("%s: %+v", q, ev)
	}
	return ev.Result.Rows[0][0]
}

// waitFor polls q through obs until it reads want, failing after within.
// What a server does about a connection — let it go after a kill, stop a
// statement after a cancel — happens on its own schedule, so a single look
// straight after would race it.
func waitFor(t *testing.T, obs *db.Manager, q, want string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for obsVal(t, obs, q) != want {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not %s after %s", q, want, within)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// killBackend ends connection id from outside, as an administrator (or a
// server's idle timeout) would, and waits until the server has let it go.
func killBackend(t *testing.T, obs *db.Manager, e liveEngine, id string) {
	t.Helper()
	obsExec(t, obs, fmt.Sprintf(e.kill, id))
	waitFor(t, obs, fmt.Sprintf(e.listed, id), "0", 15*time.Second)
}

// hasNote reports whether any note at level l contains text.
func hasNote(notes []Note, l Level, text string) bool {
	for _, n := range notes {
		if n.Level == l && strings.Contains(n.Text, text) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Connecting
// ---------------------------------------------------------------------------

// A connect lands the catalog, and its Counts job lands the row counts —
// counted on the real server, not SQLite's exact count of a tiny table. On
// Postgres it also lands the levels above the tables: the server's
// databases, with the DSN's marked current, and the database's schemas,
// with the one the sidebar opened on.
func TestLiveWorkspaceConnect(t *testing.T) {
	forLive(t, func(t *testing.T, e liveEngine) {
		w, _ := liveWorkspace(t, e)
		ev := w.Connect("live").Job().(*Connected)
		if ev.Err != nil || ev.Changed || ev.Counts == nil {
			t.Fatalf("reconnect: %+v (a connect to the active connection changes nothing)", ev)
		}
		ref, ok := w.TableIndex().Lookup(liveTable)
		if !ok {
			t.Fatalf("%s not in the catalog: %v", liveTable, w.Catalog().Rows)
		}
		rc := ev.Counts().(*RowCounts)
		if rc.Stale || len(rc.Notes) != 0 {
			t.Fatalf("counts: %+v", rc)
		}
		if c := w.RowCounts()[ref]; c.N != 3 {
			t.Errorf("%s count = %+v, want 3 (all: %v)", liveTable, c, w.RowCounts())
		}

		if !db.Navigable(e.driver) {
			// MySQL lists its server's databases (db.HasDatabases), the
			// DSN's own marked current, but a database is its one schema:
			// no schema level, its tables listed whole
			if ev.Schemas != nil || ev.Schema != "" {
				t.Errorf("%s has no schema level: %+v", e.driver, ev)
			}
			cur := ""
			for _, d := range ev.Databases {
				if d.Current {
					cur = d.Name
				}
			}
			if db.HasDatabases(e.driver) && cur == "" {
				t.Errorf("%s lists no current database: %+v", e.driver, ev.Databases)
			}
			return
		}
		cur := ""
		for _, d := range w.Databases() {
			if d.Current {
				cur = d.Name
			}
		}
		if want := wsVal(t, w, `SELECT current_database()`); cur != want {
			t.Errorf("current database = %q, want %q (all: %+v)", cur, want, w.Databases())
		}
		def := ""
		for _, s := range w.Schemas() {
			if s.Default {
				def = s.Name
			}
		}
		if def != "public" {
			t.Errorf("default schema = %q, want public (all: %+v)", def, w.Schemas())
		}
		// With one schema the sidebar lists "" (every schema); with more it
		// opens on the default, which holds the table.
		if s := w.CatalogSchema(); s != "" && s != "public" {
			t.Errorf("sidebar opened on %q", s)
		}
	})
}

// ---------------------------------------------------------------------------
// The pinned session
// ---------------------------------------------------------------------------

// RETRY ONCE: a session that only ever ran queries, cut by the server while
// it sat idle, is replaced and the statement run again on a fresh one — the
// user sees the result, not the server's "terminating connection". The
// cut is seen at once (db.Session peeks at the socket, on MySQL too since
// N-088); a run after a pause pings first, which is the case that matters: a
// person comes back to a session the server dropped while they were away.
func TestLiveWorkspaceRetryOnce(t *testing.T) {
	cases := []struct {
		name string
		wait bool // pause past the session's idle threshold after the cut
	}{
		{"at once", false},
		{"after a pause", true},
	}
	forLive(t, func(t *testing.T, e liveEngine) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				w, obs := liveWorkspace(t, e)
				id := wsVal(t, w, e.backend)
				used := time.Now()
				killBackend(t, obs, e, id)
				if c.wait {
					time.Sleep(time.Until(used.Add(1200 * time.Millisecond)))
				}

				ev := run(t, w, e.backend)
				if ev.Err != nil {
					t.Fatalf("run after the cut: %v — want it retried on a new session", ev.Err)
				}
				if got := ev.Result.Rows[0][0]; got == id {
					t.Errorf("still on the killed connection %s", id)
				}
				if w.LastErr() != "" || hasNote(ev.Notes, Warn, "") || hasNote(ev.Notes, Err, "") {
					t.Errorf("a transparent retry says nothing: lastErr %q, notes %+v", w.LastErr(), ev.Notes)
				}
			})
		}
	})
}

// SESSION LOST: a session that held a transaction is not replayed on a
// fresh one when the server cuts it — the COMMIT would find no transaction
// and "succeed". The run fails as lost, the server has rolled the
// transaction back, and the next run starts on a new session.
func TestLiveWorkspaceSessionLost(t *testing.T) {
	forLive(t, func(t *testing.T, e liveEngine) {
		w, obs := liveWorkspace(t, e)
		run(t, w, "BEGIN")
		if ev := run(t, w, `UPDATE `+liveTable+` SET n = 99 WHERE id = 1`); ev.Err != nil {
			t.Fatalf("update: %v", ev.Err)
		}
		id := wsVal(t, w, e.backend)
		if conn, stateful := w.Session(); conn != "live" || !stateful {
			t.Fatalf("session = %q, stateful %v", conn, stateful)
		}
		killBackend(t, obs, e, id)

		ev := run(t, w, "COMMIT")
		if !errors.Is(ev.Err, db.ErrSessionLost) {
			t.Fatalf("COMMIT after the cut: err = %v, want ErrSessionLost", ev.Err)
		}
		if !hasNote(ev.Notes, Err, "session lost") || !strings.HasPrefix(ev.Status, "error after") {
			t.Errorf("notes = %+v, status %q", ev.Notes, ev.Status)
		}
		if conn, _ := w.Session(); conn != "" {
			t.Errorf("the lost session is still pinned to %q", conn)
		}
		if n := obsVal(t, obs, `SELECT n FROM `+liveTable+` WHERE id = 1`); n != "1" {
			t.Errorf("n = %s, want the lost transaction's 99 rolled back to 1", n)
		}
		if got := wsVal(t, w, e.backend); got == id {
			t.Errorf("the next run is on the killed connection %s", id)
		}
	})
}

// CANCEL REACHES THE SERVER: Stop on a long statement lands as stopped (not
// failed) at once, and the server stops executing it — on MySQL that takes
// the KILL db.Session sends, as the driver only closes its socket. A stop
// in a transaction costs the session (the driver drops the connection to
// abort the statement), which the run says, and the server rolls back what
// the transaction had done. Either way the next run works.
func TestLiveWorkspaceCancel(t *testing.T) {
	cases := []struct {
		name string
		tx   bool // inside a transaction that has written
	}{
		{"stateless", false},
		{"in a transaction", true},
	}
	forLive(t, func(t *testing.T, e liveEngine) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				w, obs := liveWorkspace(t, e)
				if c.tx {
					run(t, w, "BEGIN")
					run(t, w, `UPDATE `+liveTable+` SET n = 99 WHERE id = 1`)
				}
				id := wsVal(t, w, e.backend)
				st, err := w.RunStmts([]string{e.heavy}, "query")
				if err != nil {
					t.Fatal(err)
				}
				done := async(st.Job)
				waitFor(t, obs, fmt.Sprintf(e.busy, id), "1", 5*time.Second)

				if n, status := w.Cancel(); !strings.Contains(n.Text, "stopping query") || status != "stopping query…" {
					t.Errorf("cancel = %+v, %q", n, status)
				}
				ev := await(t, done).(*RunDone)
				if !errors.Is(ev.Err, db.ErrCanceled) || !strings.HasPrefix(ev.Status, "stopped after") {
					t.Fatalf("err = %v, status %q", ev.Err, ev.Status)
				}
				if ev.Elapsed > 5*time.Second {
					t.Errorf("the stop took %s to land", ev.Elapsed)
				}
				if w.LastErr() != "" || w.Busy() {
					t.Errorf("a stop is not a failure: lastErr %q, busy %v", w.LastErr(), w.Busy())
				}
				if lost := hasNote(ev.Notes, Warn, "the session was lost"); lost != errors.Is(ev.Err, db.ErrSessionLost) {
					t.Errorf("the notes and the error disagree about the session: %+v / %v", ev.Notes, ev.Err)
				}
				if c.tx && !errors.Is(ev.Err, db.ErrSessionLost) {
					// Not a rule, but what both drivers do today; if one
					// starts keeping the connection, the rollback check
					// below no longer holds and this test needs rethinking.
					t.Fatalf("stopping inside a transaction kept the session: %v", ev.Err)
				}
				// Well inside the statement's own length: only a stop on the
				// server gets there in time.
				waitFor(t, obs, fmt.Sprintf(e.busy, id), "0", 3*time.Second)
				if c.tx {
					if n := obsVal(t, obs, `SELECT n FROM `+liveTable+` WHERE id = 1`); n != "1" {
						t.Errorf("n = %s, want the stopped transaction rolled back to 1", n)
					}
				}
				if ev := run(t, w, `SELECT count(*) FROM `+liveTable); ev.Err != nil || ev.Result.Rows[0][0] != "3" {
					t.Errorf("the run after the stop: %+v", ev)
				}
			})
		}
	})
}

// An EXPLAIN ANALYZE runs its statement, so its Stop must reach the server
// as a run's does: db.Session.Explain runs under the same guard as Run.
func TestLiveWorkspaceCancelExplain(t *testing.T) {
	forLive(t, func(t *testing.T, e liveEngine) {
		w, obs := liveWorkspace(t, e)
		id := wsVal(t, w, e.backend)
		st, err := w.Explain(e.heavy, "", true)
		if err != nil {
			t.Fatal(err)
		}
		done := async(st.Job)
		waitFor(t, obs, fmt.Sprintf(e.busy, id), "1", 5*time.Second)
		w.Cancel()
		ev := await(t, done).(*ExplainDone)
		if !errors.Is(ev.Err, db.ErrCanceled) || !strings.HasPrefix(ev.Status, "stopped after") {
			t.Fatalf("err = %v, status %q", ev.Err, ev.Status)
		}
		waitFor(t, obs, fmt.Sprintf(e.busy, id), "0", 3*time.Second)
	})
}

// ---------------------------------------------------------------------------
// Switching connections
// ---------------------------------------------------------------------------

// Switching to another connection closes the session pinned to the old one
// through the event's Release job: on the server, its connection goes, and
// with it the open transaction — the row it had written is back, and its
// lock is free for anyone. A run before the release finished would see the
// new connection; the old one is never reused.
func TestLiveWorkspaceSwitch(t *testing.T) {
	forLive(t, func(t *testing.T, e liveEngine) {
		w, obs := liveWorkspace(t, e, "live2")
		run(t, w, "BEGIN")
		run(t, w, `UPDATE `+liveTable+` SET n = 99 WHERE id = 1`)
		id := wsVal(t, w, e.backend)

		ev := w.Switch("live2").Job().(*Connected)
		if ev.Err != nil || !ev.Changed || ev.Left != "live" || ev.Release == nil || w.Active() != "live2" {
			t.Fatalf("switch: %+v, active %q", ev, w.Active())
		}
		if w.Derived(ev.Left) {
			t.Error("a configured connection is not derived: its pool is kept")
		}
		rel, _ := ev.Release().(*SessionReleased)
		if rel == nil || rel.Conn != "live" || !rel.Stateful || !hasNote(rel.Notes, Warn, "left live") {
			t.Fatalf("release = %+v", rel)
		}
		waitFor(t, obs, fmt.Sprintf(e.listed, id), "0", 15*time.Second)
		if n := obsVal(t, obs, `SELECT n FROM `+liveTable+` WHERE id = 1`); n != "1" {
			t.Errorf("n = %s, want the left transaction rolled back to 1", n)
		}
		// the row lock went with it: a write through the new connection's
		// session goes straight through rather than waiting on it
		if ev := run(t, w, `UPDATE `+liveTable+` SET n = 5 WHERE id = 1`); ev.Err != nil || ev.Conn != "live2" {
			t.Fatalf("write on live2: %+v", ev)
		}
		if got := wsVal(t, w, e.backend); got == id {
			t.Errorf("live2's session is on live's old connection %s", id)
		}
	})
}

// A database picked from the server's list is a connection derived onto it
// ("live/<database>"). Switching there opens that database; switching away
// reports it as Left and Derived, which is a UI's cue to close its pool
// (db.Manager.Disconnect) — and after that close the server holds no
// connection to it at all, so the browsing does not pile up pools.
func TestLiveWorkspaceDerivedDatabase(t *testing.T) {
	pgOnly(t, func(t *testing.T, e liveEngine) {
		w, obs := liveWorkspace(t, e)
		const other = "dbc_live_ws_other"
		drop := `DROP DATABASE IF EXISTS ` + other + ` WITH (FORCE)`
		obsExec(t, obs, drop, `CREATE DATABASE `+other)
		t.Cleanup(func() { _, _ = obs.Run("live", drop) })
		onOther := fmt.Sprintf(`SELECT count(*) FROM pg_stat_activity WHERE datname = '%s'`, other)

		// the list was read at connect, before the database existed: connect
		// again for one that has it
		_, st, err := w.Disconnect()
		if err != nil {
			t.Fatal(err)
		}
		st.Job()
		if ev := w.Connect("live").Job().(*Connected); ev.Err != nil {
			t.Fatalf("reconnect: %+v", ev)
		}
		listed := false
		for _, d := range w.Databases() {
			listed = listed || (d.Name == other && !d.Current)
		}
		if !listed {
			t.Fatalf("%s not in the database list: %+v", other, w.Databases())
		}

		derived := config.DerivedName("live", other)
		ev := w.Switch(derived).Job().(*Connected)
		if ev.Err != nil || !ev.Changed || ev.Left != "live" || !w.Derived(derived) {
			t.Fatalf("switch to %s: %+v", derived, ev)
		}
		if got := wsVal(t, w, `SELECT current_database()`); got != other {
			t.Errorf("on %s, current_database() = %s", derived, got)
		}
		cur := ""
		for _, d := range w.Databases() {
			if d.Current {
				cur = d.Name
			}
		}
		if cur != other {
			t.Errorf("current database in the list = %q, want %s", cur, other)
		}
		if n := obsVal(t, obs, onOther); n == "0" {
			t.Fatal("no connection to the picked database")
		}

		back := w.Switch("live").Job().(*Connected)
		if back.Err != nil || back.Left != derived || !w.Derived(back.Left) {
			t.Fatalf("switch back: %+v", back)
		}
		// what dbc web's closeLeft and the TUI's releaseThenClose do
		back.Release()
		if !w.Manager().Disconnect(back.Left) {
			t.Errorf("Disconnect(%s) found no pool to close", back.Left)
		}
		waitFor(t, obs, onOther, "0", 15*time.Second)
		if got := wsVal(t, w, `SELECT current_database()`); got == other {
			t.Error("back on live, still in the other database")
		}
	})
}

// A schema pick replaces the sidebar's tables with another schema's, on the
// active connection, and counts their rows; "all schemas" lists every one;
// a schema that is not there falls back to the default and says so. The
// catalog's index follows each pick, so a table of the picked schema is
// found by its qualified name — what Show columns and the assistant need.
func TestLiveWorkspacePickSchema(t *testing.T) {
	pgOnly(t, func(t *testing.T, e liveEngine) {
		w, obs := liveWorkspace(t, e)
		drop := `DROP SCHEMA IF EXISTS dbc_live_wsa CASCADE; DROP SCHEMA IF EXISTS dbc_live_wsb CASCADE`
		for _, s := range strings.Split(drop, "; ") {
			obsExec(t, obs, s)
		}
		obsExec(t, obs,
			`CREATE SCHEMA dbc_live_wsa`, `CREATE TABLE dbc_live_wsa.t1 (id int)`,
			`CREATE SCHEMA dbc_live_wsb`, `CREATE TABLE dbc_live_wsb.t2 (id int)`, `CREATE TABLE dbc_live_wsb.t3 (id int)`,
			`INSERT INTO dbc_live_wsb.t2 VALUES (1), (2)`)
		t.Cleanup(func() {
			for _, s := range strings.Split(drop, "; ") {
				_, _ = obs.Run("live", s)
			}
		})
		// reconnect, so the schema list holds the new schemas
		_, st, _ := w.Disconnect()
		st.Job()
		if ev := w.Connect("live").Job().(*Connected); ev.Err != nil || ev.Schema != "public" {
			t.Fatalf("connect: %+v, want it on public, the default with tables", ev)
		}

		// completion's schema is read once here, before any pick: the picks
		// below must keep it (they change only the ranking) — N-095
		if err := w.LoadCompletions(context.Background()); err != nil {
			t.Fatalf("load completions: %v", err)
		}

		pick := func(p SchemaPick) *SchemaLoaded {
			t.Helper()
			st, err := w.PickSchema(p)
			if err != nil {
				t.Fatalf("pick %+v: %v", p, err)
			}
			ev := st.Job().(*SchemaLoaded)
			if ev.Stale || ev.Catalog == nil {
				t.Fatalf("pick %+v: %+v", p, ev)
			}
			return ev
		}

		ev := pick(SchemaPick{Name: "dbc_live_wsb"})
		if ev.Schema != "dbc_live_wsb" || w.CatalogSchema() != "dbc_live_wsb" || len(ev.Catalog.Rows) != 2 {
			t.Fatalf("pick wsb: schema %q, tables %v", ev.Schema, ev.Catalog.Rows)
		}
		// the pick kept completion's cache, and its tables now rank first
		buf := "SELECT * FROM t"
		res, ready := w.Complete(buf, len(buf))
		if !ready {
			t.Fatal("the schema pick dropped completion's cache")
		}
		order := map[string]int{}
		for i, it := range res.Items {
			if _, seen := order[it.Label]; !seen {
				order[it.Label] = i
			}
		}
		if i2, ok2 := order["t2"]; !ok2 || order["t1"] < i2 {
			t.Errorf("after picking wsb, t2 should rank before t1: %v", order)
		}
		ref, ok := w.TableIndex().Lookup("dbc_live_wsb.t2")
		if !ok {
			t.Fatal("dbc_live_wsb.t2 not found in the picked schema's index")
		}
		if rc := ev.Counts().(*RowCounts); rc.Stale || w.RowCounts()[ref].N != 2 {
			t.Errorf("counts: %+v, t2 = %+v", rc, w.RowCounts()[ref])
		}
		// Show columns runs through the index, on the session
		cst, err := w.ShowColumns("dbc_live_wsb.t2")
		if err != nil {
			t.Fatalf("show columns: %v", err)
		}
		if cev := cst.Job().(*RunDone); cev.Err != nil || len(cev.Result.Rows) != 1 {
			t.Errorf("columns of t2: %+v", cev)
		}

		ev = pick(SchemaPick{All: true})
		names := map[string]bool{}
		for _, r := range db.TableRefs(ev.Catalog.Rows) {
			names[r.Schema+"."+r.Name] = true
		}
		if ev.Schema != "" || !names["dbc_live_wsa.t1"] || !names["dbc_live_wsb.t3"] || !names["public."+liveTable] {
			t.Errorf("all schemas: schema %q, tables %v", ev.Schema, names)
		}

		ev = pick(SchemaPick{Name: "dbc_live_ws_gone"})
		if ev.Schema != "public" || !hasNote(ev.Notes, Info, "no schema dbc_live_ws_gone here any more: listing public") {
			t.Errorf("a gone schema: %q, notes %+v", ev.Schema, ev.Notes)
		}
	})
}

// liveWorkspaceDSN is liveWorkspace's workspace on a DSN of the test's own
// (the env's with something added), with no table of its own: for checks
// that need the connection set up differently, such as its search_path.
func liveWorkspaceDSN(t *testing.T, e liveEngine, dsn string) *Workspace {
	t.Helper()
	cfg := &config.Config{
		MaxRows: 1000, MaxDisplayRows: 2000, AIContextRows: config.DefaultAIContextRows,
		DefaultConnection: "live", Connections: []config.Connection{{Name: "live", Driver: e.driver, DSN: dsn}},
	}
	mgr := db.NewManager(cfg)
	t.Cleanup(mgr.Close)
	w := New(cfg, mgr, nil, Options{})
	t.Cleanup(w.Close)
	if ev := w.Connect("live").Job().(*Connected); ev.Err != nil {
		t.Fatalf("connect: %+v", ev)
	}
	return w
}

// completeAt is Workspace.Complete at the ▮ in buf, failing when the cache
// is cold.
func completeAt(t *testing.T, w *Workspace, buf string) map[string]sqlcompleteItem {
	t.Helper()
	i := strings.Index(buf, "▮")
	res, ready := w.Complete(buf[:i]+buf[i+len("▮"):], i)
	if !ready {
		t.Fatal("completion's cache is cold")
	}
	out := map[string]sqlcompleteItem{}
	for _, it := range res.Items {
		// a label offered from two schemas is keyed by the detail too
		out[it.Label+" · "+strings.SplitN(it.Detail, " ·", 2)[0]] = sqlcompleteItem{Insert: it.Insert, Detail: it.Detail}
	}
	return out
}

type sqlcompleteItem struct{ Insert, Detail string }

// COMPLETION ON A REAL POSTGRES (N-094): a table outside public goes in
// qualified and public's bare; a mixed-case name is quoted; and with a
// search_path naming another schema first, that schema's tables go in
// bare too — and a public table it shadows is qualified, since the bare
// name would find the other one.
func TestLiveWorkspaceCompletionNames(t *testing.T) {
	pgOnly(t, func(t *testing.T, e liveEngine) {
		w, obs := liveWorkspace(t, e)
		drop := `DROP SCHEMA IF EXISTS dbc_live_wsc CASCADE`
		obsExec(t, obs, drop,
			`CREATE SCHEMA dbc_live_wsc`,
			`CREATE TABLE dbc_live_wsc."MixedCase" ("Amount" numeric, id int)`,
			`CREATE TABLE dbc_live_wsc.plain (id int)`,
			`CREATE TABLE dbc_live_wsc.`+liveTable+` (other int)`) // shadows public's on the path below
		t.Cleanup(func() { _, _ = obs.Run("live", drop) })
		_, st, _ := w.Disconnect()
		st.Job()
		if ev := w.Connect("live").Job().(*Connected); ev.Err != nil {
			t.Fatalf("reconnect: %+v", ev)
		}
		if err := w.LoadCompletions(context.Background()); err != nil {
			t.Fatalf("load completions: %v", err)
		}

		// the default search_path ("$user", public)
		got := completeAt(t, w, "SELECT * FROM ▮")
		for key, want := range map[string]string{
			liveTable + " · public":       liveTable,
			liveTable + " · dbc_live_wsc": "dbc_live_wsc." + liveTable,
			"MixedCase · dbc_live_wsc":    `dbc_live_wsc."MixedCase"`,
			"plain · dbc_live_wsc":        "dbc_live_wsc.plain",
		} {
			if it, ok := got[key]; !ok || it.Insert != want {
				t.Errorf("default path: %s inserts %q, want %q", key, it.Insert, want)
			}
		}
		cols := completeAt(t, w, `SELECT m.▮ FROM dbc_live_wsc."MixedCase" m`)
		if it, ok := cols["Amount · numeric"]; !ok || it.Insert != `"Amount"` {
			t.Errorf(`m. → %v; want "Amount" quoted`, cols)
		}

		// search_path = dbc_live_wsc, public, set on the connection (pgx
		// passes an unknown DSN parameter to the server as a setting)
		dsn := os.Getenv(e.env)
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		w2 := liveWorkspaceDSN(t, e, dsn+sep+"search_path=dbc_live_wsc,public")
		if err := w2.LoadCompletions(context.Background()); err != nil {
			t.Fatalf("load completions: %v", err)
		}
		got = completeAt(t, w2, "SELECT * FROM ▮")
		for key, want := range map[string]string{
			"plain · dbc_live_wsc":        "plain",
			"MixedCase · dbc_live_wsc":    `"MixedCase"`,
			liveTable + " · dbc_live_wsc": liveTable,
			liveTable + " · public":       "public." + liveTable,
		} {
			if it, ok := got[key]; !ok || it.Insert != want {
				t.Errorf("search_path wsc,public: %s inserts %q, want %q", key, it.Insert, want)
			}
		}
		// and a bare dbc_live_ws in the statement is the path's first
		cols = completeAt(t, w2, "SELECT x.▮ FROM "+liveTable+" x")
		if _, ok := cols["other · integer"]; !ok {
			t.Errorf("x. on the path's %s → %v; want dbc_live_wsc's column other", liveTable, cols)
		}
	})
}

// A BIG CATALOG (N-094), opt-in with DBC_LIVE_BIG=1 on top of the DSN, as
// it builds thousands of tables: the first load of a catalog just under the
// schema reader's 250,000-row bound succeeds in reasonable time (logged),
// and one past it fails with the reason the UIs show as "completion is
// without the schema", while completion goes on with the vocabulary.
func TestLiveWorkspaceCompletionBigCatalog(t *testing.T) {
	if os.Getenv("DBC_LIVE_BIG") == "" {
		t.Skip("set DBC_LIVE_BIG=1 (with DBC_LIVE_PG_DSN) to build a 260,000-column catalog")
	}
	pgOnly(t, func(t *testing.T, e liveEngine) {
		w, obs := liveWorkspace(t, e)
		drop := `DROP SCHEMA IF EXISTS dbc_live_wsbig CASCADE`
		obsExec(t, obs, drop, `CREATE SCHEMA dbc_live_wsbig`)
		t.Cleanup(func() { _, _ = obs.Run("live", drop) })
		// tables from..to, 100 int columns each
		build := func(from, to int) {
			t.Helper()
			start := time.Now()
			obsExec(t, obs, fmt.Sprintf(`DO $$
DECLARE cols text := (SELECT string_agg('c' || j || ' int', ', ') FROM generate_series(1, 100) j);
BEGIN
  FOR i IN %d..%d LOOP
    EXECUTE format('CREATE TABLE dbc_live_wsbig.t%%s (%%s)', i, cols);
  END LOOP;
END $$`, from, to))
			t.Logf("built tables %d..%d in %s", from, to, time.Since(start).Round(time.Millisecond))
		}
		reload := func() error {
			t.Helper()
			_, st, _ := w.Disconnect()
			st.Job()
			if ev := w.Connect("live").Job().(*Connected); ev.Err != nil {
				t.Fatalf("reconnect: %+v", ev)
			}
			start := time.Now()
			err := w.LoadCompletions(context.Background())
			t.Logf("first load: %s (err %v)", time.Since(start).Round(time.Millisecond), err)
			return err
		}

		build(1, 2400) // 240,000 column rows, under the bound
		if err := reload(); err != nil {
			t.Fatalf("a catalog under the bound failed to load: %v", err)
		}
		got := completeAt(t, w, "SELECT * FROM dbc_live_wsbig.t239▮")
		if _, ok := got["t2399 · dbc_live_wsbig"]; !ok {
			t.Errorf("dbc_live_wsbig.t239 → %v", got)
		}

		build(2401, 2600) // 260,000: past it
		err := reload()
		if err == nil || !strings.Contains(err.Error(), "too big") {
			t.Fatalf("a catalog past the bound: err = %v, want too big", err)
		}
		res, ready := w.Complete("SEL", 3)
		if !ready || len(res.Items) == 0 || res.Items[0].Label != "SELECT" {
			t.Errorf("after a failed load, completion offers %v (ready %v), want the vocabulary", res.Items, ready)
		}
	})
}
