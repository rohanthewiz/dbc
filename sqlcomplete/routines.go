package sqlcomplete

import (
	"slices"
	"strings"

	"github.com/rohanthewiz/dbc/model"
)

// The database's own functions and procedures (Request.Routines), offered
// beside the dialect's built-in vocabulary.
//
// WHERE EACH IS OFFERED. A routine is offered where the statement can use
// it, which is a matter of its kind:
//
//	context                          offered                         inserted as
//	───────────────────────────────  ──────────────────────────────  ─────────────
//	an expression (SELECT ▮, = ▮)     functions, aggregates, windows  ref(▮)
//	FROM ▮ · JOIN ▮                   set-returning functions         ref(▮)
//	CALL ▮                            procedures                      ref(▮)
//	FUNCTION ▮ (DROP, ALTER, COMMENT  every non-procedure, triggers   ref
//	  ON …, CREATE OR REPLACE …)      included
//	EXECUTE FUNCTION ▮ (a trigger's)  trigger functions first         ref()
//	PROCEDURE ▮                       procedures                      ref
//	ROUTINE ▮                         every routine                   ref
//	schema.▮                          that schema's, by the context   name…
//	                                  before the qualifier
//
// ref is the name, schema-qualified when the bare name would not find it
// (routineBare). The caret lands between the parentheses, or after them
// for a routine no overload of which takes an argument; with a "(" already
// after the caret, the name goes in alone.
//
// OVERLOADS. Postgres allows several routines of one name in one schema,
// differing by argument types. They are one suggestion — what is inserted
// is the same name — whose detail is the first signature with a count of
// the others, and whose documentation lists every signature.

// rgroup is the routines of one name in one schema: one suggestion.
type rgroup struct {
	schema, name string
	overloads    []model.Routine
}

// kind is the group's kind, for the icon and filtering: a procedure when
// any overload is one (Postgres keeps functions and procedures in one
// namespace, so a mixed group is a curiosity), else the first overload's.
func (g *rgroup) kind() model.RoutineKind {
	for _, r := range g.overloads {
		if r.Kind == model.RoutineProcedure {
			return r.Kind
		}
	}
	return g.overloads[0].Kind
}

// any reports whether some overload satisfies ok.
func (g *rgroup) any(ok func(model.Routine) bool) bool {
	return slices.ContainsFunc(g.overloads, ok)
}

// setReturning reports whether a function returns rows, which FROM can
// read: Postgres's SETOF t and TABLE(…) results.
func setReturning(r model.Routine) bool {
	return r.Kind == model.RoutineFunction &&
		(strings.HasPrefix(r.Result, "SETOF ") || strings.HasPrefix(r.Result, "TABLE("))
}

// indexRoutines groups the request's routines by schema and name, in their
// order, and records the schemas that hold them. It runs in index, after
// the tables are indexed.
func (c *completer) indexRoutines() {
	c.rbySchema = map[string][]*rgroup{}
	byKey := map[string]*rgroup{}
	for _, r := range c.req.Routines {
		key := r.Schema + "\x00" + r.Name
		g := byKey[key]
		if g == nil {
			g = &rgroup{schema: r.Schema, name: r.Name}
			byKey[key] = g
			c.routines = append(c.routines, g)
			s := strings.ToLower(r.Schema)
			c.rbySchema[s] = append(c.rbySchema[s], g)
		}
		g.overloads = append(g.overloads, r)
	}
}

// routineFilter picks the routines a context can use: whether to offer g,
// and at what rank relative to the call's base rank (0 or 1).
type routineFilter func(g *rgroup) (offer bool, rankUp int)

// callable keeps the routines an expression can call.
func callable(g *rgroup) (bool, int) { return g.any(model.Routine.Callable), 0 }

// procedures keeps the procedures, for CALL and PROCEDURE.
func procedures(g *rgroup) (bool, int) { return g.kind() == model.RoutineProcedure, 0 }

// rowSources keeps the set-returning functions, for FROM.
func rowSources(g *rgroup) (bool, int) { return g.any(setReturning), 0 }

// anyRoutine keeps every routine, for ROUTINE.
func anyRoutine(*rgroup) (bool, int) { return true, 0 }

// notProcedures keeps what FUNCTION names: functions of every kind,
// triggers included.
func notProcedures(g *rgroup) (bool, int) { return g.kind() != model.RoutineProcedure, 0 }

// triggersFirst keeps what EXECUTE FUNCTION names — a trigger function —
// offering the other functions a rank later, since a trigger function's
// result type is the only thing that marks it, and a catalog read on an
// old server may not have.
func triggersFirst(g *rgroup) (bool, int) {
	switch g.kind() {
	case model.RoutineTrigger:
		return true, 0
	case model.RoutineProcedure:
		return false, 0
	}
	return true, 1
}

// routineSuggestions offers the routines filter keeps at rank. call says
// whether the name is followed by its argument list (a call) or stands
// alone (DDL naming it). bare inserts the name unqualified: it follows a
// schema the user typed.
func (c *completer) routineSuggestions(groups []*rgroup, filter routineFilter, rank int, call, bare bool) {
	paren := c.caret < len(c.buf) && c.buf[c.caret] == '('
	for _, g := range groups {
		ok, up := filter(g)
		if !ok {
			continue
		}
		ins := c.d.quote(g.name)
		if !bare {
			ins = c.routineRef(g)
		}
		it := Item{Label: g.name, Kind: KindFunction, Detail: c.routineDetail(g), Doc: routineDoc(g), Insert: ins, Cursor: -1}
		if g.kind() == model.RoutineProcedure {
			it.Kind = KindProcedure
		}
		if call && !paren {
			it.Insert = ins + "()"
			// between the parentheses, unless nothing could go there
			if g.any(func(r model.Routine) bool { return r.Args != "" }) {
				it.Cursor = len(ins) + 1
			}
		}
		c.add(it, rank+up)
	}
}

