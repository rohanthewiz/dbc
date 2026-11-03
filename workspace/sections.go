package workspace

import (
	"context"
	"strings"

	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/db"
)

// The sidebar's three sections — Tables, Views, Routines — each headed and
// foldable, so all three are in sight at once:
//
//	▾ Tables · 34        open as a tab starts
//	  customers
//	  orders
//	▸ Views · 3          folded as a tab starts
//	▸ Routines           folded as a tab starts; unknown count until read
//
// WHAT AN OPEN SECTION COSTS. Tables and views come in the one catalog
// read a connect already makes (db.TablesQuery lists both, typed), so
// folding either is a UI's memory and nothing more: no read runs to open
// one, and none is saved by folding it. Routines are a read of their own
// (pg_proc, information_schema.routines), so their section's fold IS the
// routines switch (ShowRoutines): folded, nothing is read; opening it
// reads them, and every landing after reads them again while it stays
// open. A view's or routine's definition is read only when asked for
// (ViewDDL, RoutineDDL), one at a time.
//
// The folds are the workspace's so each tab (a TUI tab, a dbc web tab)
// keeps its own, as it keeps its row counts' switch.

// Section names one of the sidebar's sections.
type Section string

const (
	SectionTables   Section = "tables"
	SectionViews    Section = "views"
	SectionRoutines Section = "routines"
)

// ParseSection reads a section's name as a page sends it; ok false for
// any other word.
func ParseSection(s string) (Section, bool) {
	switch sec := Section(strings.ToLower(strings.TrimSpace(s))); sec {
	case SectionTables, SectionViews, SectionRoutines:
		return sec, true
	}
	return "", false
}

// SectionOpen reports whether a section is unfolded. Tables start open,
// Views and Routines folded.
func (w *Workspace) SectionOpen(s Section) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	switch s {
	case SectionTables:
		return !w.tablesFolded
	case SectionViews:
		return w.showViews
	case SectionRoutines:
		return w.showRoutines
	}
	return false
}

// OpenSection folds or unfolds a section. Only Routines has anything to
// read: opening it returns ShowRoutines's Job (nil when there is nothing
// to read now); the other two return nil, always.
func (w *Workspace) OpenSection(s Section, open bool) Job {
	switch s {
	case SectionRoutines:
		return w.ShowRoutines(open)
	case SectionTables:
		w.mu.Lock()
		w.tablesFolded = !open
		w.mu.Unlock()
	case SectionViews:
		w.mu.Lock()
		w.showViews = open
		w.mu.Unlock()
	}
	return nil
}

// ViewDDL reads view's definition (db.Manager.ViewDDL) on the active
// connection. The Job's event is a *ViewDDL, whose Err says why there is
// none. Like RoutineDDL it runs on the pool, takes no busy slot and lands
// in no result: it is a look at the catalog, for a viewer.
//
// view must be one the catalog lists as a view: a UI's name is checked
// against it, so a page can ask only for the views its sidebar shows, and
// a table is refused in words rather than sent to a server that would say
// "no such view" less clearly.
func (w *Workspace) ViewDDL(view db.TableRef) (Start, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	name := w.active
	if _, ok := w.cfg.ConnByName(name); !ok || w.catalog == nil {
		return Start{}, refuse(NoConnection, Warn, "no active connection — pick one in the sidebar")
	}
	listed := false
	for _, t := range db.TableRefs(w.catalog.Rows) {
		if t.Schema == view.Schema && t.Name == view.Name {
			if !t.View {
				return Start{}, refuse(Invalid, Warn, "%s is a table, not a view — its columns are what it has", t.Name)
			}
			listed = true
			break
		}
	}
	if !listed {
		return Start{}, refuse(Invalid, Warn, "%s is not in the sidebar's list any more — refresh it", view.Name)
	}
	qname := view.Name
	if view.Schema != "" {
		qname = view.Schema + "." + view.Name
	}
	mgr, budget := w.mgr, w.cfg.CatalogTimeout
	return Start{
		Notes: []Note{notef(Muted, "reading the definition of %s…", qname)},
		Job: func() Event {
			ctx, cancel := withBudget(context.Background(), budget)
			defer cancel()
			ev := &ViewDDL{Conn: name, View: view}
			ev.DDL, ev.Err = mgr.ViewDDL(ctx, name, view)
			if ev.Err != nil {
				ev.Notes = append(ev.Notes, notef(Err, "no definition of %s: %s", qname, serr.StringFromErr(ev.Err)))
			}
			return ev
		},
	}, nil
}
