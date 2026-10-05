package db

import (
	"context"
	"strconv"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/model"
)

// The sidebar's catalog, loaded a level at a time.
//
// A Postgres host can hold many databases, each with many schemas, each with
// many tables, and listing every table of everything up front is the one
// thing a sidebar on a big server must not do. So on Postgres a connect
// reads only the two short lists, and the tables of one schema at a time:
//
//	connect ──► DatabasesQuery      every database on the server  (one row each)
//	        ──► SchemaSummaryQuery  this database's schemas, with
//	                                their table counts and the
//	                                search_path's first schema    (one row each)
//	        ──► SchemaTablesQuery   the tables of ONE schema: the
//	                                one asked for, else that first
//	                                schema                         (the slow part, bounded)
//	pick a schema   ──► SchemaTablesQuery again
//	pick a database ──► a connect to "<conn>/<database>" (config.DatabaseSep)
//
// MySQL has the top level only. Its "schema" is its database — one name,
// one namespace — and dbc's catalog queries are scoped to DATABASE(), the
// one the DSN connects to, so a MySQL sidebar lists a database's tables
// whole (TablesQuery) and has no schema level, but its server's other
// databases are another connection each, exactly as on Postgres:
//
//	connect ──► DatabasesQuery      every database on the server
//	        ──► TablesQuery         this database's tables, whole
//	pick a database ──► a connect to "<conn>/<database>"
//
// So two questions, two functions: HasDatabases (Postgres, MySQL) is
// whether there is a database picker, Navigable (Postgres) whether the
// tables come a schema at a time behind a schema picker.
//
// SQLite has only main, and a bytdb file is one small catalog: neither has
// anything to navigate. They keep loading their whole table list, and
// their Databases and SchemaSummary are empty.

// maxCatalogRows bounds the sidebar's catalog read. It is the diagram's
// bound rather than max_rows, for the diagram's reason: max_rows caps what
// a person sees of a result they asked for, and applied to the catalog it
// silently cut the sidebar to the first 1,000 tables — on a big database,
// to the first few schemas in alphabetical order, with the rest simply
// missing. A quarter of a million relations is past anything a sidebar is
// for, and stopping there keeps a runaway catalog from eating memory.
const maxCatalogRows = maxSchemaRows

// AllSchemasLimit is the most tables a Postgres database may have for its
// sidebar to offer "all schemas" at once. Below it, the whole list is a
// blink to read and to draw, and one list is easier to scan than a picker;
// above it, a schema at a time is the only load a big database stays quick
// under.
const AllSchemasLimit = 5000

// Navigable reports whether a driver's sidebar is loaded a level at a time
// (databases, schemas, then one schema's tables) rather than whole.
func Navigable(driver string) bool {
	drv, err := driverFor(driver)
	return err == nil && drv == "pgx"
}

// HasDatabases reports whether a driver's connection can list its server's
// databases and be switched onto one of them, as "<conn>/<database>"
// (config.DatabaseSep) — the database picker. It is
// config.SupportsDatabases, which decides what ConnByName will derive, for
// a known driver.
func HasDatabases(driver string) bool {
	_, err := driverFor(driver)
	return err == nil && config.SupportsDatabases(driver)
}

// DatabaseInfo is one database on a connection's server.
type DatabaseInfo struct {
	Name    string
	Current bool // the database this connection is open on
}

// SchemaInfo is one schema of a database, as the sidebar's picker lists it.
type SchemaInfo struct {
	Name string
	// Tables is its tables, views, matviews and foreign tables, or
	// TablesUnknown when the list came from SchemaNames, which does not
	// count them.
	Tables  int
	Default bool // the first schema on search_path: what a bare name means
}

// TablesUnknown is SchemaInfo.Tables for a schema whose tables were not
// counted (SchemaNames). It is not 0: an uncounted schema may well hold
// tables, and a picker must neither dim it as empty nor add it into a total.
const TablesUnknown = -1

