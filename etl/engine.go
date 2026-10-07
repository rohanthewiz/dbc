// Package etl moves rows between database connections — possibly on
// different engines — for dbc scripts: stream rows out of one connection
// (Reader), optionally reshape them in Go, and load them into another
// (Writer). Copy composes the two into "copy this table over there".
//
// Everything streams: no step holds a whole table in memory, so a copy is
// bounded by the network and the destination's write speed, not by RAM.
//
//	           ┌──────────── Copy ─────────────────────────────────────┐
//	src conn ─►│ Reader ─► row []any ─► Transform? ─► Writer           │─► dst conn
//	           │                                    ├ Postgres: COPY FROM STDIN (text)
//	           │                                    └ others:   batched INSERT … VALUES
//	           └───────────────────────────────────────────────────────┘
//	Postgres → Postgres with nothing to do to the rows skips the middle:
//	  COPY (SELECT …) TO STDOUT ──io.Pipe──► COPY … FROM STDIN
//
// A Writer loads inside one transaction, so a failed or stopped load leaves
// the destination as it was. On Postgres (and bytdb, SQLite) that includes
// the CREATE TABLE and TRUNCATE a Copy runs first; MySQL commits DDL
// implicitly, so there only the rows are atomic.
package etl

import (
	"database/sql"
	"strconv"
	"strings"

	"github.com/rohanthewiz/serr"
)

// Engine is a destination/source SQL dialect: how identifiers are quoted,
// which placeholder style binds parameters, and which types a created table
// gets. It is derived from the database/sql driver name, never from the
// connection's name.
type Engine int

const (
	Postgres Engine = iota + 1
	MySQL
	SQLite
	Bytdb
)

func (e Engine) String() string {
	switch e {
	case Postgres:
		return "postgres"
	case MySQL:
		return "mysql"
	case SQLite:
		return "sqlite"
	case Bytdb:
		return "bytdb"
	}
	return "unknown"
}

// EngineOf maps a canonical database/sql driver name — what db.Driver
// returns: pgx, mysql, sqlite, bytdb — to its Engine.
func EngineOf(driver string) (Engine, error) {
	switch driver {
	case "pgx":
		return Postgres, nil
	case "mysql":
		return MySQL, nil
	case "sqlite":
		return SQLite, nil
	case "bytdb":
		return Bytdb, nil
	}
	return 0, serr.New("etl: unsupported driver", "driver", driver)
}

// Conn is one end of a transfer: an open pool plus the engine that decides
// its SQL dialect. Name is only used in messages.
type Conn struct {
	Name   string
	DB     *sql.DB
	Engine Engine
	// Trace, when set, is handed each statement a transfer runs to ready
	// this end for the rows — Copy's CREATE TABLE, a Writer's Setup, its
	// TRUNCATE or the DELETE standing in for one — just before it runs.
	// The rows' own traffic (the SELECT, the COPY, the INSERT batches) is
	// not: it changes no structure, and would be a call per batch. dbc's
	// scripts log the DDL among these (sdb.S.LogDDL), which is why the
	// statements are handed over as written rather than summarized.
	Trace func(stmt string)
}

// trace hands stmt to c.Trace, when there is one.
func (c Conn) trace(stmt string) {
	if c.Trace != nil {
		c.Trace(stmt)
	}
}

