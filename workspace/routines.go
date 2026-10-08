package workspace

import (
	"context"

	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/model"
)

// The sidebar's routines list: the stored functions and procedures of the
// schema whose tables the sidebar lists, beside them, and each one's DDL.
//
// It follows the row counts' pattern (ShowRowCounts, countsJobLocked)
// rather than riding inside the catalog read, for the same two reasons:
// the tables must draw without waiting on it, and it is off until asked
// for, so a sidebar showing tables costs no pg_proc read. So each landing
// that installs a catalog — a connect, a Refresh, a relist after DDL, a
// schema pick — makes a Routines job while the switch is on:
//
//	ShowRoutines(true) ──► Routines job for the catalog listed now
//	connect / pick lands ─► setCatalogLocked (routines nil)
//	                    └─► Routines job ─► mgr.Routines(schema) ─► *RoutinesLoaded
//	                                           lands only while connGen and
//	                                           routineGen are what it was made
//	                                           under; else Stale
//
// The schema is the catalog's (w.schema): one schema's routines beside
// one schema's tables, every schema's beside every schema's. MySQL lists
// its database's, as its tables are listed whole.
//
// routines nil means "not read (yet)"; a read that found none, or failed,
// leaves an empty list — a UI then says "no routines" rather than
// "loading…" forever, and the failure's note says why.
//
// A routine's DDL (RoutineDDL) is a Job of its own, on the pool, which
// neither takes the run's busy slot nor lands in the results: it is a
// look at the catalog, as the list is, and a UI shows it in a viewer.

// RoutinesShown reports whether the sidebar's routines list is on
// (ShowRoutines); it starts off.
func (w *Workspace) RoutinesShown() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.showRoutines
}

// Routines is the stored functions and procedures of the schema the
// catalog lists, in the catalog's order (schema, then name); nil until
// their read lands, or while the list is off. The slice is shared: read
// it, never write to it.
func (w *Workspace) Routines() []model.Routine {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.routines
}

// ShowRoutines turns the sidebar's routines list on or off. On, it
// returns the Job that reads the routines of the catalog listed now, whose
// event is a *RoutinesLoaded; nil when nothing is listed yet (the next
// connect or schema pick reads them, the switch being on) or the driver
// stores none (db.HasRoutines). From then on every catalog landing reads
// them again. Off drops them and makes a read in flight land Stale.
//
//	off ──ShowRoutines(true)──► on ─► Routines job ─► *RoutinesLoaded
//	on  ──ShowRoutines(false)─► off: routines nil, in-flight → Stale
func (w *Workspace) ShowRoutines(on bool) Job {
	w.mu.Lock()
	defer w.mu.Unlock()
	if on == w.showRoutines {
		return nil // a repeat: the read already ran, or runs
	}
	w.showRoutines = on
	if !on {
		w.cancelRoutinesLocked()
		w.routineGen++ // a read still on its way lands Stale
		w.routines = nil
		return nil
	}
	if w.active == "" || w.catalog == nil {
		return nil
	}
	return w.routinesJobLocked(w.active, w.connGen)
}

// cancelRoutinesLocked stops the routines read in flight, if any. The
// caller holds mu.
func (w *Workspace) cancelRoutinesLocked() {
	if w.routineCancel != nil {
		w.routineCancel()
		w.routineCancel = nil
	}
}

// routinesJobLocked makes the Job that reads the routines of the catalog
// the landing of gen installed — w.schema's, or every schema's — for the
// sidebar. nil while the list is off, and on a driver without routines.
// The caller holds mu, and has installed the catalog.
//
// A job made supersedes the one before it (routineGen), which is
// canceled: a schema picked twice quickly must not land the first
// schema's routines after the second's. gen guards the other direction,
// a connect or pick that lands after this job was made.
func (w *Workspace) routinesJobLocked(name string, gen int) Job {
	if !w.showRoutines {
		return nil
	}
	if cc, ok := w.cfg.ConnByName(name); !ok || !db.HasRoutines(cc.Driver) {
		// nothing to read, so nothing will land: the list is read — and
		// empty — now, or a UI left on routines from another connection
		// would say "loading…" for good
		w.routines = []model.Routine{}
		return nil
	}
	w.cancelRoutinesLocked()
	ctx, cancel := context.WithCancel(context.Background())
	w.routineCancel = cancel
	w.routineGen++
	rg := w.routineGen
	schema, mgr := w.schema, w.mgr
	var scope []string // nil: every user schema (Manager.Routines)
	if schema != "" {
		scope = []string{schema}
	}
	return func() Event {
		defer cancel()
		// the tables' budget: the read is one pass over pg_proc (or
		// information_schema.routines), no larger than the table list's
		rctx, rcancel := context.WithTimeout(ctx, catalogTimeout)
		routines, err := mgr.Routines(rctx, name, scope)
		rcancel()
		w.mu.Lock()
		defer w.mu.Unlock()
		ev := &RoutinesLoaded{Conn: name, Schema: schema}
		if gen != w.connGen || rg != w.routineGen {
			ev.Stale = true // a later landing, or the switch going off, owns the list
			return ev
		}
		w.routineCancel = nil
		if err != nil {
			ev.Notes = append(ev.Notes, notef(Warn, "routines list unavailable: %s", serr.StringFromErr(err)))
		}
		if routines == nil {
			routines = []model.Routine{} // read: "none", not "not yet" (see above)
		}
		w.routines, ev.Routines = routines, routines
		return ev
	}
}

// RoutineDDL reads r's definition — the CREATE statement its server
// renders (db.Manager.RoutineDDL) — on the active connection. The Job's
// event is a *RoutineDDL, whose Err says why there is none.
//
// r is a routine of the sidebar's list (Routines), or one a UI rebuilt
// from it: on Postgres its ID (the pg_proc oid) is what names it, so an
// overload is never mistaken for another.
func (w *Workspace) RoutineDDL(r model.Routine) (Start, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	name := w.active
	cc, ok := w.cfg.ConnByName(name)
	switch {
	case !ok:
		return Start{}, refuse(NoConnection, Warn, "no active connection — pick one in the sidebar")
	case !db.HasRoutines(cc.Driver):
		return Start{}, refuse(Invalid, Warn, "%s stores no routines", name)
	case r.Name == "":
		return Start{}, refuse(Invalid, Warn, "no routine named")
	}
	mgr := w.mgr
	return Start{
		Notes: []Note{notef(Muted, "reading the definition of %s…", r.QName())},
		Job: func() Event {
			ctx, cancel := context.WithTimeout(context.Background(), catalogTimeout)
			defer cancel()
			ev := &RoutineDDL{Conn: name, Routine: r}
			ev.DDL, ev.Err = mgr.RoutineDDL(ctx, name, r)
			if ev.Err != nil {
				ev.Notes = append(ev.Notes, notef(Err, "no definition of %s: %s", r.QName(), serr.StringFromErr(ev.Err)))
			}
			return ev
		},
	}, nil
}
