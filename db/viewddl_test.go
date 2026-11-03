package db

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/config"
)

// Each engine's view definition statement: names as literals or quoted
// identifiers, never bare, and the column the definition is in.
func TestViewDDLQuery(t *testing.T) {
	v := TableRef{Schema: "sa'les", Name: "order`s", View: true}
	cases := []struct {
		driver, has string
		col         int
	}{
		{"postgres", `n.nspname = 'sa''les' AND c.relname = 'order` + "`" + `s'`, 1},
		{"bytdb", "pg_get_viewdef(c.oid, true)", 1},
		{"mysql", "SHOW CREATE VIEW `sa'les`.`order``s`", 1},
		{"sqlite", `type = 'view' AND name = 'order` + "`" + `s'`, 0},
	}
	for _, c := range cases {
		q, err := ViewDDLQuery(c.driver, v)
		if err != nil || !strings.Contains(q, c.has) {
			t.Errorf("%s: %q, %v; want it to hold %q", c.driver, q, err, c.has)
		}
		if got := ViewDDLColumn(c.driver); got != c.col {
			t.Errorf("%s: column %d, want %d", c.driver, got, c.col)
		}
	}
	if _, err := ViewDDLQuery("postgres", TableRef{}); err == nil {
		t.Error("a view with no name should be refused")
	}
	// a Postgres view with no schema is public's
	if q, _ := ViewDDLQuery("postgres", TableRef{Name: "v"}); !strings.Contains(q, "n.nspname = 'public'") {
		t.Errorf("no schema: %q", q)
	}
}

// The CREATE put around pg_get_viewdef's query: OR REPLACE for a view, none
// for a materialized view (which has no OR REPLACE), names quoted only
// where they must be.
func TestPgViewDDL(t *testing.T) {
	got := pgViewDDL(TableRef{Schema: "public", Name: "totals"}, "v", " SELECT 1;")
	if got != "CREATE OR REPLACE VIEW public.totals AS\nSELECT 1;" {
		t.Errorf("view: %q", got)
	}
	got = pgViewDDL(TableRef{Schema: "Sales", Name: "big totals"}, "m", "SELECT 1;")
	if got != `CREATE MATERIALIZED VIEW "Sales"."big totals" AS`+"\nSELECT 1;" {
		t.Errorf("matview: %q", got)
	}
	for in, want := range map[string]string{"a_1": "a_1", "1a": `"1a"`, `x"y`: `"x""y"`, "": `""`} {
		if got := pgIdent(in); got != want {
			t.Errorf("pgIdent(%q) = %q, want %q", in, got, want)
		}
	}
}

// A view's definition read through the Manager on the two embedded
// engines, as the sidebar asks for it: SQLite's as typed, bytdb's rebuilt
// around pg_get_viewdef, both ending in a semicolon; a view that is not
// there is an error, not an empty statement.
func TestManagerViewDDL(t *testing.T) {
	ctx := context.Background()

	t.Run("bytdb", func(t *testing.T) {
		mgr := bytdbMgr(t)
		for _, s := range []string{
			"CREATE TABLE t (a int PRIMARY KEY)",
			"CREATE VIEW big AS SELECT a FROM t WHERE a > 1",
		} {
			if _, err := mgr.Run("bd", s); err != nil {
				t.Fatalf("%s: %v", s, err)
			}
		}
		ddl, err := mgr.ViewDDL(ctx, "bd", TableRef{Schema: "public", Name: "big", View: true})
		if err != nil {
			t.Fatal(err)
		}
		if ddl != "CREATE OR REPLACE VIEW public.big AS\nSELECT a FROM t WHERE a > 1;" {
			t.Errorf("ddl %q", ddl)
		}
		if _, err := mgr.ViewDDL(ctx, "bd", TableRef{Schema: "public", Name: "gone"}); err == nil {
			t.Error("a missing view should be an error")
		}
	})

	t.Run("sqlite", func(t *testing.T) {
		cfg := &config.Config{MaxRows: 1000, Connections: []config.Connection{{
			Name: "lite", Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "v.db"),
		}}}
		mgr := NewManager(cfg)
		t.Cleanup(mgr.Close)
		for _, s := range []string{
			"CREATE TABLE t (a int)",
			"CREATE VIEW big AS SELECT a FROM t WHERE a > 1",
		} {
			if _, err := mgr.Run("lite", s); err != nil {
				t.Fatalf("%s: %v", s, err)
			}
		}
		ddl, err := mgr.ViewDDL(ctx, "lite", TableRef{Schema: "main", Name: "big", View: true})
		if err != nil {
			t.Fatal(err)
		}
		if ddl != "CREATE VIEW big AS SELECT a FROM t WHERE a > 1;" {
			t.Errorf("ddl %q", ddl)
		}
		if _, err := mgr.ViewDDL(ctx, "lite", TableRef{Name: "t"}); err == nil {
			t.Error("a table is not a view")
		}
	})
}
