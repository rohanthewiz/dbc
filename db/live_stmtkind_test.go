package db

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/model"
)

// The live statement-kind tests run the statements whose verb does not say
// what they return (kindMaybe: stmtkind.go) against real servers, since
// what comes back — a result set, several, or only a count — is the
// server's and the driver's doing, which no SQLite test can stand in for.
// Opt-in like live_test.go, with the same DSN variables. They create and
// drop a dbc_live_kind table and dbc_live_kind_* procedures.

// kindShape is a result in one line, for comparing: "rows [id] [[1] [2]]"
// for each result set, or "affected 3".
func kindShape(r *model.Result) string {
	if r.IsExec {
		return fmt.Sprintf("affected %d", r.Affected)
	}
	var b strings.Builder
	for i, set := range r.Sets() {
		if i > 0 {
			b.WriteString(" + ")
		}
		fmt.Fprintf(&b, "rows %v %v", set.Columns, set.Rows)
	}
	return b.String()
}

// kindStep is a statement and the shape of its result.
type kindStep struct{ stmt, want string }

// kindOnSession runs steps in order on one Session — the cursor and
// prepared-statement steps need its connection — and checks each shape.
func kindOnSession(t *testing.T, mgr *Manager, steps []kindStep) {
	t.Helper()
	ctx := context.Background()
	sess, err := mgr.Session(ctx, "live")
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer sess.Close()
	for _, s := range steps {
		res, err := sess.Run(ctx, s.stmt)
		if err != nil {
			t.Fatalf("%s: %v", s.stmt, err)
		}
		if got := kindShape(res); got != s.want {
			t.Errorf("%s\n  got  %s\n  want %s", s.stmt, got, s.want)
		}
	}
}

// kindOnPool runs one statement on the pool and checks its shape.
func kindOnPool(t *testing.T, mgr *Manager, want, stmt string, args ...any) {
	t.Helper()
	res, err := mgr.RunContext(context.Background(), "live", stmt, args...)
	if err != nil {
		t.Fatalf("%s: %v", stmt, err)
	}
	if got := kindShape(res); got != want {
		t.Errorf("%s (pool)\n  got  %s\n  want %s", stmt, got, want)
	}
}

// On Postgres a CALL with an INOUT parameter, an EXECUTE of a prepared
// SELECT and a FETCH hand back their rows — each used to report "n
// affected" — and the ones that return none still report their count: an
// EXECUTE of a prepared INSERT says how many rows it wrote.
func TestLiveStmtKindPostgres(t *testing.T) {
	mgr := liveMgr(t, "DBC_LIVE_PG_DSN", "postgres")
	drop := []string{
		`DROP PROCEDURE IF EXISTS dbc_live_kind_io(int)`,
		`DROP PROCEDURE IF EXISTS dbc_live_kind_w()`,
		`DROP TABLE IF EXISTS dbc_live_kind`,
	}
	liveExec(t, mgr, drop...)
	t.Cleanup(func() {
		for _, s := range drop {
			_, _ = mgr.Run("live", s)
		}
	})
	liveExec(t, mgr,
		`CREATE TABLE dbc_live_kind (id int PRIMARY KEY, v text)`,
		`INSERT INTO dbc_live_kind VALUES (1, 'a'), (2, 'b'), (3, 'c')`,
		`CREATE PROCEDURE dbc_live_kind_io(INOUT x int) LANGUAGE plpgsql AS $$ BEGIN x := x * 2; END $$`,
		// commits inside: a procedure may, when its CALL is not itself
		// inside a transaction — which the Query path must not put it in
		`CREATE PROCEDURE dbc_live_kind_w() LANGUAGE plpgsql AS $$ BEGIN UPDATE dbc_live_kind SET v = v || '!'; COMMIT; END $$`,
	)

	kindOnSession(t, mgr, []kindStep{
		{`CALL dbc_live_kind_io(21)`, "rows [x] [[42]]"},
		// "CALL" is the whole command tag: Postgres gives a CALL no count
		{`CALL dbc_live_kind_w()`, "affected 0"},
		{`SELECT v FROM dbc_live_kind WHERE id = 1`, "rows [v] [[a!]]"},

		{`PREPARE dbc_live_q AS SELECT id FROM dbc_live_kind ORDER BY id`, "affected 0"},
		{`EXECUTE dbc_live_q`, "rows [id] [[1] [2] [3]]"},
		{`PREPARE dbc_live_ins AS INSERT INTO dbc_live_kind VALUES (8, 'h'), (9, 'i')`, "affected 0"},
		{`EXECUTE dbc_live_ins`, "affected 2"},
		{`DELETE FROM dbc_live_kind WHERE id > 3`, "affected 2"},

		{`BEGIN`, "affected 0"},
		{`DECLARE dbc_live_c CURSOR FOR SELECT id FROM dbc_live_kind ORDER BY id`, "affected 0"},
		{`FETCH 2 FROM dbc_live_c`, "rows [id] [[1] [2]]"},
		{`FETCH 2 FROM dbc_live_c`, "rows [id] [[3]]"},
		{`CLOSE dbc_live_c`, "affected 0"},
		// The same text, over another query: the rows must come back
		// under THIS cursor's columns. With the statement prepared once
		// and its columns remembered (pgx's default mode) they arrived
		// under the first cursor's, and failed to decode.
		{`DECLARE dbc_live_c CURSOR FOR SELECT v, id FROM dbc_live_kind ORDER BY id`, "affected 0"},
		{`FETCH 2 FROM dbc_live_c`, "rows [v id] [[a! 1] [b! 2]]"},
		{`COMMIT`, "affected 0"},

		// rowless on Postgres, a result set on MySQL: a kindMaybe on both
		{`ANALYZE dbc_live_kind`, "affected 0"},
	})

	// on the pool, and with an argument
	kindOnPool(t, mgr, "rows [x] [[42]]", `CALL dbc_live_kind_io(21)`)
	kindOnPool(t, mgr, "rows [x] [[10]]", `CALL dbc_live_kind_io($1)`, 5)
	// through RunNotices, the scripts' path
	res, _, err := mgr.RunNotices(context.Background(), "live", `CALL dbc_live_kind_io(4)`)
	if err != nil || kindShape(res) != "rows [x] [[8]]" {
		t.Errorf("RunNotices CALL: %v, %v", res, err)
	}
	// two statements in one text keep to the simple protocol, which takes
	// them; the extended protocol of a Query would refuse the pair
	kindOnPool(t, mgr, "affected 0", `CALL dbc_live_kind_io(1); CALL dbc_live_kind_io(2)`)
}

