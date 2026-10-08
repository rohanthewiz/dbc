package model

// Routine is a function or procedure stored in a database, as its catalog
// lists it: what completion offers beside the dialect's built-in
// functions. It lives here, not in db or sqlcomplete, because both use it
// — db reads it, sqlcomplete suggests it — and neither should import the
// other.
//
// Args and Result are the engine's own rendering, kept as text: completion
// shows them and never parses them, so a structured form would be work
// for nothing.
type Routine struct {
	Schema string
	Name   string
	Kind   RoutineKind
	// Args is the argument list as declared, without its parentheses:
	// "customer integer, since date DEFAULT now()"; "" for none.
	Args string
	// Result is what a function returns: "integer", "SETOF orders",
	// "TABLE(id integer, total numeric)"; "" for a procedure.
	Result string
	// ID tells one overload from another when the routine's definition is
	// asked for (db.Manager.RoutineDDL): Postgres's pg_proc oid, as text.
	// "" on MySQL, which has no overloads — schema, name and kind are
	// enough there.
	ID string
}

// QName is the routine's name qualified by its schema, as a sidebar and
// a log line show it: "public.order_total". A routine read without a
// schema is its bare name.
func (r Routine) QName() string {
	if r.Schema == "" {
		return r.Name
	}
	return r.Schema + "." + r.Name
}

// RoutineKind says how a routine is called.
type RoutineKind string

const (
	RoutineFunction  RoutineKind = "function"  // in an expression: f(x)
	RoutineProcedure RoutineKind = "procedure" // in a CALL statement
	RoutineAggregate RoutineKind = "aggregate" // in an expression, over rows
	RoutineWindow    RoutineKind = "window"    // in an expression, with OVER
	// RoutineTrigger is a function returning trigger or event_trigger:
	// it runs only from CREATE TRIGGER … EXECUTE FUNCTION, never in an
	// expression, so completion offers it after FUNCTION alone.
	RoutineTrigger RoutineKind = "trigger"
)

// Callable reports whether the routine can be called in an expression.
func (r Routine) Callable() bool {
	switch r.Kind {
	case RoutineFunction, RoutineAggregate, RoutineWindow:
		return true
	}
	return false
}
