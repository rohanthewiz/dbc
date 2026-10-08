package web

import (
	"github.com/rohanthewiz/rweb"

	"github.com/rohanthewiz/dbc/model"
	"github.com/rohanthewiz/dbc/workspace"
)

// The sidebar's routines list and each routine's DDL — the TUI's f and
// Enter (tui/routines.go), on the same workspace calls:
//
//	"Routines" tab ──POST …/routines {on}──► Workspace.ShowRoutines
//	                ◄── "routines" event: the list (null while it loads)
//	connect / refresh / schema pick lands ─► its Routines job ─► "routines"
//	a routine's Enter ──POST …/ddl {routine}──► Workspace.RoutineDDL
//	                ◄── "ddl" event: the statement, for the page's viewer
//
// The list rides in the sidebar's state too (sideState.Routines), so a
// "conn" event — which redraws the whole sidebar — never draws a stale
// one; "routines" is what fills it in after, as "counts" does the row
// counts.

// routineRef is one routine as the sidebar lists it. QName is the name to
// put in SQL, qualified on the tables' rule (see qualifyRoutines); Kind is
// model.RoutineKind's word. ID is the server's handle on one overload
// (Postgres's oid), sent back as-is to ask for its DDL.
type routineRef struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
	QName  string `json:"qname"`
	Kind   string `json:"kind"`
	Args   string `json:"args"`
	Result string `json:"result,omitempty"`
	ID     string `json:"id,omitempty"`
	// Overloaded marks a name shared with another routine listed, so the
	// page shows each one's arguments to tell them apart.
	Overloaded bool `json:"overloaded,omitempty"`
}

// routines is the tab's routines list as sidebar rows: nil (JSON null)
// while they are off or still loading, an empty list when there are none —
// the page says "loading…" for one and "no routines" for the other.
//
// A name is qualified by its schema when the database has several (the
// tables' rule: a routine of one schema is still called by schema.name
// from off the search_path) or when the routines listed span several.
func routines(ws *workspace.Workspace) []routineRef {
	rs := ws.Routines()
	if rs == nil {
		return nil
	}
	qualify := len(ws.Schemas()) > 1
	for _, r := range rs {
		if r.Schema != rs[0].Schema {
			qualify = true
			break
		}
	}
	seen := map[string]int{}
	for _, r := range rs {
		seen[r.QName()]++
	}
	out := make([]routineRef, len(rs))
	for i, r := range rs {
		q := r.Name
		if qualify {
			q = r.QName()
		}
		out[i] = routineRef{Schema: r.Schema, Name: r.Name, QName: q, Kind: string(r.Kind),
			Args: r.Args, Result: r.Result, ID: r.ID, Overloaded: seen[r.QName()] > 1}
	}
	return out
}

// routinesEvent brings the routines list, after the "conn" that drew the
// tables, and whenever the switch is turned. Active lets the page drop a
// list for a connection it has since left; On is the tab's switch as it
// stands (Workspace.RoutinesShown).
type routinesEvent struct {
	Active   string       `json:"active"`
	Routines []routineRef `json:"routines"`
	On       bool         `json:"on"`
}

func routinesEventOf(t *tab) routinesEvent {
	return routinesEvent{Active: t.ws.Active(), Routines: routines(t.ws), On: t.ws.RoutinesShown()}
}

// routinesReq is the sidebar's Tables / Routines switch.
type routinesReq struct {
	On bool `json:"on"`
}

// handleRoutines turns the tab's routines list on or off
// (Workspace.ShowRoutines). On, the read runs in the background and lands
// as a "routines" event; off, or on with nothing to read, the event goes
// out now, so the page never waits on one that is not coming.
func (s *Server) handleRoutines(ctx rweb.Context) error {
	t, err := s.hub.get(ctx.Request().PathParam("id"))
	if err != nil {
		return fail(ctx, err)
	}
	var req routinesReq
	if err = decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	loading := false
	if job := t.ws.ShowRoutines(req.On); job != nil {
		loading = true
		go func() { s.deliver(t, job()) }()
	} else {
		t.send("routines", routinesEventOf(t))
	}
	return ok(ctx, map[string]any{"on": req.On, "loading": loading})
}

// ddlReq names a routine of the sidebar's list, by the fields its row
// carries.
type ddlReq struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Args   string `json:"args"`
	ID     string `json:"id"`
}

// handleRoutineDDL reads a listed routine's definition
// (Workspace.RoutineDDL). It answers at once; the statement arrives as a
// "ddl" event, and a failure as a log line.
//
// The routine must be one the tab lists: the request is matched against
// Workspace.Routines rather than trusted, so a page can only ask for the
// definitions its sidebar shows — and a list gone stale (another client
// dropped the routine) is told to refresh rather than read by an oid that
// may now be something else.
func (s *Server) handleRoutineDDL(ctx rweb.Context) error {
	t, err := s.hub.get(ctx.Request().PathParam("id"))
	if err != nil {
		return fail(ctx, err)
	}
	var req ddlReq
	if err = decode(ctx, &req); err != nil {
		return fail(ctx, err)
	}
	var r model.Routine
	found := false
	for _, x := range t.ws.Routines() {
		if x.ID == req.ID && x.Schema == req.Schema && x.Name == req.Name && string(x.Kind) == req.Kind && x.Args == req.Args {
			r, found = x, true
			break
		}
	}
	if !found {
		// 409: the page's list and the tab's disagree, which a refresh settles
		return fail(ctx, conflict("%s is not in the routines list any more — refresh the sidebar", req.Name))
	}
	st, err := t.ws.RoutineDDL(r)
	if err != nil {
		if rf, isRefusal := asRefusal(err); isRefusal {
			t.notes([]workspace.Note{rf.Note})
		}
		return fail(ctx, err)
	}
	s.launch(t, st)
	return ok(ctx, map[string]any{"loading": true})
}

// ddlEvent is a routine's definition, for the page's viewer.
type ddlEvent struct {
	Conn    string     `json:"conn"`
	Routine routineRef `json:"routine"`
	DDL     string     `json:"ddl"`
}
