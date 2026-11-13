package db

import (
	"slices"

	"github.com/rohanthewiz/dbc/explain"
	"github.com/rohanthewiz/dbc/sqlsplit"
)

// What a statement may leave behind once it returns, as far as its text
// tells. Three questions hang on it:
//
//	Session.Run        stateful after anything but effectNone, so a cut
//	                   connection is reported lost (ErrSessionLost), never
//	                   silently replaced by one without the state
//	ChangesRows        a recount after effectAny only (and not after the
//	                   session verbs, SET or BEGIN)
//	Session.Explain    MySQL, SQLite and bytdb analyze effectNone only; an
//	                   analyze on Postgres marks the session stateful after
//	                   effectSession (an advisory lock outlives the rollback)
//
// The verb decides most statements: anything that is not a read
// (queryVerbs) is effectAny, and so is a read whose WITH holds a write.
// What this adds is the reads that are not plain (N-272) — a SELECT is a
// read by its verb, yet:
//
//	SELECT set_config('search_path', 'x', false)  a setting      effectSession
//	SELECT pg_advisory_lock(42)                   a lock         effectSession
//	SELECT GET_LOCK('job', 0)              MySQL  a lock         effectSession
//	SELECT @v := 1                         MySQL  a variable     effectSession
//	SELECT … INTO t FROM …                 PG     a new table    effectAny
//	SELECT … INTO @v / OUTFILE …           MySQL  a variable, a file
//	PRAGMA foreign_keys = ON               SQLite a setting      effectSession
//	EXPLAIN ANALYZE DELETE …                      the DELETE     its inner's
//
// Before, each of these counted as a plain read: after the server cut the
// session while it sat idle, the next statement was replayed on a fresh
// one (FaultRetry) without the setting, the lock or the variable — the
// case ErrSessionLost exists to stop, and a silent one: the replay runs
// under the default search_path, or without foreign keys, or believing it
// holds a lock nobody holds.
//
// It is lexical, so it sees what the statement's own text calls. A view or
// a function that sets state out of sight is not seen; nor is one that
// writes, which is why Postgres analyzes every statement inside a rollback
// (explainPostgres) rather than trusting effectNone.
type effect uint8

const (
	effectNone    effect = iota // a plain read: nothing outlives it
	effectSession               // state on the session only: a setting, a lock, a variable
	effectAny                   // anything at all: a write, DDL, a transaction, a SET
)

// sessionFuncs are the functions whose call leaves state on the session —
// state a replacement session lacks without anything saying so. Keys are
// lowercase, as sqlsplit.Calls gives names; one set serves every engine,
// since a user function of the same name elsewhere only errs toward
// "stateful".
//
//	set_config          SET by another name (with true, SET LOCAL: gone at
//	                    the transaction's end — counted anyway)
//	setseed             the seed random() continues from
//	pg_advisory_lock…   session-level advisory locks, the shared and try
//	pg_try_advisory_…   forms too
//	dblink_connect(_u)  a named connection later dblink calls use
//	get_lock            MySQL's named lock, held until released or the
//	                    session ends
//
// Left out on purpose:
//   - pg_advisory_xact_lock and its kin: inside a transaction the BEGIN has
//     made the session stateful already; outside one the lock is gone when
//     the statement ends.
//   - the unlock functions: they leave the session closer to a fresh one.
//   - nextval: it sets what currval and lastval return, but a session that
//     lost that fails loudly ("currval … is not yet defined in this
//     session") rather than silently, and SELECT nextval(…) is common
//     enough that a "session state" badge after it would cry wolf.
var sessionFuncs = map[string]bool{
	"set_config":                  true,
	"setseed":                     true,
	"pg_advisory_lock":            true,
	"pg_advisory_lock_shared":     true,
	"pg_try_advisory_lock":        true,
	"pg_try_advisory_lock_shared": true,
	"dblink_connect":              true,
	"dblink_connect_u":            true,
	"get_lock":                    true,
}

// pragmaReads are the SQLite pragmas that take an argument in parens and
// only read: the argument names what to read (a table, an index) or caps
// the output (integrity_check(N)). For any other pragma, PRAGMA name(value)
// is the setter's second spelling — PRAGMA foreign_keys(1) is PRAGMA
// foreign_keys = 1.
var pragmaReads = map[string]bool{
	"table_info": true, "table_xinfo": true, "table_list": true,
	"index_info": true, "index_xinfo": true, "index_list": true,
	"foreign_key_list": true, "foreign_key_check": true,
	"integrity_check": true, "quick_check": true,
}

// effectOf classifies stmt (see effect), its strings and comments read as
// d reads them.
//
//	main verb not a read, or a WITH with a write CTE ──► effectAny
//	EXPLAIN without ANALYZE ───────────────────────────► effectNone (nothing runs)
//	EXPLAIN ANALYZE inner ─────────────────────────────► effectOf(inner)
//	PRAGMA with = or a setter's (value) ───────────────► effectSession
//	SHOW, DESCRIBE ────────────────────────────────────► effectNone
//	SELECT, VALUES, TABLE:
//	  INTO in the main statement ──────────────────────► effectAny
//	  calls a sessionFuncs function, anywhere ─────────► effectSession
//	  MySQL, a := anywhere ────────────────────────────► effectSession
//	  else ────────────────────────────────────────────► effectNone
//
// INTO is looked for past the CTE list (Verbs' MainAt) and only after a
// SELECT-like verb: DESCRIBE INSERT INTO t … only explains the INSERT. A
// state function is looked for in the whole statement, CTE bodies and
// subqueries included — `SELECT * FROM jobs WHERE pg_try_advisory_lock(id)`
// takes its locks in a WHERE. The := rule is MySQL's alone: there it can
// only assign a user variable, while Postgres uses := for a named argument
// (f(a := 1)).
func effectOf(stmt string, d sqlsplit.Dialect) effect {
	v := d.Verbs(stmt)
	if !queryVerbs[v.Main] {
		return effectAny
	}
	// A WITH is judged by its CTEs too: Postgres lets one write, so
	// `WITH d AS (DELETE … RETURNING *) SELECT count(*) FROM d` returns
	// rows like a SELECT, yet it deleted them.
	for _, cte := range v.CTEs {
		if returningVerbs[cte] {
			return effectAny
		}
	}
	main := stmt[v.MainAt:]
	switch v.Main {
	case "explain":
		// EXPLAIN ANALYZE runs its statement; a bare EXPLAIN only plans
		// it. Strip from the verb on, so a comment the dialect reads (a
		// MySQL #) is already behind.
		inner, analyze, ok := explain.Strip(main)
		if !ok || !analyze {
			return effectNone
		}
		return effectOf(inner, d)
	case "pragma":
		// No comparisons in a PRAGMA, so any = is the setter's. Some
		// setters write the database file rather than the session
		// (user_version, journal_mode = WAL); none changes a table's rows,
		// which is all effectAny would add.
		if d.HasOp(main, "=") {
			return effectSession
		}
		for _, f := range d.Calls(main) {
			if !pragmaReads[f] {
				return effectSession
			}
		}
		return effectNone
	case "show", "describe", "desc":
		return effectNone
	}
	if d.HasKeyword(main, "into") {
		return effectAny
	}
	if slices.ContainsFunc(d.Calls(stmt), func(f string) bool { return sessionFuncs[f] }) {
		return effectSession
	}
	if d == sqlsplit.MySQL && d.HasOp(stmt, ":=") {
		return effectSession
	}
	return effectNone
}
