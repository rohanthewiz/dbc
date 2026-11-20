package sdb

import (
	"fmt"
	"os"
	"testing"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/model"
)

// The script API over a statement with several result sets, and over one
// whose rows Postgres also counts (N-317), against real servers: which
// sets a CALL returns, and the command tag's count, are the server's and
// the driver's doing. Opt-in, with the DSN variables of db's live tests.

// liveScriptMgr is a Manager with one connection, "live", on the server
// env names, or a skip when it is not set.
func liveScriptMgr(t *testing.T, env, driver string) *db.Manager {
	t.Helper()
	dsn := os.Getenv(env)
	if dsn == "" {
		t.Skipf("set %s to run against a live %s", env, driver)
	}
	mgr := db.NewManager(&config.Config{MaxRows: 1000,
		Connections: []config.Connection{{Name: "live", Driver: driver, DSN: dsn}}})
	t.Cleanup(mgr.Close)
	return mgr
}

// A script that runs a MySQL CALL whose procedure SELECTs twice and shows
// the result shows both sets, each a result of its own, in order — where
// it used to show the first alone.
func TestLiveShowEverySetMySQL(t *testing.T) {
	mgr := liveScriptMgr(t, "DBC_LIVE_MYSQL_DSN", "mysql")
	for _, q := range []string{
		`DROP PROCEDURE IF EXISTS dbc_live_sdb_two`,
		`CREATE PROCEDURE dbc_live_sdb_two() BEGIN SELECT 1 AS a; SELECT 2 AS b, 3 AS c; END`,
	} {
		if _, err := mgr.Run("live", q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	t.Cleanup(func() { _, _ = mgr.Run("live", `DROP PROCEDURE IF EXISTS dbc_live_sdb_two`) })

	var shown []*model.Result
	s := New(mgr, func(r *model.Result) { shown = append(shown, r) }, nil)
	r, err := s.Query("live", `CALL dbc_live_sdb_two()`)
	if err != nil {
		t.Fatal(err)
	}
	s.Show(r)
	var got []string
	for _, x := range shown {
		got = append(got, fmt.Sprint(x.Columns, x.Rows))
	}
	if want := "[[a] [[1]] [b c] [[2 3]]]"; fmt.Sprint(got) != want {
		t.Errorf("shown: %v\n want %v", got, want)
	}
}

// A script's s.Exec of a Postgres FETCH hands back the rows it fetched,
// and of an EXECUTE of a prepared SELECT its rows' count, as each did when
// it ran as an Exec — not the 0 of a statement that returned rows. A MOVE,
// rowless, counts as it always did; a plain SELECT through Exec gives 0.
//
// A script has no session of its own: each statement takes a pooled
// connection. The pool is held to one connection here so that the cursor
// and the prepared statement are there for the statements after them,
// and the cursor is WITH HOLD, so it needs no open transaction.
func TestLiveExecCountsFetch(t *testing.T) {
	mgr := liveScriptMgr(t, "DBC_LIVE_PG_DSN", "postgres")
	dbh, err := mgr.DB("live")
	if err != nil {
		t.Fatal(err)
	}
	dbh.SetMaxOpenConns(1)
	s := New(mgr, nil, nil)
	exec := func(stmt string) int64 {
		t.Helper()
		n, err := s.Exec("live", stmt)
		if err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
		return n
	}
	exec(`DECLARE dbc_live_sdb_c CURSOR WITH HOLD FOR SELECT g FROM generate_series(1, 10) g`)
	t.Cleanup(func() { _, _ = s.Exec("live", `CLOSE dbc_live_sdb_c`) })
	exec(`PREPARE dbc_live_sdb_q AS SELECT g FROM generate_series(1, 4) g`)
	t.Cleanup(func() { _, _ = s.Exec("live", `DEALLOCATE dbc_live_sdb_q`) })

	for _, c := range []struct {
		stmt string
		want int64
	}{
		{`FETCH 3 FROM dbc_live_sdb_c`, 3},
		{`MOVE 2 IN dbc_live_sdb_c`, 2},
		{`FETCH 10 FROM dbc_live_sdb_c`, 5}, // only five were left
		{`EXECUTE dbc_live_sdb_q`, 4},
		{`SELECT 1`, 0},
	} {
		if n := exec(c.stmt); n != c.want {
			t.Errorf("s.Exec(%q) = %d, want %d", c.stmt, n, c.want)
		}
	}
	// and Query keeps the rows, the count beside them
	r, err := s.Query("live", `EXECUTE dbc_live_sdb_q`)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Rows) != 4 || r.IsExec || r.Affected != 4 {
		t.Errorf("Query of EXECUTE: %d rows, IsExec %v, Affected %d", len(r.Rows), r.IsExec, r.Affected)
	}
}
