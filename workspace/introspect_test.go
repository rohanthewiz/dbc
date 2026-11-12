package workspace

import (
	"context"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/config"
)

// Levels of introspection (introspect.go), on SQLite: what a connect reads
// at off, what ends the unread state, and which level a read runs at. What
// names changes on a database with schemas to count — the summary skipped,
// completion scoped, a slow summary remembered — needs Postgres, and is in
// live_test.go.

// offConn is lazyConn with the other connection's introspection off.
func offConn(t *testing.T) *Workspace {
	t.Helper()
	w := newTestWorkspace(t)
	addConn(w, config.Connection{Name: "other", Driver: "sqlite", DSN: memDSN(), Introspect: config.IntrospectOff})
	if _, err := w.mgr.Run("other", "CREATE TABLE only_here (id INTEGER, label TEXT)"); err != nil {
		t.Fatal(err)
	}
	return w
}

// noteTexts is the notes' words, one per line, for a Contains.
func noteTexts(notes []Note) string {
	out := make([]string, len(notes))
	for i, n := range notes {
		out[i] = n.Text
	}
	return strings.Join(out, "\n")
}

// The effective level is the connection's own, else the global default,
// else full — lowered from full to names once the Manager remembers the
// connection's schema summary as slow, and never raised by that memory.
func TestLevelOf(t *testing.T) {
	w := newTestWorkspace(t)
	addConn(w, config.Connection{Name: "n", Driver: "sqlite", DSN: memDSN(), Introspect: config.IntrospectNames})
	addConn(w, config.Connection{Name: "o", Driver: "sqlite", DSN: memDSN(), Introspect: config.IntrospectOff})
	for name, want := range map[string]level{demo: levelFull, "n": levelNames, "o": levelOff} {
		if got := levelOf(w.cfg, w.mgr, name); got != want {
			t.Errorf("levelOf(%s) = %d, want %d", name, got, want)
		}
	}
	w.cfg.Introspect = config.IntrospectOff
	if got := levelOf(w.cfg, w.mgr, demo); got != levelOff {
		t.Errorf("under a global off, demo reads at %d", got)
	}
	if got := levelOf(w.cfg, w.mgr, "n"); got != levelNames {
		t.Errorf("a connection's own names lost to the global off: %d", got)
	}
	w.cfg.Introspect = ""

	w.mgr.MarkSummarySlow(demo)
	w.mgr.MarkSummarySlow("o")
	if got := levelOf(w.cfg, w.mgr, demo); got != levelNames {
		t.Errorf("a slow summary left demo at %d, want names", got)
	}
	if got := levelOf(w.cfg, w.mgr, "o"); got != levelOff {
		t.Errorf("a slow summary raised an off connection to %d", got)
	}
}

// A connect at off lands with no catalog read and none reading: the
// workspace is on the connection and runs statements, the sidebar's lists
// are empty by choice (CatalogUnread, not CatalogLoading), and a note says
// what fills them. Nothing but a Refresh reads: not a second click on the
// row, not a schema pick, not the relist after a run's DDL.
func TestConnectAtOffReadsNothing(t *testing.T) {
	w := offConn(t)
	ev := w.Switch("other").Job().(*Connected)
	if ev.Err != nil || !ev.Changed || ev.Load != nil || ev.Counts != nil || ev.Routines != nil {
		t.Fatalf("connect at off: %+v", ev)
	}
	if got := noteTexts(ev.Notes); !strings.Contains(got, "connected to other") ||
		!strings.Contains(got, `catalog of other not read (introspect = "off") — a refresh reads it`) {
		t.Errorf("notes = %q", got)
	}
	if !w.Connected() || !w.CatalogUnread() || w.CatalogLoading() || w.Catalog() != nil || w.TableIndex() != nil {
		t.Fatalf("after the connect: connected %v, unread %v, loading %v, catalog %v",
			w.Connected(), w.CatalogUnread(), w.CatalogLoading(), w.Catalog() != nil)
	}
	if res := run(t, w, "SELECT count(*) FROM only_here"); res.Err != nil {
		t.Fatalf("a run on the unread connection: %v", res.Err)
	}

	if st := w.Switch("other"); st.Job != nil {
		t.Error("a second click on the row started a connect: it is not the refresh that reads")
	}
	_, err := w.PickSchema(SchemaPick{Name: "main"})
	refusal(t, err, Invalid)
	if res := run(t, w, "CREATE TABLE made_here (id INTEGER)"); res.Err != nil || res.Relist != nil {
		t.Errorf("DDL on the unread connection: err %v, relist started %v", res.Err, res.Relist != nil)
	}
	if !w.CatalogUnread() {
		t.Error("something read the catalog without a refresh")
	}
}