// QuoteIdent quotes one identifier for the engine, doubling any embedded
// quote character so a hostile or odd column name cannot break out.
func (e Engine) QuoteIdent(name string) string {
	if e == MySQL {
		return "`" + strings.ReplaceAll(name, "`", "``") + "`"
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// QuoteTable quotes a possibly schema-qualified table name ("sales.orders").
// A name that already contains a quote character is taken to be quoted by
// the caller and used verbatim — splitting `"a.b".c` on dots would be wrong,
// and guessing at the user's intent is worse than trusting it.
func (e Engine) QuoteTable(name string) string {
	if strings.ContainsAny(name, "\"`") {
		return name
	}
	parts := strings.Split(name, ".")
	for i, p := range parts {
		parts[i] = e.QuoteIdent(p)
	}
	return strings.Join(parts, ".")
}

// quoteCols quotes and comma-joins a column list.
func (e Engine) quoteCols(cols []string) string {
	q := make([]string, len(cols))
	for i, c := range cols {
		q[i] = e.QuoteIdent(c)
	}
	return strings.Join(q, ", ")
}

// placeholder is the n-th (1-based) bind parameter in the engine's style.
func (e Engine) placeholder(n int) string {
	switch e {
	case Postgres, Bytdb:
		return "$" + strconv.Itoa(n)
	}
	return "?"
}

// truncateStmt empties a table inside the load's transaction, so that a
// failed load rolls the emptying back too.
//
//	Postgres, bytdb  TRUNCATE — transactional on both
//	SQLite           has no TRUNCATE; DELETE without WHERE is its equivalent
//	                 (and takes its truncate optimization)
//	MySQL            TRUNCATE is DDL there and commits implicitly — the old
//	                 rows would be gone even if the load then failed — so
//	                 DELETE, slower on a big table but rolled back with the rest
func (e Engine) truncateStmt(table string) string {
	if e == SQLite || e == MySQL {
		return "DELETE FROM " + e.QuoteTable(table)
	}
	return "TRUNCATE TABLE " + e.QuoteTable(table)
}

// transactionalDDL reports whether a CREATE TABLE can run inside the load's
// transaction and roll back with it. bytdb refuses DDL in a transaction
// block; MySQL accepts it but commits implicitly, which would end the
// load's transaction before the first row. On both, Copy creates the
// table first, on its own.
func (e Engine) transactionalDDL() bool {
	return e == Postgres || e == SQLite
}

// typeFamily buckets a driver-reported column type (ColumnType.
// DatabaseTypeName, upper-cased by every driver dbc uses) into the few
// kinds a cross-engine CREATE TABLE can express faithfully. Anything not
// recognised becomes text, which every engine can hold and every value can
// be rendered as — lossy for exotic types, but never a failed load.
type typeFamily int

const (
	famText typeFamily = iota
	famInt
	famFloat
	famNumeric
	famBool
	famDate
	famTimestamp
	famTimestampTZ
	famBytes
)

func familyOf(dbType string) typeFamily {
	t := strings.ToUpper(strings.TrimSpace(dbType))
	switch {
	case isBinaryType(t):
		return famBytes
	case t == "BOOL" || t == "BOOLEAN":
		return famBool
	case intTypes[strings.TrimPrefix(t, "UNSIGNED ")]:
		// An exact match, not "contains INT": INTERVAL, POINT, INT4RANGE and
		// the Postgres array _INT4 all contain it and none is an integer.
		// MySQL reports unsigned columns as "UNSIGNED BIGINT"; Postgres
		// reports SERIAL as INT4.
		return famInt
	case strings.HasPrefix(t, "FLOAT") || t == "DOUBLE" || t == "REAL" || t == "DOUBLE PRECISION":
		return famFloat
	case t == "NUMERIC" || t == "DECIMAL" || strings.HasPrefix(t, "NUMERIC(") || strings.HasPrefix(t, "DECIMAL("):
		// SQLite reports the declared type verbatim, modifier and all
		// ("DECIMAL(10,2)"); Postgres and MySQL report the bare name. The
		// precision is dropped: no destination mapping below uses it.
		return famNumeric
	case t == "DATE":
		return famDate
	case t == "TIMESTAMPTZ":
		return famTimestampTZ
	case strings.HasPrefix(t, "TIMESTAMP") || t == "DATETIME":
		return famTimestamp
	}
	return famText
}

var intTypes = map[string]bool{
	"INT": true, "INT2": true, "INT4": true, "INT8": true, "INTEGER": true,
	"SMALLINT": true, "MEDIUMINT": true, "BIGINT": true, "TINYINT": true,
}

// isBinaryType reports whether a driver type name holds raw bytes. Such
// columns keep their []byte values; every other column's []byte (MySQL's
// text protocol returns most values that way) is read as a string.
func isBinaryType(upper string) bool {
	switch upper {
	case "BYTEA", "BLOB", "TINYBLOB", "MEDIUMBLOB", "LONGBLOB", "BINARY", "VARBINARY", "BYTES":
		return true
	}
	return false
}

// columnType is the type a created destination column gets for a source
// column of the given family.
func (e Engine) columnType(f typeFamily) string {
	switch e {
	case MySQL:
		// LONGTEXT/LONGBLOB rather than VARCHAR(n): the source gives no
		// length, and a too-short guess fails the load half way.
		return [...]string{"LONGTEXT", "BIGINT", "DOUBLE", "DECIMAL(65,30)", "BOOLEAN",
			"DATE", "DATETIME(6)", "DATETIME(6)", "LONGBLOB"}[f]
	case SQLite:
		return [...]string{"TEXT", "INTEGER", "REAL", "NUMERIC", "BOOLEAN",
			"DATE", "TIMESTAMP", "TIMESTAMP", "BLOB"}[f]
	case Bytdb:
		// Postgres type names, except numeric: bytdb has no exact decimal
		// type, so a numeric column becomes double precision. Text would
		// keep every digit but cannot load the source values: SQLite yields
		// a numeric as int64/float64, which a bytdb text column refuses
		// ("value does not fit column type"), while a float column takes
		// those and the decimal strings pgx and MySQL yield alike. It also
		// keeps numeric order and arithmetic, which text would turn
		// lexicographic. The cost is precision past ~15 significant digits
		// (float64), and that is silent — a copy that needs every digit
		// creates the destination itself (e.g. with a text column).
		// timestamptz is kept: bytdb parses it and folds it into timestamp.
		return [...]string{"text", "bigint", "double precision", "double precision", "boolean",
			"date", "timestamp", "timestamptz", "bytea"}[f]
	}
	// Postgres.
	return [...]string{"text", "bigint", "double precision", "numeric", "boolean",
		"date", "timestamp", "timestamptz", "bytea"}[f]
}
