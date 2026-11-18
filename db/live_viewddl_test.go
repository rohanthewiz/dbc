package db

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/config"
)

// A view's DDL read from live servers (N-243): the unit tests check the
// statements' text only, and the sidebar's Views section asks for this on
// Postgres and MySQL, which the embedded engines stand in for elsewhere.
// Opt-in, like every live test (live_test.go): DBC_LIVE_PG_DSN and
// DBC_LIVE_MYSQL_DSN.

// On Postgres: a plain view (CREATE OR REPLACE VIEW), a materialized one
// (CREATE MATERIALIZED VIEW, no OR REPLACE), and a view in a mixed-case
// schema with a spaced name, quoted by pgIdent. Each statement read back
// is run again on a fresh name to prove it is SQL the server takes.
func TestLiveViewDDLPostgres(t *testing.T) {
	mgr := liveMgr(t, "DBC_LIVE_PG_DSN", "postgres")
	ctx := context.Background()
	liveExec(t, mgr,
		`DROP SCHEMA IF EXISTS dbc_live_v CASCADE`,
		`DROP SCHEMA IF EXISTS "DbcLive V" CASCADE`,
		`CREATE SCHEMA dbc_live_v`,
		`CREATE SCHEMA "DbcLive V"`,
		`CREATE TABLE dbc_live_v.orders (id int PRIMARY KEY, total numeric)`,
		`CREATE VIEW dbc_live_v.big_orders AS SELECT id, total FROM dbc_live_v.orders WHERE total > 100`,
		`CREATE MATERIALIZED VIEW dbc_live_v.order_sums AS SELECT count(*) AS n, sum(total) AS s FROM dbc_live_v.orders`,
		`CREATE VIEW "DbcLive V"."Big Orders" AS SELECT id FROM dbc_live_v.orders`,
	)
	t.Cleanup(func() {
		liveExec(t, mgr, `DROP SCHEMA IF EXISTS dbc_live_v CASCADE`, `DROP SCHEMA IF EXISTS "DbcLive V" CASCADE`)
	})

	cases := []struct {
		view TableRef
		head string // the statement's first line
		body string // a piece of the query it must hold
	}{
		{TableRef{Schema: "dbc_live_v", Name: "big_orders", View: true},
			"CREATE OR REPLACE VIEW dbc_live_v.big_orders AS", "total > 100"},
		{TableRef{Schema: "dbc_live_v", Name: "order_sums", View: true},
			"CREATE MATERIALIZED VIEW dbc_live_v.order_sums AS", "sum(total)"},
		{TableRef{Schema: "DbcLive V", Name: "Big Orders", View: true},
			`CREATE OR REPLACE VIEW "DbcLive V"."Big Orders" AS`, "FROM dbc_live_v.orders"},
	}
	for _, c := range cases {
		ddl, err := mgr.ViewDDL(ctx, "live", c.view)
		if err != nil {
			t.Errorf("%s: %v", c.view.Name, err)
			continue
		}
		t.Logf("%s:\n%s", c.view.Name, ddl)
		head, _, _ := strings.Cut(ddl, "\n")
		if head != c.head || !strings.Contains(ddl, c.body) || !strings.HasSuffix(ddl, ";") {
			t.Errorf("%s:\n%s", c.view.Name, ddl)
		}
		// the DDL runs: the same statement under another name
		again := strings.Replace(ddl, c.view.Name, c.view.Name+"_again", 1)
		if c.view.Name == "Big Orders" {
			again = strings.Replace(ddl, `"Big Orders"`, `"Big Orders again"`, 1)
		}
		if _, err := mgr.Run("live", again); err != nil {
			t.Errorf("%s: its DDL does not run: %v\n%s", c.view.Name, err, again)
		}
	}

	// a view that is gone is an error that says so
	if _, err := mgr.ViewDDL(ctx, "live", TableRef{Schema: "dbc_live_v", Name: "nope", View: true}); err == nil ||
		!strings.Contains(err.Error(), "no such view") {
		t.Errorf("a missing view: %v", err)
	}
}

