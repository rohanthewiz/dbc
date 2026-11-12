package workspace

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rohanthewiz/dbc/config"
)

// The catalog is read after the connect lands, not inside it (ConnectPick):
// these tests are about the gap between the two landings — what the
// workspace is while its catalog is on the way, and what may supersede the
// read. Tests about what a connect ends up listing use landed, which runs
// both.

// lazyConn adds a second, seeded-empty SQLite connection with one table
// and returns the workspace: something to connect to whose catalog is
// known not to be the demo's.
func lazyConn(t *testing.T) *Workspace {
	t.Helper()
	w := newTestWorkspace(t)
	addConn(w, config.Connection{Name: "other", Driver: "sqlite", DSN: memDSN()})
	if _, err := w.mgr.Run("other", "CREATE TABLE only_here (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	return w
}

// dial runs a switch's Job alone — the connect lands, its catalog read
// (Connected.Load) is left to the test.
func dial(t *testing.T, w *Workspace, name string) *Connected {
	t.Helper()
	ev := w.Switch(name).Job().(*Connected)
	if ev.Err != nil || ev.Load == nil {
		t.Fatalf("connect to %s: err %v, Load set %v", name, ev.Err, ev.Load != nil)
	}
	return ev
}

// A connect lands as soon as the connection is open: the workspace is on
// it and runs statements, with nothing listed and CatalogLoading saying
// why; the Load job then fills the sidebar.
func TestConnectLandsBeforeItsCatalog(t *testing.T) {
	w := lazyConn(t)
	ev := dial(t, w, "other")

	if !ev.Changed || ev.Left != demo || ev.Status != "connected" || ev.Catalog != nil {
		t.Fatalf("event = %+v", ev)
	}
	if n := ev.Notes[len(ev.Notes)-1]; n.Text != "connected to other" || n.Level != Ok {
		t.Errorf("last note = %+v", n)
	}
	if w.Active() != "other" || !w.Connected() {
		t.Fatalf("active = %q, connected %v", w.Active(), w.Connected())
	}
	// the demo's tables must not sit under the other connection's name
	if w.Catalog() != nil || w.TableIndex() != nil || !w.CatalogLoading() {
		t.Fatalf("mid-load: catalog %v, loading %v", w.Catalog() != nil, w.CatalogLoading())
	}
	// the connection is usable before its sidebar is: that is the point
	if res := run(t, w, "SELECT 41 + 1 AS n"); res.Err != nil || res.Result.Rows[0][0] != "42" {
		t.Fatalf("a run during the load: %+v", res)
	}
	// a second click on the row neither dials again nor restarts the read
	if st := w.Switch("other"); st.Job != nil {
		t.Error("switching to the connection whose catalog is loading should do nothing")
	}
	// there are no schemas to resolve a pick against yet
	_, err := w.PickSchema(SchemaPick{Name: "main"})
	refusal(t, err, Busy)

	ld := ev.Load().(*CatalogLoaded)
	if ld.Stale || ld.Canceled || ld.Conn != "other" || ld.Catalog == nil || len(ld.Notes) != 0 {
		t.Fatalf("loaded = %+v", ld)
	}
	if w.CatalogLoading() || w.Catalog() != ld.Catalog || !hasTable(w, "only_here") {
		t.Errorf("after the load: loading %v, only_here listed %v", w.CatalogLoading(), hasTable(w, "only_here"))
	}
}

// The catalog's row counting starts from the load's landing, not the
// connect's: there is nothing to count until the tables are listed.
func TestCatalogLoadStartsTheCounting(t *testing.T) {
	w := lazyConn(t)
	countsOn(t, w)
	ev := dial(t, w, "other")
	if ev.Counts != nil {
		t.Error("the connect made a Counts job with no tables listed")
	}
	ld := ev.Load().(*CatalogLoaded)
	if ld.Counts == nil {
		t.Fatal("the load made no Counts job with the counts on")
	}
	if rc := ld.Counts().(*RowCounts); rc.Stale || len(rc.Counts) != 1 {
		t.Errorf("counts = %+v", rc)
	}
}

// A later connect supersedes a catalog read still in flight: the read
// lands Stale and installs nothing over the newer connection's list.
func TestCatalogLoadSupersededByAConnect(t *testing.T) {
	w := lazyConn(t)
	old := dial(t, w, "other")
	if ev := landed(w.Switch(demo)); ev.Err != nil || !hasTable(w, "cats") {
		t.Fatalf("switch back: %+v", ev)
	}
	if ld := old.Load().(*CatalogLoaded); !ld.Stale {
		t.Fatalf("the superseded load landed: %+v", ld)
	}
	if w.Active() != demo || !hasTable(w, "cats") || hasTable(w, "only_here") {
		t.Error("the stale load replaced the demo's list")
	}
}

// Disconnect abandons the read with the connection.
func TestCatalogLoadSupersededByDisconnect(t *testing.T) {
	w := lazyConn(t)
	ev := dial(t, w, "other")
	if _, _, err := w.Disconnect(); err != nil {
		t.Fatal(err)
	}
	if w.CatalogLoading() {
		t.Error("still loading after the disconnect")
	}
	if ld := ev.Load().(*CatalogLoaded); !ld.Stale || w.Catalog() != nil {
		t.Fatalf("the load landed after the disconnect: %+v", ld)
	}
}

// Cancel, with no run and no dial in flight, stops the catalog read. It
// lands as canceled — the connection stays — and a Refresh reads it again.
func TestCancelStopsTheCatalogLoad(t *testing.T) {
	w := lazyConn(t)
	ev := dial(t, w, "other")
	if n, _ := w.Cancel(); n.Text != "canceling the catalog read of other…" {
		t.Fatalf("cancel note = %+v", n)
	}
	if !w.CatalogLoading() {
		t.Error("the read is no longer marked as loading before it has landed")
	}
	ld := ev.Load().(*CatalogLoaded)
	if ld.Stale || !ld.Canceled || ld.Catalog != nil || ld.Status != "catalog read canceled" {
		t.Fatalf("loaded = %+v", ld)
	}
	if n := ld.Notes[len(ld.Notes)-1]; !strings.HasPrefix(n.Text, "catalog read of other canceled") || n.Level != Warn {
		t.Errorf("last note = %+v", n)
	}
	// no "tables list unavailable" beside it: a cancel is not a failure
	for _, n := range ld.Notes {
		if strings.Contains(n.Text, "unavailable") {
			t.Errorf("a canceled read reported a failure: %q", n.Text)
		}
	}
	if w.CatalogLoading() || w.Active() != "other" || !w.Connected() {
		t.Fatalf("after the cancel: loading %v, active %q", w.CatalogLoading(), w.Active())
	}
	if n, _ := w.Cancel(); n.Text != "nothing is running" {
		t.Errorf("a second cancel = %+v", n)
	}

	st, err := w.Refresh()
	if err != nil {
		t.Fatal(err)
	}
	if re := st.Job().(*Connected); re.Err != nil || re.Status != "refreshed" || !hasTable(w, "only_here") {
		t.Fatalf("refresh after the cancel: %+v", re)
	}
}

// A run in flight is what Cancel stops first: with the catalog read after
// the connect, a statement can be running while the sidebar still loads.
func TestCancelPrefersTheRunToTheCatalogLoad(t *testing.T) {
	w := lazyConn(t)
	ev := dial(t, w, "other")
	slow := "WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n) SELECT count(*) FROM n"
	st, err := w.RunStmts([]string{slow}, "query")
	if err != nil {
		t.Fatal(err)
	}
	done := async(st.Job)
	if n, _ := w.Cancel(); n.Text != "stopping query…" {
		t.Fatalf("cancel note = %+v", n)
	}
	await(t, done)
	if ld := ev.Load().(*CatalogLoaded); ld.Canceled || ld.Catalog == nil {
		t.Fatalf("the run's stop canceled the catalog read: %+v", ld)
	}
}

// A Refresh during the load takes it over rather than being refused: it
// reads for the pick the connect was made with, and the load lands Stale.
func TestRefreshSupersedesTheCatalogLoad(t *testing.T) {
	w := lazyConn(t)
	ev := dial(t, w, "other")
	st, err := w.Refresh()
	if err != nil {
		t.Fatal(err)
	}
	re := st.Job().(*Connected)
	if re.Err != nil || re.Changed || re.Load != nil || re.Catalog == nil || !hasTable(w, "only_here") {
		t.Fatalf("refresh = %+v", re)
	}
	if w.CatalogLoading() {
		t.Error("still loading after the refresh landed the catalog")
	}
	if ld := ev.Load().(*CatalogLoaded); !ld.Stale {
		t.Fatalf("the superseded load landed: %+v", ld)
	}
}

// refreshPickLocked asks for the connect's pick while its read is in
// flight: nothing is listed yet, so what is listed says nothing.
func TestRefreshPickHonoursACatalogLoadInFlight(t *testing.T) {
	w := lazyConn(t)
	ev := w.SwitchPick("other", SchemaPick{Name: "sales"}).Job().(*Connected)
	if ev.Load == nil {
		t.Fatal("no Load job")
	}
	w.mu.Lock()
	got := w.refreshPickLocked()
	w.mu.Unlock()
	if got != (SchemaPick{Name: "sales"}) {
		t.Errorf("pick mid-load = %+v", got)
	}
	ev.Load()
	w.mu.Lock()
	got = w.refreshPickLocked()
	w.mu.Unlock()
	if got != w.defaultPick() { // SQLite: one schema, every list is ""
		t.Errorf("pick after the load = %+v", got)
	}
}

// DDL run while the catalog still loads relists: the load may have read
// the tables before the DDL landed, and nothing else would correct it.
func TestDDLDuringTheCatalogLoadRelists(t *testing.T) {
	w := lazyConn(t)
	ev := dial(t, w, "other")
	res := run(t, w, "CREATE TABLE made_mid_load (id INTEGER)")
	if res.Err != nil || res.Relist == nil {
		t.Fatalf("run = %+v", res)
	}
	if re := res.Relist().(*Connected); re.Err != nil || !re.Relisted || !hasTable(w, "made_mid_load") {
		t.Fatalf("relist = %+v", re)
	}
	if ld := ev.Load().(*CatalogLoaded); !ld.Stale {
		t.Fatalf("the superseded load landed: %+v", ld)
	}
}

// Against a real Postgres whose catalog cannot be read just now: the
// connect still lands, at once, and the workspace is on the connection
// while the catalog read waits. This is the case the split was made for —
// a production database whose schema summary spent its whole budget held
// "connecting…" for as long, with the connection open all the while.
//
// The catalog is stalled with an ACCESS EXCLUSIVE lock on pg_namespace,
// held by another session: every sidebar read scans it or resolves a name
// through it, and queues behind the lock, where a login touches no
// namespace and passes. (A lock on pg_class would stall the logins too.)
//
//	observer: BEGIN; LOCK pg_namespace ────────── ROLLBACK
//	workspace:        connect ─► lands at once        │
//	                  Load ───── (queued) ────────────┴─► lands, tables
//
// No statement is run under the lock, though running one mid-load is the
// point of the split (TestConnectLandsBeforeItsCatalog does, on SQLite):
// pgx prepares what it runs, and preparing even SELECT 1 resolves
// search_path through pg_namespace, so it would queue too — behind this
// test's lock, not behind the catalog read.
//
// Opt-in, like the other live tests: needs DBC_LIVE_PG_DSN.
func TestLiveConnectLandsAheadOfABlockedCatalog(t *testing.T) {
	w, obs := liveWorkspace(t, liveEngines[0])
	if liveEngines[0].driver != "postgres" {
		t.Fatalf("liveEngines[0] is %s; this test locks a Postgres catalog", liveEngines[0].driver)
	}
	_, st, _ := w.Disconnect()
	if st.Job != nil {
		st.Job()
	}

	dbh, err := obs.DBContext(context.Background(), "live")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := dbh.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			_ = tx.Rollback()
		}
	}
	t.Cleanup(release) // a failure below must not leave the catalog locked
	if _, err := tx.Exec(`LOCK TABLE pg_catalog.pg_namespace IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock: %v", err)
	}

	// Each step is awaited with a deadline rather than called: one that
	// wrongly waits on the lock must fail here, not hang the test with
	// the catalog locked until go test's own timeout.
	start := time.Now()
	ev := await(t, async(w.Connect("live").Job)).(*Connected)
	if ev.Err != nil || ev.Load == nil || !w.Connected() || !w.CatalogLoading() {
		t.Fatalf("connect under the lock: %+v", ev)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("the connect took %s with the catalog blocked: it waited for something it should not", d)
	}
	loading := async(ev.Load)
	select {
	case got := <-loading:
		t.Fatalf("the catalog read landed under the lock: %+v", got)
	case <-time.After(300 * time.Millisecond): // still queued behind the lock, as arranged
	}
	if w.Catalog() != nil || !w.CatalogLoading() || w.Active() != "live" {
		t.Errorf("mid-load: catalog %v, loading %v, active %q", w.Catalog() != nil, w.CatalogLoading(), w.Active())
	}

	release()
	ld := await(t, loading).(*CatalogLoaded)
	if ld.Stale || ld.Canceled || ld.Catalog == nil || len(ld.Schemas) == 0 || w.CatalogLoading() {
		t.Fatalf("loaded after the lock went: %+v", ld)
	}
}
