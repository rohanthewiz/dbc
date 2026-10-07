package workspace

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/model"
)

// catalogTimeout bounds the sidebar's table list — a connect's, or a schema
// pick's: a huge catalog must not hold the connect open.
const catalogTimeout = 10 * time.Second

// The budgets of the two short lists a connect reads before the tables on a
// db.Navigable driver. Each read has its own, rather than all three sharing
// catalogTimeout: on a big production database a slow schema summary used
// to spend the whole budget, and the table list after it then failed at
// once on a context already past its deadline — the sidebar empty, and the
// log blaming the table list for the summary's time.
//
// The summary's is the shorter, and a package var so a test can force the
// fallback: when the summary is slow, the counts are what is slow
// (db.SchemaNamesQuery), and a picker without them is worth more than the
// seconds spent waiting for them.
var (
	navListTimeout       = 5 * time.Second // databases; schema names
	schemaSummaryTimeout = 5 * time.Second // schemas with their table counts
)

// SchemaPick says which tables a sidebar on a db.Navigable driver lists
// (Postgres: see db/navigate.go). The zero value is the default: the first
// schema on search_path, which is what a bare table name means there.
type SchemaPick struct {
	// Name is the schema to list. One the database no longer has falls
	// back to the default rather than to an empty list.
	Name string
	// All lists every schema's tables, where the database has at most
	// db.AllSchemasLimit of them; past that, the default schema, with a
	// note saying why.
	All bool
}

// Switch is picking a connection: a connect with a line for the log, and
// nothing at all when name is already active and its catalog loaded.
func (w *Workspace) Switch(name string) Start {
	return w.SwitchPick(name, w.defaultPick())
}

// SwitchPick is Switch, opening the sidebar on pick: a UI that remembers a
// schema per connection asks for it here, rather than loading the default
// schema's tables only to replace them with a PickSchema at once.
func (w *Workspace) SwitchPick(name string, pick SchemaPick) Start {
	w.mu.Lock()
	same := name == w.active && w.catalog != nil
	w.mu.Unlock()
	if same {
		return Start{}
	}
	st := w.ConnectPick(name, pick)
	if st.Job != nil {
		st.Notes = append([]Note{notef(Info, "connecting to %s…", name)}, st.Notes...)
	}
	return st
}

// defaultPick is the pick a connect that names no schema makes: every
// schema for a UI without a picker (Options.WholeCatalog), else the default.
func (w *Workspace) defaultPick() SchemaPick {
	return SchemaPick{All: w.wholeCatalog}
}

// Connect opens the connection and loads its sidebar, opening on the
// default schema (see SchemaPick, Options.WholeCatalog).
func (w *Workspace) Connect(name string) Start {
	return w.ConnectPick(name, w.defaultPick())
}

