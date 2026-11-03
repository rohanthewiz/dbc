package db

import (
	"context"
	"strings"

	bytdbdrv "github.com/rohanthewiz/bytdb/stdlib"
	"github.com/rohanthewiz/serr"
)

// The definition of one view, for the sidebar's Views section ("Show
// DDL"), the counterpart of RoutineDDL for routines.
//
// The view itself is already in the catalog (TablesQuery lists views
// beside tables, in one read), so nothing here runs until a view's DDL is
// asked for: one statement, for one view, on the pool.
//
//	Postgres ──► pg_class + pg_get_viewdef(oid, true)   the SELECT; the CREATE is put around it here
//	bytdb    ──► the same: it serves pg_class (relkind 'v') and pg_get_viewdef(oid)
//	MySQL    ──► SHOW CREATE VIEW                         the "Create View" column (the second)
//	SQLite   ──► sqlite_master.sql                        the CREATE VIEW as it was typed
//
// WHY pg_class AND NOT information_schema.views: the standard view shows a
// definition only to the view's owner (NULL for everyone else), leaves out
// materialized views, and bytdb serves no information_schema.views at all.
// pg_get_viewdef renders any view the role can see, as \d+ does.
//
// Postgres renders only the query, so the CREATE is rebuilt around it —
// CREATE OR REPLACE VIEW, or CREATE MATERIALIZED VIEW (which has no OR
// REPLACE) for relkind 'm' — with the name quoted only where it needs it,
// as pg_get_functiondef quotes a routine's. What the rebuilt statement
// leaves out (WITH options such as security_barrier, a matview's storage
// parameters, the owner and grants) is what pg_dump adds beside it; the
// viewer is for reading and reusing the query, not for a faithful dump.

// ViewDDLQuery returns the statement that reads view's definition on
// driver: its first row holds it, in the column ViewDDLColumn says. On
// Postgres and bytdb the row is relkind · query, which ViewDDL turns into a
// CREATE statement.
//
// Names go into the statement as literals (sqlLit) or quoted identifiers,
// never spliced bare: the view may come from a page.
func ViewDDLQuery(driver string, view TableRef) (string, error) {
	drv, err := driverFor(driver)
	if err != nil {
		return "", err
	}
	if view.Name == "" {
		return "", serr.New("no view named")
	}
	switch drv {
	case "pgx", bytdbdrv.DriverName:
		schema := view.Schema
		if schema == "" {
			schema = "public"
		}
		return `SELECT c.relkind::text, pg_catalog.pg_get_viewdef(c.oid, true)
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = ` + sqlLit(schema, false) + ` AND c.relname = ` + sqlLit(view.Name, false) + `
  AND c.relkind IN ('v', 'm')`, nil
	case "mysql":
		name := mysqlIdent(view.Name)
		if view.Schema != "" {
			name = mysqlIdent(view.Schema) + "." + name
		}
		return "SHOW CREATE VIEW " + name, nil
	case "sqlite":
		// SQLite keeps the statement as typed; no schema but main is listed
		// (TablesQuery), so the name alone finds it
		return `SELECT sql FROM sqlite_master WHERE type = 'view' AND name = ` + sqlLit(view.Name, false), nil
	}
	return "", serr.New("this driver lists no views", "driver", driver)
}

// ViewDDLColumn is the column of ViewDDLQuery's row that holds the
// definition: MySQL's second (after the view's name); the second on
// Postgres and bytdb too (after relkind); SQLite's only one.
func ViewDDLColumn(driver string) int {
	if drv, _ := driverFor(driver); drv == "sqlite" {
		return 0
	}
	return 1
}

// pgViewDDL puts the CREATE around the query pg_get_viewdef rendered.
// kind is pg_class.relkind: 'm' for a materialized view, else a view.
func pgViewDDL(view TableRef, kind, query string) string {
	name := pgIdent(view.Name)
	if view.Schema != "" {
		name = pgIdent(view.Schema) + "." + name
	}
	head := "CREATE OR REPLACE VIEW "
	if kind == "m" {
		head = "CREATE MATERIALIZED VIEW "
	}
	// pg_get_viewdef starts its pretty form with a space (" SELECT …"),
	// which the line break after AS makes a stray indent
	return head + name + " AS\n" + strings.TrimLeft(query, " ")
}

// pgIdent quotes a Postgres identifier only where it must be — anything
// but lower-case letters, digits and underscores, or a leading digit —
// as quote_ident does, so the common view reads "public.order_totals"
// rather than "\"public\".\"order_totals\"". A reserved word used as a
// name is not caught (quote_ident consults the keyword list); it is rare
// in a view's name and the statement then fails visibly, not silently.
func pgIdent(s string) string {
	plain := s != "" && !(s[0] >= '0' && s[0] <= '9')
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			plain = false
			break
		}
	}
	if plain {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// ViewDDL reads view's definition (see ViewDDLQuery), on the pool as the
// sidebar's other reads are, so it never lands inside a transaction the
// user has open. The text ends in a semicolon, so it runs as a statement
// when put in the editor.
//
// A definition the server hides — MySQL's SHOW CREATE VIEW needs SHOW VIEW
// and SELECT on the view, and fails without them — comes back as the
// server's error; an empty one (a NULL pg_get_viewdef) as an error saying
// so, rather than an empty statement.
func (m *Manager) ViewDDL(ctx context.Context, name string, view TableRef) (string, error) {
	cc, ok := m.cfg.ConnByName(name)
	if !ok {
		return "", serr.New("unknown connection", "name", name)
	}
	q, err := ViewDDLQuery(cc.Driver, view)
	if err != nil {
		return "", err
	}
	dbh, err := m.DBContext(ctx, name)
	if err != nil {
		return "", err
	}
	qname := view.Name
	if view.Schema != "" {
		qname = view.Schema + "." + view.Name
	}
	_, rows, _, err := limitedRows(ctx, dbh, q, 1)
	if err != nil {
		return "", wrapRunErr(ctx, err, name, "op", "read the definition of "+qname)
	}
	col := ViewDDLColumn(cc.Driver)
	if len(rows) == 0 || len(rows[0]) <= col {
		return "", serr.New("no such view any more — refresh the sidebar", "view", qname)
	}
	ddl := strings.TrimRight(rows[0][col], " \t\r\n")
	if ddl == "" {
		return "", serr.New("the server shows no definition of this view to this user", "view", qname)
	}
	if drv, _ := driverFor(cc.Driver); drv == "pgx" || drv == bytdbdrv.DriverName {
		ddl = pgViewDDL(view, rows[0][0], ddl)
	}
	if !strings.HasSuffix(ddl, ";") {
		ddl += ";"
	}
	return ddl, nil
}