// On MySQL a CALL hands back every result set its procedure SELECTs, and
// the table-maintenance statements their report; a CALL or an EXECUTE that
// only writes still reports rows affected, on the pool as on a session.
func TestLiveStmtKindMySQL(t *testing.T) {
	mgr := liveMgr(t, "DBC_LIVE_MYSQL_DSN", "mysql")
	drop := []string{
		`DROP PROCEDURE IF EXISTS dbc_live_kind_two`,
		`DROP PROCEDURE IF EXISTS dbc_live_kind_w`,
		`DROP PROCEDURE IF EXISTS dbc_live_kind_arg`,
		`DROP PROCEDURE IF EXISTS dbc_live_kind_fail`,
		`DROP TABLE IF EXISTS dbc_live_kind`,
	}
	liveExec(t, mgr, drop...)
	t.Cleanup(func() {
		for _, s := range drop {
			_, _ = mgr.Run("live", s)
		}
	})
	liveExec(t, mgr,
		`CREATE TABLE dbc_live_kind (id int PRIMARY KEY, v varchar(40))`,
		`INSERT INTO dbc_live_kind VALUES (1, 'a'), (2, 'b'), (3, 'c')`,
		`CREATE PROCEDURE dbc_live_kind_two() BEGIN
			SELECT id FROM dbc_live_kind ORDER BY id;
			SELECT v, id FROM dbc_live_kind WHERE id > 1 ORDER BY id;
		END`,
		`CREATE PROCEDURE dbc_live_kind_w() BEGIN UPDATE dbc_live_kind SET v = CONCAT(v, '!'); END`,
		`CREATE PROCEDURE dbc_live_kind_arg(IN n int) BEGIN
			SELECT id FROM dbc_live_kind WHERE id >= n ORDER BY id;
			SELECT count(*) AS total FROM dbc_live_kind;
		END`,
		// a first result set, then an error: the statement fails
		`CREATE PROCEDURE dbc_live_kind_fail() BEGIN
			SELECT 1 AS one;
			SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'dbc_live_kind raised';
		END`,
	)

	const two = "rows [id] [[1] [2] [3]] + rows [v id] [[b 2] [c 3]]"
	kindOnPool(t, mgr, two, `CALL dbc_live_kind_two()`)
	// an argument sends the CALL through the prepared-statement protocol,
	// whose result sets are read by another part of the driver
	kindOnPool(t, mgr, "rows [id] [[2] [3]] + rows [total] [[3]]", `CALL dbc_live_kind_arg(?)`, 2)
	// the count is ROW_COUNT()'s, read on the connection the CALL ran on
	kindOnPool(t, mgr, "affected 3", `CALL dbc_live_kind_w()`)

	kindOnSession(t, mgr, []kindStep{
		{`CALL dbc_live_kind_two()`, "rows [id] [[1] [2] [3]] + rows [v id] [[b! 2] [c! 3]]"},
		{`CALL dbc_live_kind_w()`, "affected 3"},
		{`PREPARE dbc_live_s FROM 'UPDATE dbc_live_kind SET v = ''k'' WHERE id < 3'`, "affected 0"},
		{`EXECUTE dbc_live_s`, "affected 2"},
		{`PREPARE dbc_live_r FROM 'SELECT id FROM dbc_live_kind WHERE id < 3 ORDER BY id'`, "affected 0"},
		{`EXECUTE dbc_live_r`, "rows [id] [[1] [2]]"},
		// a statement in no list of verbs, and rowless
		{`FLUSH TABLES`, "affected 0"},
	})

	// the maintenance statements report in rows; what the report says is
	// the server's, so only its columns are checked
	for _, stmt := range []string{`CHECK TABLE dbc_live_kind`, `ANALYZE TABLE dbc_live_kind`, `OPTIMIZE TABLE dbc_live_kind`} {
		res, err := mgr.Run("live", stmt)
		if err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
		if res.IsExec || fmt.Sprint(res.Columns) != "[Table Op Msg_type Msg_text]" || len(res.Rows) == 0 {
			t.Errorf("%s: got %s, want the statement's report as rows", stmt, kindShape(res))
		}
	}

	// a procedure that raises after its first result set fails the
	// statement, with the procedure's message
	if res, err := mgr.Run("live", `CALL dbc_live_kind_fail()`); err == nil || !strings.Contains(err.Error(), "dbc_live_kind raised") {
		t.Errorf("CALL of a procedure that raises: result %v, err %v; want its error", res, err)
	}
	// and the connection it failed on is fit for the next statement
	kindOnPool(t, mgr, "rows [id] [[1] [2] [3]] + rows [v id] [[k 2] [c!! 3]]", `CALL dbc_live_kind_two()`)
}