// The user's Refresh is the read: the tables are listed, the unread state
// ends, and from then on the sidebar behaves as any other — a relist
// follows DDL. A switch away and back starts unread again.
func TestRefreshReadsAnOffConnection(t *testing.T) {
	w := offConn(t)
	if ev := w.Switch("other").Job().(*Connected); ev.Err != nil {
		t.Fatal(ev.Err)
	}
	st, err := w.Refresh()
	if err != nil {
		t.Fatal(err)
	}
	ev := st.Job().(*Connected)
	if ev.Err != nil || ev.Catalog == nil || ev.Status != "refreshed" {
		t.Fatalf("refresh: %+v", ev)
	}
	if w.CatalogUnread() || !hasTable(w, "only_here") {
		t.Fatalf("after the refresh: unread %v, only_here listed %v", w.CatalogUnread(), hasTable(w, "only_here"))
	}
	if strings.Contains(noteTexts(ev.Notes), "not read") {
		t.Errorf("the refresh still says the catalog is not read: %q", noteTexts(ev.Notes))
	}
	res := run(t, w, "CREATE TABLE made_here (id INTEGER)")
	if res.Err != nil || res.Relist == nil {
		t.Fatalf("DDL after the refresh: err %v, relist %v", res.Err, res.Relist != nil)
	}
	if rl := res.Relist().(*Connected); rl.Catalog == nil || !hasTable(w, "made_here") {
		t.Errorf("the relist did not list made_here: %+v", rl)
	}

	if back := landed(w.Switch(demo)); back.Err != nil || w.CatalogUnread() || !hasTable(w, "cats") {
		t.Fatalf("switch to a full connection: %+v, unread %v", back, w.CatalogUnread())
	}
	if again := w.Switch("other").Job().(*Connected); again.Err != nil || again.Load != nil || !w.CatalogUnread() {
		t.Errorf("back on the off connection: %+v, unread %v", again, w.CatalogUnread())
	}
}

// A refresh that read nothing — canceled here before its first list —
// leaves the connection unread, and the next one reads. A Disconnect ends
// the state with the connection.
func TestUnreadSurvivesAFailedRefresh(t *testing.T) {
	w := offConn(t)
	if ev := w.Switch("other").Job().(*Connected); ev.Err != nil {
		t.Fatal(ev.Err)
	}
	st, err := w.Refresh()
	if err != nil {
		t.Fatal(err)
	}
	w.Cancel() // the refresh, before its Job runs: every list then fails on a dead context
	ev := st.Job().(*Connected)
	if ev.Catalog != nil || !w.CatalogUnread() {
		t.Fatalf("a canceled refresh: catalog read %v, unread %v", ev.Catalog != nil, w.CatalogUnread())
	}
	if got := noteTexts(ev.Notes); !strings.Contains(got, "its catalog is still not read") {
		t.Errorf("notes = %q", got)
	}
	// and the next one reads
	if st, err = w.Refresh(); err != nil {
		t.Fatal(err)
	}
	if ev = st.Job().(*Connected); ev.Catalog == nil || w.CatalogUnread() {
		t.Fatalf("the refresh after: %+v, unread %v", ev, w.CatalogUnread())
	}
	if ev := w.Switch(demo).Job().(*Connected); ev.Err != nil {
		t.Fatal(ev.Err)
	}
	if ev := w.Switch("other").Job().(*Connected); ev.Err != nil || !w.CatalogUnread() {
		t.Fatalf("back on other: %+v", ev)
	}
	if _, st, err := w.Disconnect(); err != nil {
		t.Fatal(err)
	} else if st.Job != nil {
		st.Job()
	}
	if w.CatalogUnread() {
		t.Error("unread with no connection")
	}
}

// The pick a connect was asked for is kept while the connection is unread,
// for the Refresh that lists it: a UI that remembers a schema per
// connection must not lose it to the level.
func TestUnreadKeepsTheConnectsPick(t *testing.T) {
	w := offConn(t)
	pick := SchemaPick{Name: "sales"}
	if ev := w.SwitchPick("other", pick).Job().(*Connected); ev.Err != nil {
		t.Fatal(ev.Err)
	}
	w.mu.Lock()
	got := w.refreshPickLocked()
	w.mu.Unlock()
	if got != pick {
		t.Errorf("the refresh would list %+v, want the connect's pick %+v", got, pick)
	}
}