// On MySQL: SHOW CREATE VIEW's second column, and a user without SHOW VIEW
// gets the server's own refusal rather than an empty statement.
func TestLiveViewDDLMySQL(t *testing.T) {
	mgr := liveMgr(t, "DBC_LIVE_MYSQL_DSN", "mysql")
	ctx := context.Background()
	liveExec(t, mgr,
		"DROP VIEW IF EXISTS dbc_live_v_big",
		"DROP TABLE IF EXISTS dbc_live_v_orders",
		"CREATE TABLE dbc_live_v_orders (id int PRIMARY KEY, total decimal(10,2))",
		"CREATE VIEW dbc_live_v_big AS SELECT id, total FROM dbc_live_v_orders WHERE total > 100",
	)
	t.Cleanup(func() {
		liveExec(t, mgr, "DROP VIEW IF EXISTS dbc_live_v_big", "DROP TABLE IF EXISTS dbc_live_v_orders",
			"DROP USER IF EXISTS 'dbc_live_noview'@'%'")
	})
	ddl, err := mgr.ViewDDL(ctx, "live", TableRef{Name: "dbc_live_v_big", View: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("MySQL view:\n%s", ddl)
	if !strings.HasPrefix(ddl, "CREATE ") || !strings.Contains(ddl, "VIEW `dbc_live_v_big` AS") ||
		!strings.Contains(ddl, "> 100") || !strings.HasSuffix(ddl, ";") {
		t.Errorf("view DDL:\n%s", ddl)
	}

	// A user with SELECT on the view but no SHOW VIEW. The DSN's own user
	// creates it (a throwaway server's root), so this needs CREATE USER.
	dsn := liveDSN(t, "DBC_LIVE_MYSQL_DSN")
	db := currentDB(t, mgr)
	liveExec(t, mgr,
		"DROP USER IF EXISTS 'dbc_live_noview'@'%'",
		"CREATE USER 'dbc_live_noview'@'%' IDENTIFIED BY 'nv-pw-1'",
		fmt.Sprintf("GRANT SELECT ON `%s`.`dbc_live_v_big` TO 'dbc_live_noview'@'%%'", db),
	)
	noview := NewManager(&config.Config{MaxRows: 10, Connections: []config.Connection{{
		Name: "nv", Driver: "mysql", DSN: withMySQLUser(t, dsn, "dbc_live_noview", "nv-pw-1")}}})
	t.Cleanup(noview.Close)
	_, err = noview.ViewDDL(ctx, "nv", TableRef{Name: "dbc_live_v_big", View: true})
	t.Logf("without SHOW VIEW: %v", err)
	if err == nil || !strings.Contains(err.Error(), "SHOW VIEW") {
		t.Errorf("no SHOW VIEW: %v, want the server's refusal naming SHOW VIEW", err)
	}
}

// liveDSN is the DSN env holds (liveMgr has skipped without it).
func liveDSN(t *testing.T, env string) string {
	t.Helper()
	return os.Getenv(env)
}

// currentDB is the MySQL connection's database.
func currentDB(t *testing.T, mgr *Manager) string {
	t.Helper()
	res, err := mgr.Run("live", "SELECT DATABASE()")
	if err != nil || len(res.Rows) == 0 {
		t.Fatalf("SELECT DATABASE(): %v", err)
	}
	return res.Rows[0][0]
}

// withMySQLUser is a go-sql-driver DSN (user:pass@tcp(host)/db?…) with its
// user and password replaced.
func withMySQLUser(t *testing.T, dsn, user, pass string) string {
	t.Helper()
	at := strings.LastIndex(dsn, "@")
	if at < 0 {
		t.Fatalf("no @ in the MySQL DSN")
	}
	return url.UserPassword(user, pass).String() + dsn[at:]
}