// routineDetail is the line beside a routine's name: its first signature,
// the schema before it when the catalog has several, the count of any
// other overloads after.
//
//	order_total(o integer) → numeric
//	billing · order_total(o integer) → numeric · +1 overload
func (c *completer) routineDetail(g *rgroup) string {
	d := signature(g.overloads[0])
	if c.manySchemas {
		d = g.schema + " · " + d
	}
	switch n := len(g.overloads) - 1; {
	case n == 1:
		d += " · +1 overload"
	case n > 1:
		d += " · +" + itoa(n) + " overloads"
	}
	return d
}

// signature renders a routine as name(args) → result, as the built-in
// vocabulary's signatures read; a procedure has no result.
func signature(r model.Routine) string {
	s := r.Name + "(" + r.Args + ")"
	if r.Result != "" {
		s += " → " + r.Result
	}
	return s
}

// routineDoc is a routine's documentation: what it is and where it lives,
// then every overload's signature, one per line.
//
//	function billing.order_total
//	order_total(o integer) → numeric
//	order_total(o integer, tax boolean) → numeric
func routineDoc(g *rgroup) string {
	lines := []string{string(g.kind()) + " " + g.schema + "." + g.name}
	for _, r := range g.overloads {
		lines = append(lines, signature(r))
	}
	return strings.Join(lines, "\n")
}

// routineRef is how a statement names g: bare where the bare name resolves
// to it, schema-qualified otherwise — by tableRef's rule (bare), over the
// routines' schemas: on the search path, and no schema before it there
// with a routine of that name. Built-in functions (pg_catalog, searched
// before the path) are not considered: a user function shadowed by one is
// rare, and its qualified name would read as a mistake to most.
func (c *completer) routineRef(g *rgroup) string {
	if c.routineBare(g) {
		return c.d.quote(g.name)
	}
	return c.d.quote(g.schema) + "." + c.d.quote(g.name)
}

func (c *completer) routineBare(g *rgroup) bool {
	path := c.searchPath()
	if path == nil || (!c.manySchemas && c.req.SearchPath == nil) {
		return true
	}
	for _, s := range path {
		if s == g.schema {
			return true
		}
		for _, o := range c.rbySchema[strings.ToLower(s)] {
			if o.schema == s && o.name == g.name {
				return false // shadowed by an earlier schema's routine
			}
		}
	}
	return false
}

// routineAfter reports whether toks — the statement's tokens before the
// word being typed — end where a routine is named, and with what filter:
//
//	CALL ▮                          the statement's first word
//	DROP | ALTER | CREATE [OR] REPLACE | ON | EXECUTE  FUNCTION|PROCEDURE|ROUTINE ▮
//	DROP FUNCTION IF EXISTS ▮
//
// The word before FUNCTION is required so that a column named function
// (a plain identifier on Postgres) is not taken for the keyword.
func routineAfter(toks []tok) (filter routineFilter, call, ok bool) {
	n := len(toks)
	word := func(k int) string { // k = 1 is the last token
		if n-k < 0 || toks[n-k].kind != tWord {
			return ""
		}
		return toks[n-k].low
	}
	if n == 1 && word(1) == "call" {
		return procedures, true, true
	}
	kw, lead := word(1), word(2)
	if kw == "exists" && word(2) == "if" {
		kw, lead = word(3), word(4) // DROP FUNCTION IF EXISTS ▮
	}
	if routineWords[kw] == nil || !routineLeads[lead] {
		return nil, false, false
	}
	return routineContext(lead, kw)
}

// routineLeads are the words before FUNCTION, PROCEDURE or ROUTINE when it
// introduces a routine's name.
var routineLeads = map[string]bool{
	"drop": true, "alter": true, "create": true, "replace": true, "on": true, "execute": true,
}

// qualifiedRoutines is the filter for the routines of a schema the user
// typed (s.▮), toks being the tokens before the schema: a routine
// context's own (CALL s.▮, DROP FUNCTION s.▮), else set-returning
// functions where a table goes (FROM s.▮), else what an expression calls.
func qualifiedRoutines(toks []tok) (filter routineFilter, call bool) {
	if f, c, ok := routineAfter(toks); ok {
		return f, c
	}
	if n := len(toks); n > 0 && toks[n-1].kind == tWord && (tableWords[toks[n-1].low] || toks[n-1].low == "join") {
		return rowSources, true
	}
	return callable, true
}

// routineWords are the keywords that name a routine in DDL, with what each
// offers: FUNCTION ▮, PROCEDURE ▮, ROUTINE ▮.
var routineWords = map[string]routineFilter{
	"function": notProcedures, "procedure": procedures, "routine": anyRoutine,
}

// routineContext is the filter, and whether a call follows, for a routine
// name after the keyword kw (prev is the keyword before it, for EXECUTE
// FUNCTION); ok is false when kw does not precede a routine name.
func routineContext(prev, kw string) (filter routineFilter, call, ok bool) {
	switch {
	case kw == "call":
		return procedures, true, true
	case prev == "execute" && (kw == "function" || kw == "procedure"):
		// a trigger's EXECUTE FUNCTION f(), or EXECUTE PROCEDURE f() as
		// it was spelled before Postgres 11: either way it names a
		// function, whatever the keyword says
		return triggersFirst, true, true
	case routineWords[kw] != nil:
		return routineWords[kw], false, true
	}
	return nil, false, false
}
