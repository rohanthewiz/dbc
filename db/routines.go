package db

import (
	"context"
	"strings"

	bytdbdrv "github.com/rohanthewiz/bytdb/stdlib"
	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/model"
)

// Reading a connection's stored functions and procedures, for completion.
//
//	RoutinesQuery ──► schema · name · kind · args · result
//	                         │
//	          BuildRoutines (pure, tested without a database)
//	                         ▼
//	                  []model.Routine
//
// Every engine's query returns those five columns, so BuildRoutines reads
// them once for all. kind is already one of model.RoutineKind's words, but
// for Postgres's single letters (prokind), which BuildRoutines maps.
//
// WHICH ENGINES. Postgres reads pg_proc, MySQL information_schema.routines.
// SQLite has no stored routines (its functions are built in or registered
// by the program, and the dialect's vocabulary has the built-in ones), and
// bytdb serves no pg_proc: both read none, with no error — there is
// nothing to read, rather than a failure.
//
// WHAT IS LEFT OUT. The system schemas' routines (pg_catalog's are the
// built-in functions, which the dialect's vocabulary documents better),
// and on Postgres the routines no statement calls: those taking or
// returning internal or cstring (an extension's type I/O and index support
// functions — pg_trgm alone has dozens) and the handlers (language, FDW,
// index and table access method). Those are dropped in BuildRoutines
// rather than in SQL: the handler types differ between Postgres versions
// (table_am_handler is 12+), and a cast to a type the server lacks would
// fail the whole query.

// RoutinesQuery returns the statement listing the connection's stored
// functions and procedures, as schema · name · kind · args · result; "" for
// an engine with none to list.
func RoutinesQuery(driver string) (string, error) {
	return routinesQuery(driver, nil)
}

// routinesQuery is RoutinesQuery, kept to the scope schemas on Postgres when
// scope is non-nil — the schemas a scoped completion load reads tables from
// (see Manager.SchemaIn).
//
// prokind is Postgres 11+; on an older server the query fails and
// completion goes without routines, as it would on any read error.
func routinesQuery(driver string, scope []string) (string, error) {
	drv, err := driverFor(driver)
	if err != nil {
		return "", err
	}
	switch drv {
	case "pgx":
		where := ""
		if scope != nil {
			where = "\n  AND " + pgSchemaIn(scope)
		}
		// pg_get_function_arguments renders defaults and modes as
		// declared ("a integer, VARIADIC b text[]"); pg_get_function_result
		// renders SETOF and TABLE(…) results, and is NULL for a procedure
		return `SELECT n.nspname, p.proname, p.prokind::text,
       pg_catalog.pg_get_function_arguments(p.oid), pg_catalog.pg_get_function_result(p.oid)
FROM pg_catalog.pg_proc p
JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
WHERE n.nspname <> 'information_schema' AND n.nspname NOT LIKE 'pg\_%'` + where + `
ORDER BY 1, 2`, nil
	case "mysql":
		// a procedure's parameters are IN, OUT or INOUT, and say so; a
		// function's can only be IN, which MySQL reports (8.4) or leaves
		// NULL (older servers) but nobody declares — so it is shown for
		// procedures alone (a NULL, which CONCAT_WS skips, for
		// functions). ordinal_position 0 is a function's return value,
		// which the routine row carries.
		return `SELECT r.routine_schema, r.routine_name, LOWER(r.routine_type),
       COALESCE((SELECT GROUP_CONCAT(CONCAT_WS(' ', CASE WHEN r.routine_type = 'PROCEDURE' THEN p.parameter_mode END, p.parameter_name, p.dtd_identifier)
                                     ORDER BY p.ordinal_position SEPARATOR ', ')
                 FROM information_schema.parameters p
                 WHERE p.specific_schema = r.routine_schema AND p.specific_name = r.specific_name
                   AND p.ordinal_position > 0), ''),
       CASE WHEN r.routine_type = 'FUNCTION' THEN r.dtd_identifier ELSE '' END
FROM information_schema.routines r
WHERE r.routine_schema = DATABASE()
ORDER BY r.routine_name`, nil
	case "sqlite", bytdbdrv.DriverName:
		return "", nil
	}
	return "", serr.New("no routine listing for this driver", "driver", driver)
}

// pgKinds maps pg_proc.prokind to a routine kind.
var pgKinds = map[string]model.RoutineKind{
	"f": model.RoutineFunction, "p": model.RoutineProcedure,
	"a": model.RoutineAggregate, "w": model.RoutineWindow,
}

// uncallable are the Postgres result types of routines no statement calls
// (see WHAT IS LEFT OUT).
var uncallable = map[string]bool{
	"internal": true, "cstring": true, "language_handler": true, "fdw_handler": true,
	"index_am_handler": true, "tsm_handler": true, "table_am_handler": true,
}

// BuildRoutines turns RoutinesQuery's rows into routines, in the rows'
// order, dropping those no statement can call and marking trigger
// functions (model.RoutineTrigger).
func BuildRoutines(rows [][]string) []model.Routine {
	out := make([]model.Routine, 0, len(rows))
	for _, r := range rows {
		if len(r) < 5 {
			continue
		}
		rt := model.Routine{Schema: r[0], Name: r[1], Kind: model.RoutineKind(r[2]), Args: r[3], Result: r[4]}
		if k, ok := pgKinds[r[2]]; ok {
			rt.Kind = k
		}
		if uncallable[rt.Result] || hasServerArg(rt.Args) {
			continue
		}
		if rt.Result == "trigger" || rt.Result == "event_trigger" {
			rt.Kind = model.RoutineTrigger
		}
		out = append(out, rt)
	}
	return out
}

// hasServerArg reports whether an argument list takes a value of type
// internal, which only the server itself can pass, or cstring, the input
// of a type's I/O function — callable in principle, but never what a
// statement means to call, and an extension may bring dozens. Each
// argument is "[mode] [name] type [DEFAULT …]"; the type is matched as
// the argument's last word before any default, so a parameter merely
// named "internal" does not count.
func hasServerArg(args string) bool {
	if !strings.Contains(args, "internal") && !strings.Contains(args, "cstring") {
		return false // the common case, without splitting
	}
	for a := range strings.SplitSeq(args, ",") {
		a, _, _ = strings.Cut(a, " DEFAULT ")
		f := strings.Fields(a)
		if len(f) > 0 && (f[len(f)-1] == "internal" || f[len(f)-1] == "cstring") {
			return true
		}
	}
	return false
}

// Routines reads the connection's stored functions and procedures (see
// RoutinesQuery), kept to scope's schemas on Postgres when scope is
// non-nil. It runs on the pool, as Schema does, and is bounded by
// maxSchemaRows the same way. An engine with no routines to list gets nil
// and no error.
func (m *Manager) Routines(ctx context.Context, name string, scope []string) ([]model.Routine, error) {
	cc, ok := m.cfg.ConnByName(name)
	if !ok {
		return nil, serr.New("unknown connection", "name", name)
	}
	if !Navigable(cc.Driver) {
		scope = nil
	}
	q, err := routinesQuery(cc.Driver, scope)
	if err != nil || q == "" {
		return nil, err
	}
	dbh, err := m.DBContext(ctx, name)
	if err != nil {
		return nil, err
	}
	rows, err := stringRows(ctx, dbh, q)
	if err != nil {
		return nil, wrapRunErr(ctx, err, name, "op", "read the routines")
	}
	return BuildRoutines(rows), nil
}
