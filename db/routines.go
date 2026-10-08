package db

import (
	"context"
	"strconv"
	"strings"

	bytdbdrv "github.com/rohanthewiz/bytdb/stdlib"
	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/model"
)

// Reading a connection's stored functions and procedures, for completion.
//
//	RoutinesQuery ──► schema · name · kind · args · result · id
//	                         │
//	          BuildRoutines (pure, tested without a database)
//	                         ▼
//	                  []model.Routine
//
// Every engine's query returns those six columns, so BuildRoutines reads
// them once for all. id is what RoutineDDL needs to find one overload
// again: Postgres's pg_proc oid; MySQL has no overloads, and leaves it
// empty (schema, name and kind are the routine there). kind is already one of model.RoutineKind's words, but
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
// functions and procedures, as schema · name · kind · args · result · id;
// "" for an engine with none to list.
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
       pg_catalog.pg_get_function_arguments(p.oid), pg_catalog.pg_get_function_result(p.oid),
       p.oid::text
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
       CASE WHEN r.routine_type = 'FUNCTION' THEN r.dtd_identifier ELSE '' END,
       ''
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
		if len(r) > 5 {
			rt.ID = r[5] // optional, so rows from before the id column still read
		}
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

// HasRoutines reports whether a driver's databases store functions and
// procedures that RoutinesQuery lists: Postgres and MySQL. A sidebar
// offers its routines list only there.
func HasRoutines(driver string) bool {
	drv, err := driverFor(driver)
	return err == nil && (drv == "pgx" || drv == "mysql")
}

// The definition of one routine, for the sidebar's "Show DDL".
//
// Each engine renders its own, so the text is the server's, not rebuilt
// from the catalog here — rebuilding would miss whatever the catalog
// query did not read (volatility, security, SET clauses, the language),
// and the server's rendering is what pg_dump and mysqldump emit too:
//
//	Postgres ──► pg_get_functiondef(oid)          CREATE OR REPLACE FUNCTION … $function$ … $function$
//	MySQL    ──► SHOW CREATE FUNCTION|PROCEDURE    the "Create Function|Procedure" column (the third)
//
// Postgres renders no definition for an aggregate (pg_get_functiondef
// refuses one: its CREATE AGGREGATE is pieced together from pg_aggregate
// by pg_dump alone), so that is refused here with a sentence saying so
// rather than with the server's terser error.

// RoutineDDLQuery returns the statement that reads r's definition on
// driver: its first row holds it, in the column RoutineDDLColumn says.
//
// The routine is named by its oid on Postgres (r.ID, from RoutinesQuery),
// which tells overloads apart and needs no quoting; it is checked to be a
// number, since it may come from a page, and is then put in the statement
// as a literal. On MySQL it is named by schema and name, each quoted as
// an identifier (backticks, doubled inside), since SHOW CREATE takes no
// placeholders.
func RoutineDDLQuery(driver string, r model.Routine) (string, error) {
	drv, err := driverFor(driver)
	if err != nil {
		return "", err
	}
	switch drv {
	case "pgx":
		if r.Kind == model.RoutineAggregate {
			return "", serr.New("Postgres renders no definition for an aggregate: pg_dump pieces its CREATE AGGREGATE together from pg_aggregate",
				"routine", r.QName())
		}
		if _, err := strconv.ParseUint(r.ID, 10, 32); err != nil {
			return "", serr.New("no oid to read the routine's definition by — list the routines again", "routine", r.QName(), "id", r.ID)
		}
		return "SELECT pg_catalog.pg_get_functiondef(" + r.ID + "::oid)", nil
	case "mysql":
		what := "FUNCTION"
		if r.Kind == model.RoutineProcedure {
			what = "PROCEDURE"
		}
		name := mysqlIdent(r.Name)
		if r.Schema != "" {
			name = mysqlIdent(r.Schema) + "." + name
		}
		return "SHOW CREATE " + what + " " + name, nil
	}
	return "", serr.New("this driver stores no routines", "driver", driver)
}

// RoutineDDLColumn is the column of RoutineDDLQuery's row that holds the
// definition: the only one on Postgres; on MySQL the third, after the
// routine's name and its sql_mode.
func RoutineDDLColumn(driver string) int {
	if drv, _ := driverFor(driver); drv == "mysql" {
		return 2
	}
	return 0
}

// mysqlIdent quotes a MySQL identifier: backticks, with any inside doubled.
func mysqlIdent(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}

// RoutineDDL reads r's definition as its server renders it (see
// RoutineDDLQuery), on the pool as the sidebar's other reads are, so it
// never lands inside a transaction the user has open.
//
// The text comes back ending in a semicolon, so it runs as a statement
// when put in the editor: pg_get_functiondef ends at the body's closing
// $function$ without one, and MySQL's SHOW CREATE has none either.
//
// A definition the server hides — MySQL shows it only to the routine's
// definer and to users with SHOW_ROUTINE (8.0.20+) or the global SELECT
// privilege, and a NULL otherwise — is an error that says so, rather
// than an empty statement.
func (m *Manager) RoutineDDL(ctx context.Context, name string, r model.Routine) (string, error) {
	cc, ok := m.cfg.ConnByName(name)
	if !ok {
		return "", serr.New("unknown connection", "name", name)
	}
	q, err := RoutineDDLQuery(cc.Driver, r)
	if err != nil {
		return "", err
	}
	dbh, err := m.DBContext(ctx, name)
	if err != nil {
		return "", err
	}
	_, rows, _, err := limitedRows(ctx, dbh, q, 1)
	if err != nil {
		return "", wrapRunErr(ctx, err, name, "op", "read the definition of "+r.QName())
	}
	col := RoutineDDLColumn(cc.Driver)
	if len(rows) == 0 || len(rows[0]) <= col {
		return "", serr.New("no such routine any more — list the routines again", "routine", r.QName())
	}
	ddl := strings.TrimRight(rows[0][col], " \t\r\n")
	if ddl == "" {
		return "", serr.New("the server shows this routine's definition only to its definer, or to a user with SHOW_ROUTINE or global SELECT",
			"routine", r.QName())
	}
	if !strings.HasSuffix(ddl, ";") {
		ddl += ";"
	}
	return ddl, nil
}