// Completion on an unread connection answers at once with the dialect's
// words and loads nothing; once a Refresh has read the sidebar it loads as
// on any connection.
func TestCompletionAtOff(t *testing.T) {
	w := offConn(t)
	if ev := w.Switch("other").Job().(*Connected); ev.Err != nil {
		t.Fatal(ev.Err)
	}
	buf := "SELECT * FROM "
	r, ready := w.Complete(buf, len(buf))
	if !ready || hasLabel(r, "only_here") {
		t.Fatalf("unread: ready %v, offers only_here %v", ready, hasLabel(r, "only_here"))
	}
	if r, _ = w.Complete("SEL", 3); !hasLabel(r, "SELECT") {
		t.Errorf("unread: the dialect's words are missing: %v", r.Items)
	}
	if err := w.LoadCompletions(context.Background()); err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	loaded := w.compl.conn == "other" && w.compl.schema != nil
	w.mu.Unlock()
	if loaded {
		t.Fatal("LoadCompletions read the schema of an unread connection")
	}

	st, err := w.Refresh()
	if err != nil {
		t.Fatal(err)
	}
	if ev := st.Job().(*Connected); ev.Err != nil || ev.Catalog == nil {
		t.Fatalf("refresh: %+v", ev)
	}
	if _, ready = w.Complete(buf, len(buf)); ready {
		t.Fatal("ready after the refresh with nothing loaded yet")
	}
	if err = w.LoadCompletions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r, ready = w.Complete(buf, len(buf)); !ready || !hasLabel(r, "only_here") {
		t.Errorf("after the refresh and a load: ready %v, items %v", ready, r.Items)
	}
}

// names reads the same as full where there are no schemas to count or to
// scope by: the connect has its Load, the tables are listed, and completion
// reads whole (scope nil) rather than scoped to schemas SQLite has none of.
func TestNamesOnAnEngineWithoutSchemas(t *testing.T) {
	w := newTestWorkspace(t)
	addConn(w, config.Connection{Name: "n", Driver: "sqlite", DSN: memDSN(), Introspect: config.IntrospectNames})
	if _, err := w.mgr.Run("n", "CREATE TABLE t (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	ev := landed(w.Switch("n"))
	if ev.Err != nil || ev.Catalog == nil || w.CatalogUnread() || len(ev.Notes) == 0 || !hasTable(w, "t") {
		t.Fatalf("connect at names: %+v", ev)
	}
	if strings.Contains(noteTexts(ev.Notes), "not read") {
		t.Errorf("names said the catalog is not read: %q", noteTexts(ev.Notes))
	}
	if err := w.LoadCompletions(context.Background()); err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	scope, sc := w.compl.scope, w.compl.schema
	w.mu.Unlock()
	if scope != nil || sc == nil {
		t.Errorf("completion at names on sqlite: scope %v, schema read %v; want a whole read", scope, sc != nil)
	}
}

// ConnCompletions — completion against a connection by name, with no
// sidebar and so no Refresh — reads nothing of an off connection: the
// dialect's words, no tables, and no entry kept, so a level raised later
// reads at the next ask.
func TestConnCompletionsAtOff(t *testing.T) {
	w := offConn(t)
	cc := NewConnCompletions(w.cfg, w.mgr)
	r, err := cc.Complete(context.Background(), "other", "SELECT * FROM ", 14)
	if err != nil || hasLabel(r, "only_here") {
		t.Fatalf("off: err %v, offers only_here %v", err, hasLabel(r, "only_here"))
	}
	if tabs, err := cc.Tables(context.Background(), "other"); err != nil || len(tabs) != 0 {
		t.Errorf("off: tables %v, err %v", tabs, err)
	}
	if cc.schemaOf("other") != nil {
		t.Error("an entry was read for an off connection")
	}

	cn, _ := w.cfg.ConnByName("other")
	cn.Introspect = config.IntrospectFull
	if err = w.cfg.ReplaceConn("other", cn); err != nil {
		t.Fatal(err)
	}
	if r, err = cc.Complete(context.Background(), "other", "SELECT * FROM ", 14); err != nil || !hasLabel(r, "only_here") {
		t.Errorf("raised to full: err %v, items %v", err, r.Items)
	}
}