// ConnectPick opens the connection and fetches its catalog for a sidebar.
// The catalog goes through the pool, not the pinned session: it is the
// app's query, and must not land inside a transaction the user has open.
//
// On a db.Navigable driver the catalog is three short reads rather than one
// long one — the server's databases, this database's schemas, and the
// tables of the one schema pick names — so a connect to a big server costs
// what its sidebar shows, not what the server holds. Each has its own
// budget (navListTimeout, schemaSummaryTimeout, catalogTimeout), and a
// schema list too slow to count its tables falls back to their names
// (listSchemas). Elsewhere it is the
// whole table list, as it always was — after, on MySQL, the server's
// databases (db.HasDatabases), for the database picker.
//
// The connect runs under its own context, so Cancel can abandon it —
// without that, only connect_timeout bounds it, and with connect_timeout =
// "0" an unreachable host would hold "connecting…" until the OS gave up on
// the TCP connect, over a minute later. Starting a connect cancels any still
// in flight: the user has changed their mind, and the older one must not
// land after the newer one and switch the connection back.
//
//	pick A ──► connGen=1, dial A ───────────── (canceled) ──► lands Stale
//	pick B ──► connGen=2, cancel A, dial B ─► lands, active = B
//
// The Job's event is a *Connected.
func (w *Workspace) ConnectPick(name string, pick SchemaPick) Start {
	if name == "" {
		return Start{}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.connectLocked(name, pick, kindConnect)
}

// connectKind is what a connectLocked is for. The work is the same for
// all three — dial, then read the databases, schemas and tables — and the
// kind decides only how it lands (landConnect): what it says, and whether
// a re-read that could not list the tables keeps the list shown.
type connectKind int

const (
	kindConnect connectKind = iota // a connect proper (ConnectPick)
	kindRefresh                    // the user's re-read of the active connection (Refresh)
	kindRelist                     // the re-read a run's DDL starts (relistLocked): quieter
)

// connectLocked is ConnectPick's work, under mu: it supersedes whatever
// connect is in flight and returns the Job that dials and reads the
// catalog. kind marks a re-read of the active connection (Refresh, or the
// relist after a run's DDL), which lands differently: see landConnect.
//
// It is split out so Refresh can check what is active and start the
// re-read under one hold of mu — released between the two, a connect to
// another connection could start in the gap, and the refresh would then
// cancel it and pull the tab back onto the connection it was leaving.
func (w *Workspace) connectLocked(name string, pick SchemaPick, kind connectKind) Start {
	driver := ""
	if cc, ok := w.cfg.ConnByName(name); ok {
		driver = cc.Driver
	}
	ctx, cancel := context.WithCancel(context.Background())

	if w.connCancel != nil {
		w.connCancel()
	}
	w.cancelCatalogWorkLocked()
	w.connGen++
	gen := w.connGen
	w.connCancel, w.connName = cancel, name

	mgr := w.mgr
	job := func() Event {
		defer cancel() // releases the context; the landing may already have
		ev := &Connected{Name: name}
		if _, err := mgr.DBContext(ctx, name); err != nil {
			ev.Err = err
		} else if _, err := db.TablesQuery(driver); err == nil {
			// A failed list of databases only hides the database picker;
			// a failed list of schemas is handled by listSchemas, then
			// loadTables. ctx (the connect's own) is the parent of each
			// read's budget, so Cancel still stops whichever is running.
			dctx, dcancel := context.WithTimeout(ctx, navListTimeout)
			ev.Databases, _ = mgr.Databases(dctx, name)
			dcancel()
			schemas, notes, serr := listSchemas(ctx, mgr, name)
			ev.Schemas = schemas
			ev.Notes = append(ev.Notes, notes...)
			tctx, tcancel := context.WithTimeout(ctx, catalogTimeout)
			ev.Catalog, ev.Schema, notes = loadTables(tctx, ctx, mgr, name, driver, schemas, serr, pick)
			ev.Notes = append(ev.Notes, notes...)
			tcancel()
		}
		w.landConnect(ev, gen, kind)
		return ev
	}
	return Start{Job: job}
}

// Refresh re-reads the active connection's catalog — the server's
// databases, this database's schemas, and the tables of the schema the
// sidebar lists now — for when another client has created, dropped or
// renamed something since the connect. It is the connect a Switch to the
// connection already active skips (SwitchPick), forced, and differs from
// one only in what it keeps:
//
//   - the session: the connection does not change, so there is no Release
//     and a transaction open on the pinned session stays open;
//   - the schema pick: the sidebar comes back on the schema it lists now
//     (refreshPickLocked), not the default one;
//   - the old list, when the re-read cannot list the tables: an empty
//     sidebar would be no truer than the stale one (landConnect).
//
// The Manager's cached row counts for the connection are dropped too, so a
// sidebar with its counts on counts afresh rather than serving numbers from
// before the change that prompted the refresh. The completion cache goes
// with the landing, as on any connect (landConnect).
//
// It runs as a connect does — on the pool, under connGen, cancelable with
// Cancel — so a connect started after it supersedes it, and it supersedes a
// schema pick or a counting in flight. Refused with no active connection,
// and while a connect is still in flight (the catalog it would re-read is
// about to be replaced).
//
// The Job's event is a *Connected, with Changed false.
func (w *Workspace) Refresh() (Start, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	name := w.active
	switch {
	case w.connCancel != nil:
		return Start{}, refuse(Busy, Warn, "still connecting to %s", w.connName)
	case name == "":
		return Start{}, refuse(NoConnection, Warn, "not connected — nothing to refresh")
	}
	w.mgr.ForgetRowCounts(name)
	st := w.connectLocked(name, w.refreshPickLocked(), kindRefresh)
	st.Notes = append([]Note{notef(Info, "refreshing %s…", name)}, st.Notes...)
	return st, nil
}

// refreshPickLocked is the pick that lists again what the sidebar lists
// now. w.schema "" is ambiguous — every schema's tables picked, or a
// database with one schema (or none, or a driver that is not db.Navigable),
// where every list is "" — so the schema count tells them apart: with
// several, "" was the "all schemas" pick and stays it; otherwise the
// default pick, which on such a database lists "" again. The caller holds mu.
//
// A schema pick still loading is what the sidebar is about to list, so it
// is the one asked for: the re-read cancels the pick's load
// (cancelCatalogWorkLocked), and re-reading w.schema instead would undo
// the user's pick — the relist after a run's DDL can start mid-pick
// without the user having asked for anything.
func (w *Workspace) refreshPickLocked() SchemaPick {
	switch {
	case w.schemaCancel != nil:
		return w.schemaPick
	case w.schema != "":
		return SchemaPick{Name: w.schema}
	case len(w.schemas) > 1:
		return SchemaPick{All: true}
	}
	return w.defaultPick()
}

// cancelCatalogWorkLocked stops the sidebar work a new connect or schema
// pick replaces: the old list's row counting, and a schema load still in
// flight. Both would only land Stale. The caller holds mu.
func (w *Workspace) cancelCatalogWorkLocked() {
	if w.countCancel != nil {
		w.countCancel() // the old sidebar's numbers; see countsJob
		w.countCancel = nil
	}
	if w.schemaCancel != nil {
		w.schemaCancel()
		w.schemaCancel = nil
	}
}

// listSchemas reads a database's schemas for the sidebar's picker: with
// their table counts (db.Manager.SchemaSummary) when that finishes within
// schemaSummaryTimeout, else by name alone (db.Manager.SchemaNames), whose
// Tables are db.TablesUnknown. Only an error from both is returned — and
// then loadTables lists every table, as it did before the fallback.
//
//	summary ──ok──────────────────────────────► schemas with counts
//	   └─fails─► names ──ok─────────────────► schemas, counts unknown (+ note)
//	                └─fails──────────────────► err (loadTables reads whole)
//
// ctx is the connect's; a cancel of it is said elsewhere, so it earns no
// note and no fallback here. A driver that is not db.Navigable gets nil
// from both, at no cost: neither has a query for it.
func listSchemas(ctx context.Context, mgr *db.Manager, name string) ([]db.SchemaInfo, []Note, error) {
	sctx, scancel := context.WithTimeout(ctx, schemaSummaryTimeout)
	schemas, err := mgr.SchemaSummary(sctx, name)
	scancel()
	if err == nil || ctx.Err() != nil {
		return schemas, nil, err
	}
	nctx, ncancel := context.WithTimeout(ctx, navListTimeout)
	defer ncancel()
	names, nerr := mgr.SchemaNames(nctx, name)
	if nerr != nil {
		return nil, nil, err // the summary's error: the one worth reading
	}
	return names, []Note{notef(Warn, "schema table counts unavailable, listing schemas without them: %s",
		serr.StringFromErr(err))}, nil
}

// loadTables reads the tables a sidebar lists — those of the schema pick
// resolves to, or every table on a driver that is not db.Navigable — and
// returns them with that schema and the notes the reading earned. ctx bounds
// the read; outer is the connect's or the pick's own context, which tells a
// cancel (said elsewhere) from a failure (said here).
//
// A schema list that failed (schemasErr) leaves no schema to pick, so the
// whole catalog is read instead, as before the sidebar was loaded by
// schema: a long list beats an empty one.
//
// A table list that fails, or comes back cut at the catalog bound, still
// lets a connect land — the connection works; only the sidebar is short —
// but says so in the log.
func loadTables(ctx, outer context.Context, mgr *db.Manager, name, driver string,
	schemas []db.SchemaInfo, schemasErr error, pick SchemaPick) (*model.Result, string, []Note) {
	var notes []Note
	schema := ""
	if db.Navigable(driver) {
		if schemasErr == nil {
			schema, notes = resolvePick(schemas, pick)
		} else if outer.Err() == nil {
			notes = append(notes, notef(Warn, "schema list unavailable, listing every table: %s", serr.StringFromErr(schemasErr)))
		}
	}
	cat, err := mgr.Catalog(ctx, name, schema)
	switch {
	case err != nil && outer.Err() == nil:
		notes = append(notes, notef(Warn, "tables list unavailable: %s", serr.StringFromErr(err)))
	case err == nil && cat.Truncated:
		notes = append(notes, notef(Warn, "tables list cut at %d tables", len(cat.Rows)))
	}
	if err != nil {
		return nil, schema, notes
	}
	return cat, schema, notes
}

// resolvePick turns a pick into the schema to list, "" for every one:
//
//	All, and the database's tables ≤ AllSchemasLimit ─► "" (every schema)
//	  (never when a count is db.TablesUnknown: a database too big to
//	  count in time is no database to read whole)
//	Name, and the database has that schema ──────────► Name
//	otherwise ─► the default (defaultSchema): the search_path's first
//	             schema if it has tables, else the first schema that does
//
// A database with one schema (or none) lists "" — every schema is that one,
// and a UI then has no picker to show. The notes explain a pick that could
// not be honoured, so a sidebar showing one schema of many is never a
// surprise.
func resolvePick(schemas []db.SchemaInfo, pick SchemaPick) (string, []Note) {
	if len(schemas) <= 1 {
		return "", nil
	}
	total, counted := 0, true
	for _, s := range schemas {
		if s.Tables == db.TablesUnknown {
			counted = false
		}
		total += s.Tables
	}
	if pick.All && counted && total <= db.AllSchemasLimit {
		return "", nil
	}
	if pick.Name != "" && !pick.All {
		for _, s := range schemas {
			if s.Name == pick.Name {
				return s.Name, nil
			}
		}
	}
	def := defaultSchema(schemas)
	switch {
	case pick.All && !counted:
		return def, []Note{notef(Info, "%d schemas whose tables could not be counted, too many to chance at once: listing %s", len(schemas), def)}
	case pick.All:
		return def, []Note{notef(Info, "%d tables in %d schemas, too many to list at once: listing %s", total, len(schemas), def)}
	case pick.Name != "":
		return def, []Note{notef(Info, "no schema %s here any more: listing %s", pick.Name, def)}
	}
	return def, nil
}

// defaultSchema is the schema a sidebar opens on: the first on search_path
// (what a bare table name resolves to) if it has tables in it, else the
// first schema that does, else the search_path's, else the first. An empty
// public is common on a shared server whose tables all live in named
// schemas, and opening on an empty list there would only make the user
// pick again. schemas is not empty.
//
// A count of db.TablesUnknown is not known to be empty, so the search_path's
// schema is taken with one, as with tables: by name alone, it is the best
// guess there is.
func defaultSchema(schemas []db.SchemaInfo) string {
	path, withTables := "", ""
	for _, s := range schemas {
		if s.Default {
			if s.Tables > 0 || s.Tables == db.TablesUnknown {
				return s.Name
			}
			path = s.Name
		}
		if withTables == "" && s.Tables > 0 {
			withTables = s.Name
		}
	}
	switch {
	case withTables != "":
		return withTables
	case path != "":
		return path
	}
	return schemas[0].Name
}

// landConnect installs a connect's outcome, unless a newer connect has
// superseded it.
//
// A refresh (Workspace.Refresh) lands as a connect back to the same
// connection, with two differences. Its words say refresh, since nothing
// was connected to. And a re-read that could not list the tables lands
// nothing — not the databases and schemas it did read either, so the
// sidebar stays one consistent, if stale, picture rather than new schema
// counts over an old schema's tables:
//
//	refresh lands ─┬─ canceled / failed ──────────► notes only (as a connect)
//	               ├─ no tables, a list shown ────► notes only; old list stays
//	               └─ tables (or none ever shown) ► installed as a connect's,
//	                                                 + "refreshed …" note
//
// "A list shown" matters for a driver with no catalog query at all
// (db.TablesQuery fails): its Catalog is always nil, and its refresh is
// then only the re-dial, landing as a connect does.
//
// A relist (kindRelist, after a run's DDL: Workspace.relistLocked) lands
// as a refresh does, in its own words ("relisted"), and leaves the status
// bar alone: the user asked for the run, not for this, and the bar's
// summary of the run's result must not be replaced by the re-read's.
func (w *Workspace) landConnect(ev *Connected, gen int, kind connectKind) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if gen != w.connGen {
		ev.Stale = true // superseded by a later connect, which canceled this one
		return
	}
	w.connCancel = nil
	// the re-read's noun for the words below, and its status setter: a
	// relist says nothing to the status bar (see above)
	reread := kind != kindConnect
	noun := "refresh"
	status := func(s string) { ev.Status = s }
	if kind == kindRelist {
		noun = "relist"
		status = func(string) {}
	}
	if errors.Is(ev.Err, db.ErrCanceled) {
		if reread {
			ev.Notes = append(ev.Notes, notef(Warn, "%s of %s canceled", noun, ev.Name))
			status(noun + " canceled")
			return
		}
		ev.Notes = append(ev.Notes, notef(Warn, "connect to %s canceled", ev.Name))
		ev.Status = "connect canceled"
		return
	}
	if ev.Err != nil {
		if reread {
			ev.Notes = append(ev.Notes, notef(Err, "%s of %s failed: %s", noun, ev.Name, serr.StringFromErr(ev.Err)))
			return
		}
		ev.Notes = append(ev.Notes, notef(Err, "connect failed: %s", serr.StringFromErr(ev.Err)))
		return
	}
	if reread && ev.Catalog == nil && w.catalog != nil {
		// loadTables has already said why the tables could not be read;
		// this says what the user is looking at meanwhile
		ev.Notes = append(ev.Notes, notef(Warn, "%s not %sed: the tables listed before stay", ev.Name, noun))
		status(noun + " failed")
		return
	}
	ev.Changed = ev.Name != w.active
	if ev.Changed {
		ev.Left = w.active
	}
	w.active = ev.Name
	// the levels above go in first: the catalog's index reads the schemas
	w.databases, w.schemas, w.schema = ev.Databases, ev.Schemas, ev.Schema
	w.setCatalogLocked(ev.Catalog)
	// a connect — even back to the same connection — reads the schema
	// afresh for completion: it is how a user gets past a remembered
	// failed load, or sees what another client changed. A schema pick
	// does not (PickSchema): see complete.go.
	w.dropCompletionsLocked()
	if ev.Catalog != nil {
		ev.Counts = w.countsJobLocked(ev.Name, gen, db.TableRefs(ev.Catalog.Rows))
	}
	if ev.Changed {
		ev.Notes = append(ev.Notes, notef(Ok, "connected to %s", ev.Name))
		ev.Status = "connected"
		// issued only after active has moved to Name, so a run started in
		// the meantime is already on Name and its session is left alone
		ev.Release = w.releaseJob(ev.Name)
	}
	switch kind {
	case kindRefresh:
		ev.Notes = append(ev.Notes, notef(Ok, "refreshed %s%s", ev.Name, refreshedWhat(ev)))
		ev.Status = "refreshed"
	case kindRelist:
		ev.Notes = append(ev.Notes, notef(Info, "relisted %s after the DDL%s", ev.Name, refreshedWhat(ev)))
		ev.Relisted = true
	}
}