// DatabasesQuery returns the statement that lists the databases a
// connection's server holds and its user may connect to, as (name,
// current), or "" for a driver without databases (HasDatabases).
//
// Postgres: templates are left out (nobody browses template1), as are
// databases that refuse connections (datallowconn, e.g. one being dropped)
// and those the user lacks CONNECT on: picking one would only fail. The
// privilege test takes the oid, so a name needing quotes is not parsed as
// SQL.
//
// MySQL: information_schema.schemata already shows only the databases the
// user holds some privilege in (all of them with SHOW DATABASES), which is
// the CONNECT test's counterpart. The server's own databases are left out,
// as Postgres's templates are — information_schema, performance_schema and
// sys are views of the server, and mysql is its grant tables — unless the
// DSN itself opened one, so the picker still names the database in use.
// With no database in the DSN, DATABASE() is NULL and no row is current.
func DatabasesQuery(driver string) (string, error) {
	drv, err := driverFor(driver)
	if err != nil {
		return "", err
	}
	if !HasDatabases(driver) {
		return "", nil
	}
	if drv == "mysql" {
		return `SELECT schema_name, COALESCE(schema_name = DATABASE(), 0) AS current
FROM information_schema.schemata
WHERE schema_name NOT IN ('information_schema', 'mysql', 'performance_schema', 'sys')
   OR schema_name = DATABASE()
ORDER BY schema_name`, nil
	}
	return `SELECT datname, datname = current_database() AS current
FROM pg_catalog.pg_database
WHERE NOT datistemplate AND datallowconn AND has_database_privilege(oid, 'CONNECT')
ORDER BY datname`, nil
}

// SchemaSummaryQuery returns the statement that lists a database's schemas
// as (name, tables, is_default), or "" for a driver that is not Navigable.
//
// Every user schema is listed, empty ones too (the LEFT JOIN), so a schema
// the user knows is there is in the picker even with nothing in it. The
// counts are of what the sidebar would list (TablesQuery's relkinds), and
// cost one pass over pg_class with no per-table work. is_default marks the
// first schema on search_path that exists — current_schemas(false) leaves
// out the implicit pg_catalog and any "$user" with no schema — which is the
// schema a bare table name resolves to, and so the one to open on.
func SchemaSummaryQuery(driver string) (string, error) {
	if _, err := driverFor(driver); err != nil {
		return "", err
	}
	if !Navigable(driver) {
		return "", nil
	}
	return `SELECT n.nspname AS schema_name, count(c.oid) AS tables,
       coalesce(n.nspname = (current_schemas(false))[1], false) AS is_default
FROM pg_catalog.pg_namespace n
LEFT JOIN pg_catalog.pg_class c ON c.relnamespace = n.oid AND c.relkind IN ('r', 'p', 'v', 'm', 'f')
WHERE ` + pgUserSchemas + `
GROUP BY n.nspname
ORDER BY n.nspname`, nil
}

// SchemaNamesQuery is SchemaSummaryQuery without the counts: the same
// columns, with tables always TablesUnknown. It is the fallback for a
// database whose summary does not finish in time.
//
// The counts are the summary's cost: a pass over the whole of pg_class,
// which on a production database can hold millions of rows — every
// partition of every partitioned table, each with its indexes and TOAST
// table, plus whatever bloat churn (temp tables, partitions created and
// dropped) has left in it — and has no index on relnamespace to narrow
// the pass. pg_namespace has a row per schema and nothing else, so this
// reads in milliseconds on any server, and the sidebar still gets its
// schema picker: without one, a database too big to count is also a
// database whose tables can only be read whole, the one read certain to
// fail there.
func SchemaNamesQuery(driver string) (string, error) {
	if _, err := driverFor(driver); err != nil {
		return "", err
	}
	if !Navigable(driver) {
		return "", nil
	}
	return `SELECT n.nspname AS schema_name, ` + strconv.Itoa(TablesUnknown) + ` AS tables,
       coalesce(n.nspname = (current_schemas(false))[1], false) AS is_default
FROM pg_catalog.pg_namespace n
WHERE ` + pgUserSchemas + `
ORDER BY n.nspname`, nil
}

// SchemaTablesQuery is TablesQuery narrowed to one schema, in the same
// shape. Only a Navigable driver has one; the others list their tables
// whole.
func SchemaTablesQuery(driver, schema string) (string, error) {
	if _, err := driverFor(driver); err != nil {
		return "", err
	}
	if !Navigable(driver) {
		return "", serr.New("this driver's tables are not listed by schema", "driver", driver)
	}
	return pgTables(schema), nil
}

// Catalog lists tables for a sidebar: those of schema on a Navigable
// driver, or the whole catalog when schema is "" (and always, on the other
// drivers, which are not listed by schema).
//
// It runs on the pool, so it never lands inside a transaction the user has
// open, and reads the rows itself rather than through Run so that max_rows
// does not apply (see maxCatalogRows). A list cut at maxCatalogRows comes
// back with Truncated set, for a UI to say so, rather than as an error,
// since the tables it did read are still worth listing.
//
// The result has Columns and Rows but no Raw: it is drawn as a list, never
// as a grid or an export (Ctrl+T, which is, still goes through Run).
func (m *Manager) Catalog(ctx context.Context, name, schema string) (*model.Result, error) {
	cc, ok := m.cfg.ConnByName(name)
	if !ok {
		return nil, serr.New("unknown connection", "name", name)
	}
	q, err := TablesQuery(cc.Driver)
	if err == nil && schema != "" && Navigable(cc.Driver) {
		q, err = SchemaTablesQuery(cc.Driver, schema)
	}
	if err != nil {
		return nil, err
	}
	dbh, err := m.DBContext(ctx, name)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	cols, rows, truncated, err := limitedRows(ctx, dbh, q, maxCatalogRows)
	if err != nil {
		return nil, wrapRunErr(ctx, err, name, "op", "read the catalog")
	}
	return &model.Result{Conn: name, Query: q, Columns: cols, Rows: rows,
		Truncated: truncated, Duration: time.Since(start)}, nil
}

