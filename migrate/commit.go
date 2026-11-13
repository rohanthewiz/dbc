package migrate

import (
	"errors"
	"fmt"
	"strings"

	"github.com/go-sql-driver/mysql"

	"github.com/rohanthewiz/dbc/sqlsplit"
)

// A migration runs in a transaction so that a failure half-way leaves
// nothing behind. On MySQL that promise does not hold: DDL commits as it
// runs (MySQL's "implicit commit" statements), so the rollback after a
// failure undoes only what came after the last commit — and re-running `up`
// then fails on the first statement ("table exists") with nothing to say the
// migration was half applied. The runner cannot undo those statements, but
// it can say which ones stay, so the repair starts from the facts.
//
// MySQL's rule, checked against 8.4:
//
//   - Before an implicit-commit statement runs, everything the transaction
//     did is committed — even when the statement then fails at execution
//     (a table that exists, a duplicate column).
//   - After it, the session is back in autocommit: the transaction
//     database/sql began is over, so every later statement commits on its
//     own as it runs, and the final ROLLBACK has nothing to undo.
//   - A statement the parser refuses (a syntax error) never runs, so it
//     commits nothing.
//
// So, for a failure at statement i of a migration:
//
//	INSERT   CREATE   INSERT   ALTER ✗           3 stay: CREATE committed the
//	└──────── committed ─────┘                     first INSERT, autocommit
//	                                               the second
//	INSERT   ALTER ✗ (column exists)             1 stays: ALTER committed it
//	                                               before failing
//	INSERT   ALTER ✗ (syntax error)              0: the parse failed first
//	INSERT   CREATE TEMPORARY TABLE   UPDATE ✗   0: TEMPORARY does not commit

// mysqlImplicitCommit holds the leading verbs of MySQL's implicit-commit
// statements that a migration might contain: DDL, account management,
// transaction and lock control, and table maintenance. Replication control
// and LOAD INDEX INTO CACHE also commit but have no place in a migration;
// SET autocommit = 1 and SET PASSWORD commit too, but `set` is mostly
// session variables, which do not. LOAD DATA commits only on NDB tables.
// The reading is lexical: a CALL whose procedure runs DDL is not seen.
var mysqlImplicitCommit = map[string]bool{
	"alter": true, "create": true, "drop": true, "rename": true, "truncate": true,
	"grant": true, "revoke": true, "install": true, "uninstall": true,
	"begin": true, "start": true, "lock": true, "unlock": true,
	"analyze": true, "cache": true, "check": true, "flush": true,
	"optimize": true, "repair": true, "reset": true,
}

// mysqlCommits reports whether MySQL commits implicitly around stmt.
func mysqlCommits(stmt string) bool {
	// sqlsplit.MySQL, not MySQL.split: MySQL's own initializer names this func,
	// and Go refuses the initialization cycle
	v := sqlsplit.MySQL.Verbs(stmt)
	if !mysqlImplicitCommit[v.Main] {
		return false
	}
	if v.Main == "create" || v.Main == "drop" {
		// CREATE / DROP TEMPORARY TABLE are the exception: a temporary
		// table is the session's own, and MySQL leaves the transaction be
		if f := strings.Fields(stmt[v.MainAt:]); len(f) > 1 && strings.EqualFold(f[1], "temporary") {
			return false
		}
	}
	return true
}

// keptAfterFailure is how many of stmts stay applied after statement i
// (0-based; len(stmts) when the version row or the commit failed) failed
// inside the migration's transaction and the transaction was rolled back.
// It is 0 on an engine whose DDL is transactional. See the rule above.
func (d Dialect) keptAfterFailure(stmts []string, i int, err error) int {
	if d.commits == nil {
		return 0
	}
	for j := 0; j < i && j < len(stmts); j++ {
		if d.commits(stmts[j]) {
			return i // committed through j, autocommitted after it
		}
	}
	if i < len(stmts) && d.commits(stmts[i]) && !isSyntaxError(err) {
		return i // the failing statement committed what came before it
	}
	return 0
}

// isSyntaxError reports a MySQL parse error: 1064 (ER_PARSE_ERROR) and its
// older form 1149 (ER_SYNTAX_ERROR). The statement was refused before it ran,
// so its implicit commit never happened.
func isSyntaxError(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && (me.Number == 1064 || me.Number == 1149)
}

// partlyApplied says which statements of a failed migration stay applied,
// why, and what the version table makes of it — the line a hand repairing
// the database starts from.
func partlyApplied(kept, n int, why, dir string) string {
	which, them := fmt.Sprintf("statements 1-%d of %d stay", kept, n), "them"
	if kept == 1 {
		which, them = fmt.Sprintf("statement 1 of %d stays", n), "it"
	}
	// the version row is written only on success, so it still shows the
	// state from before the run
	listed := "pending"
	if dir == "down" {
		listed = "applied"
	}
	return fmt.Sprintf("%s applied (%s); the version table still lists the migration as %s, "+
		"so undo or finish %s by hand before migrating %s again", which, why, listed, them, dir)
}