// refreshedWhat is the tail of a refresh's "refreshed <conn>" note: what
// the sidebar now lists, so the user can see at a glance whether the table
// they were expecting arrived. Empty when there is no list (a driver
// without a catalog query).
//
//	": 12 tables"                          one schema, or a whole catalog
//	": 12 tables in sales (of 4 schemas)"  a navigable database, one schema
//	": 40 tables in 4 schemas"             the same, every schema listed
func refreshedWhat(ev *Connected) string {
	if ev.Catalog == nil {
		return ""
	}
	s := ": " + countOf(len(ev.Catalog.Rows), "table")
	switch n := len(ev.Schemas); {
	case n > 1 && ev.Schema != "":
		s += " in " + ev.Schema + " (of " + countOf(n, "schema") + ")"
	case n > 1:
		s += " in " + countOf(n, "schema")
	}
	return s // a list cut at the catalog bound has loadTables's note already
}

// countOf is "1 table", "2 tables".
func countOf(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// setCatalogLocked installs a connection's catalog and its index. Row counts
// belong to the catalog they were counted for, so they go with it.
//
// The index is told the database's whole schema list (w.schemas, which the
// caller sets first), since the catalog may hold one schema's tables: names
// are then qualified as on a many-schema database, and a schema.table in a
// question can reach a schema the sidebar has not loaded (TableIndex.SetSchemas).
func (w *Workspace) setCatalogLocked(tables *model.Result) {
	w.catalog, w.tableIdx, w.rowCounts = tables, nil, nil
	if tables != nil {
		w.tableIdx = db.NewTableIndex(db.TableRefs(tables.Rows))
		if len(w.schemas) > 0 {
			names := make([]string, len(w.schemas))
			for i, s := range w.schemas {
				names[i] = s.Name
			}
			w.tableIdx.SetSchemas(names)
		}
	}
}

// PickSchema lists another schema's tables in the sidebar, on the active
// connection, in place of the ones listed now. The tables load off the UI's
// event loop (the Job), like a connect's; the event is a *SchemaLoaded, with
// a Counts job for the new tables' row counts.
//
// It shares connGen with Connect, so whichever of a pick and a connect
// starts later wins: a connect supersedes a pick in flight (another
// database's schemas are not this one's), and a pick supersedes the old
// list's counting and any earlier pick. A pick while a connect is still in
// flight is refused — the schema list it would pick from is being replaced.
//
//	pick sales ──► gen=5, load sales ───────────── (canceled) ─► Stale
//	pick hr    ──► gen=6, cancel sales, load hr ─► lands, schema = hr
func (w *Workspace) PickSchema(pick SchemaPick) (Start, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	name := w.active
	cc, ok := w.cfg.ConnByName(name)
	switch {
	case !ok:
		return Start{}, refuse(NoConnection, Warn, "no active connection — pick one in the sidebar")
	case w.connCancel != nil:
		return Start{}, refuse(Busy, Warn, "still connecting to %s", w.connName)
	case !db.Navigable(cc.Driver):
		return Start{}, refuse(Invalid, Warn, "%s lists its tables whole, not by schema", name)
	}
	w.cancelCatalogWorkLocked()
	w.connGen++
	gen := w.connGen
	ctx, cancel := context.WithCancel(context.Background())
	w.schemaCancel, w.schemaPick = cancel, pick
	schemas, mgr, driver := w.schemas, w.mgr, cc.Driver

	job := func() Event {
		defer cancel()
		tctx, tcancel := context.WithTimeout(ctx, catalogTimeout)
		cat, schema, notes := loadTables(tctx, ctx, mgr, name, driver, schemas, nil, pick)
		tcancel()
		w.mu.Lock()
		defer w.mu.Unlock()
		ev := &SchemaLoaded{Conn: name, Schema: schema, Catalog: cat, Notes: notes}
		if gen != w.connGen {
			ev.Stale = true
			return ev
		}
		w.schemaCancel = nil
		if cat == nil {
			// the old list stays up, and so does the schema it is of: the
			// picker must not claim a schema whose tables are not shown
			ev.Schema = w.schema
			return ev
		}
		w.schema = schema
		// the completion cache stays: it holds every schema already
		// (complete.go), and the new w.schema re-ranks it on the next ask
		w.setCatalogLocked(cat)
		ev.Counts = w.countsJobLocked(name, gen, db.TableRefs(cat.Rows))
		return ev
	}
	return Start{Job: job}, nil
}

// countsJobLocked makes the Job that counts the rows of the tables of the
// connect gen landed, for the sidebar. The counting itself (exact or
// estimated, cached for a couple of minutes, shared by every workspace on
// the Manager) is db.Manager.RowCounts's.
//
// Its context is the workspace's to cancel: the next connect does, since
// the counts would only be dropped as stale when they landed, and a counting
// can hold pool connections for up to its budget. A counting canceled that
// way is not cached, so switching straight back counts again rather than
// finding half the numbers.
//
// A run that changed rows starts a recount of the same sidebar
// (recountLocked), which cancels this one: countGen tells that canceled
// counting, which has the same connGen, to land Stale rather than as
// "row counts unavailable".
//
// With the sidebar's counts off (ShowRowCounts, the default) it makes no
// Job and returns nil, which every caller already reads as "no counting":
// the one switch gates the connect's, the schema pick's and the recount's.
//
//	connect lands ─► Counts job ─► mgr.RowCounts ─► lands under mu
//	                                                 ├─ gen and countGen current → rowCounts set
//	                                                 └─ superseded               → Stale
func (w *Workspace) countsJobLocked(name string, gen int, tables []db.TableRef) Job {
	if !w.showCounts {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.countCancel = cancel
	w.countGen++
	cg := w.countGen
	mgr := w.mgr
	return func() Event {
		defer cancel()
		counts, err := mgr.RowCounts(ctx, name, tables)
		w.mu.Lock()
		defer w.mu.Unlock()
		ev := &RowCounts{Conn: name}
		if gen != w.connGen || cg != w.countGen {
			ev.Stale = true // a later connect, or recount, owns the sidebar now
			return ev
		}
		w.countCancel = nil
		if err != nil {
			// the list is still there, just without numbers: background
			// detail, not a failure worth a warning
			ev.Notes = append(ev.Notes, notef(Muted, "row counts unavailable: %s", serr.StringFromErr(err)))
			return ev
		}
		w.rowCounts, ev.Counts = counts, counts
		return ev
	}
}

// CancelConnect abandons the connect in flight. It reports false when there
// is none. The connect still lands, as canceled.
func (w *Workspace) CancelConnect() (Note, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cancelConnectLocked()
}

func (w *Workspace) cancelConnectLocked() (Note, bool) {
	if w.connCancel == nil {
		return Note{}, false
	}
	w.connCancel()
	return notef(Warn, "canceling connect to %s…", w.connName), true
}

// Disconnect leaves the active connection without picking another: no
// active connection, no catalog, and — through the Job — the pinned session
// closed, rolling back whatever it left open, as a switch does. A connect
// in flight is abandoned with it (connGen moves on, so it lands Stale), as
// is the sidebar's counting and schema loading. It returns the connection
// left: on a connect still dialing, the one being dialed when there was no
// connection before it.
//
// A run in flight is refused rather than canceled: its session is the one
// the disconnect would close, and a Stop first leaves the user to decide
// what becomes of the statement. Nothing to disconnect from is refused too.
//
// The pool is not this workspace's to close — other workspaces may be on
// the same connection (db.Manager is shared); a UI that knows they are not
// closes it with db.Manager.Disconnect once the Job has run.
//
// The Job's event is a *SessionReleased, or nil when no session was open.
func (w *Workspace) Disconnect() (left string, st Start, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	left = w.active
	dialing := w.connCancel != nil
	if left == "" && dialing {
		left = w.connName
	}
	switch {
	case left == "":
		return "", Start{}, refuse(NoConnection, Warn, "not connected")
	case w.busy:
		return "", Start{}, refuse(Busy, Warn, "busy — %s is still running (Ctrl+K stops it, then disconnect)", w.runTag)
	}
	if dialing {
		w.connCancel()
		w.connCancel = nil
	}
	w.cancelCatalogWorkLocked()
	w.connGen++ // anything of the old connection's still landing is Stale
	w.active, w.databases, w.schemas, w.schema = "", nil, nil, ""
	w.setCatalogLocked(nil)
	w.dropCompletionsLocked()
	return left, Start{
		Notes: []Note{notef(Ok, "disconnected from %s", left)},
		// Whatever session is open goes — unless, by the time the Job runs,
		// a connect has landed and a run has pinned one to the new active
		// connection: that one is the user's next work, not the old's.
		// active is read here rather than passed as "" for that reason.
		Job: func() Event {
			w.mu.Lock()
			keep := w.active
			w.mu.Unlock()
			return w.releaseJob(keep)()
		},
	}, nil
}

// Derived reports whether name is a connection derived onto another of
// its base's databases ("<conn>/<database>", config.DatabaseSep) rather
// than a configured one.
//
// It is what a UI asks of Connected.Left before closing the pool a switch
// left. A derived pool is the one worth closing: browsing a server's
// databases opens one per database picked, and without a close each stays
// open (its *sql.DB until dbc exits, its idle connections until
// conn_idle_timeout), so the count grows with the browsing rather than with
// the configured connections. A configured connection's pool is kept, as
// it always was: there is one per row of the Connections list, and
// switching back to it should not dial again.
func (w *Workspace) Derived(name string) bool {
	cc, ok := w.cfg.ConnByName(name)
	return ok && cc.Base != ""
}

// releaseJob closes the session pinned to any connection other than keep, so
// switching connections ends the old session — rolling back what it left
// open — then and there, rather than holding its transaction and locks open
// until the next run happens to replace it.
//
// It is a Job, off the UI goroutine, because it takes sessMu, which a run in
// flight holds for as long as its statement takes.
func (w *Workspace) releaseJob(keep string) Job {
	return func() Event {
		w.sessMu.Lock()
		defer w.sessMu.Unlock()
		if w.sess == nil || w.sessFor == keep {
			return nil
		}
		ev := &SessionReleased{Conn: w.sessFor, Stateful: w.sess.Stateful()}
		// A session that only ever ran queries goes quietly: nothing was lost.
		if ev.Stateful {
			ev.Notes = []Note{notef(Warn,
				"left %s: its session was closed — any open transaction was rolled back, SET values and temp tables are gone",
				ev.Conn)}
		}
		w.dropSessionLocked()
		return ev
	}
}