// Databases lists the connection's server's databases per DatabasesQuery,
// on the pool; nil with no error for a driver without databases
// (HasDatabases).
func (m *Manager) Databases(ctx context.Context, name string) ([]DatabaseInfo, error) {
	rows, err := m.navRows(ctx, name, DatabasesQuery, "list databases")
	if err != nil || rows == nil {
		return nil, err // nil rows: no databases to list on this driver
	}
	out := make([]DatabaseInfo, 0, len(rows))
	for _, r := range rows {
		if len(r) >= 2 {
			out = append(out, DatabaseInfo{Name: r[0], Current: pgBool(r[1])})
		}
	}
	return out, nil
}

// SchemaSummary lists the connection's schemas per SchemaSummaryQuery, on
// the pool; nil with no error for a driver that is not Navigable.
func (m *Manager) SchemaSummary(ctx context.Context, name string) ([]SchemaInfo, error) {
	rows, err := m.navRows(ctx, name, SchemaSummaryQuery, "list schemas")
	return schemaInfos(rows), err
}

// SchemaNames lists the connection's schemas per SchemaNamesQuery — their
// Tables TablesUnknown — on the pool; nil with no error for a driver that
// is not Navigable.
func (m *Manager) SchemaNames(ctx context.Context, name string) ([]SchemaInfo, error) {
	rows, err := m.navRows(ctx, name, SchemaNamesQuery, "list schema names")
	return schemaInfos(rows), err
}

// schemaInfos reads the (name, tables, is_default) rows the two schema
// lists share; nil for no rows (a failed read, or not Navigable).
func schemaInfos(rows [][]string) []SchemaInfo {
	if rows == nil {
		return nil
	}
	out := make([]SchemaInfo, 0, len(rows))
	for _, r := range rows {
		if len(r) >= 3 {
			n, _ := strconv.Atoi(r[1])
			out = append(out, SchemaInfo{Name: r[0], Tables: n, Default: pgBool(r[2])})
		}
	}
	return out
}

// navRows runs one of the navigator's list queries on the pool, or returns
// nothing for a driver it has no query for.
func (m *Manager) navRows(ctx context.Context, name string, query func(string) (string, error), op string) ([][]string, error) {
	cc, ok := m.cfg.ConnByName(name)
	if !ok {
		return nil, serr.New("unknown connection", "name", name)
	}
	q, err := query(cc.Driver)
	if err != nil || q == "" {
		return nil, err
	}
	dbh, err := m.DBContext(ctx, name)
	if err != nil {
		return nil, err
	}
	_, rows, _, err := limitedRows(ctx, dbh, q, maxCatalogRows)
	if err != nil {
		return nil, wrapRunErr(ctx, err, name, "op", op)
	}
	return rows, nil
}

// pgBool reads a boolean cell as database/sql renders it to a string: pgx
// gives "true"/"false" through sql.NullString, MySQL (which has no boolean
// type) "1"/"0". Anything else, NULL included, is false.
func pgBool(s string) bool {
	b, _ := strconv.ParseBool(s)
	return b
}

// DefaultDatabase is the database a connection's DSN opens, as the driver
// resolves it — pgx falls back to PGDATABASE, then to the user name, when
// the DSN names none; MySQL then opens on no database at all — or "" when
// that cannot be told (an unparsable DSN, a driver without databases) or
// there is none. A database picker maps this one back to the configured
// connection itself rather than to "<conn>/<database>", so picking it does
// not open a second pool onto the same database. A MySQL DSN naming no
// database maps none back: every database it lists is a derived
// connection, and the configured one stays the "no database" view.
func DefaultDatabase(cc config.Connection) string {
	drv, err := driverFor(cc.Driver)
	if err != nil || !HasDatabases(cc.Driver) {
		return ""
	}
	if cc.Database != "" {
		return cc.Database
	}
	if drv == "mysql" {
		mc, err := mysql.ParseDSN(cc.DSN)
		if err != nil {
			return ""
		}
		return mc.DBName
	}
	pcfg, err := pgx.ParseConfig(cc.DSN)
	if err != nil {
		return ""
	}
	return pcfg.Database
}
