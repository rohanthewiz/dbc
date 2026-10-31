package sqlsplit

import "strings"

// Dialect is the engine a buffer is scanned as. The engines dbc speaks
// agree on nearly everything the scanner looks at — comments, quoted
// names, dollar quotes are read the same whichever engine is behind them —
// and part on one thing only: what a backslash does inside a '…' string.
//
//	           'it''s'   'it\'s'                  'C:\'
//	MySQL      it's      it's  (\ escapes)        unterminated: \' is a quote
//	Postgres   it's      'it\' then s'…           C:\  (\ is plain text)
//	bytdb      it's      as Postgres              as Postgres
//	SQLite     it's      as Postgres              as Postgres
//
// Postgres and bytdb have a second kind of string, E'…' (or e'…'), in which
// a backslash escapes as it does on MySQL; SQLite has none. A scan that
// reads every string by MySQL's rule runs on past the closing quote of any
// Postgres, bytdb or SQLite string ending in a backslash — 'a\' — and with
// it goes the semicolon after it, so two statements merge into one and the
// editor colors everything that follows as a string. A scan that reads
// every string by the others' rule does the same with MySQL's 'it\'s'. So
// the rule is the engine's, and the caller, which knows the connection,
// says which: DialectOf(driver).Split(sql).
//
// The zero value is Generic, the rule for SQL whose engine is not known (a
// log line being formatted, a script's text): a backslash escapes in every
// '…', as on MySQL. That is the rule every scan followed before dialects
// existed, which is why the package-level functions (Split, Lex, …) keep
// it — they are Generic's methods under their old names, so a caller that
// has no engine to name reads SQL exactly as it always did. Generic over
// the stricter rule because of what each gets wrong on the other's SQL:
// Generic misreads only a string that ends in a backslash, while the
// stricter rule would misread every escaped quote in a MySQL string, a far
// commoner sight ('it\'s', 'O\'Brien').
type Dialect uint8

const (
	Generic  Dialect = iota // engine unknown: a backslash escapes in every '…'
	Postgres                // '' only; a backslash escapes in E'…' alone
	MySQL                   // a backslash escapes in every '…' (no NO_BACKSLASH_ESCAPES)
	SQLite                  // '' only, everywhere
	Bytdb                   // as Postgres: '' only, a backslash escapes in E'…'
)

// DialectOf is the dialect of a driver name: a connection's as configured
// (postgres, pg, mysql, sqlite3 …), the canonical one db.Driver resolves it
// to (pgx, mysql, sqlite, bytdb), or a code fence's language tag naming an
// engine (postgresql, psql, plpgsql …). Case does not matter. Anything else
// — "", "sql", an engine dbc does not speak — is Generic.
//
// The names are listed here rather than taken from db, which imports this
// package: the alias list is db's driverFor's plus the fence tags codehl
// reads as SQL, and a name missing from it only costs Generic's reading.
func DialectOf(driver string) Dialect {
	switch strings.ToLower(strings.TrimSpace(driver)) {
	case "postgres", "postgresql", "pg", "pgx", "psql", "pgsql", "plpgsql":
		return Postgres
	case "mysql", "mariadb":
		return MySQL
	case "sqlite", "sqlite3":
		return SQLite
	case "bytdb":
		return Bytdb
	}
	return Generic
}

// String is the dialect's name, as DialectOf reads it back.
func (d Dialect) String() string {
	switch d {
	case Postgres:
		return "postgres"
	case MySQL:
		return "mysql"
	case SQLite:
		return "sqlite"
	case Bytdb:
		return "bytdb"
	}
	return "generic"
}

// backslashEscapes reports whether a backslash escapes the byte after it
// inside the quoted run that opens at s[i] (q, the quote byte). Only a
// single-quoted string ever has escapes: a "…" or `…` name doubles its
// quote on every engine, and a backslash in it is plain text.
//
// An escape string is an E or e directly before the quote, standing as a
// token of its own: `E'a\'b'` is one, while in `type'x'` (no such SQL, but
// a scanner must not care) the e ends a word and the string is a plain one.
// That is how Postgres's and bytdb's lexers read it — the prefix is only
// looked for where a token starts.
func (d Dialect) backslashEscapes(s string, i int, q byte) bool {
	if q != '\'' {
		return false
	}
	switch d {
	case Postgres, Bytdb:
		return i > 0 && (s[i-1] == 'E' || s[i-1] == 'e') && (i == 1 || !isWordByte(s[i-2]))
	case SQLite:
		return false
	}
	return true // Generic, MySQL
}
