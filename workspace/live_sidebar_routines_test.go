package workspace

import (
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/model"
)

// liveRoutine finds a routine of the sidebar's list by name and argument
// list, failing when it is not there.
func liveRoutine(t *testing.T, rs []model.Routine, name, args string) model.Routine {
	t.Helper()
	for _, r := range rs {
		if r.Name == name && r.Args == args {
			return r
		}
	}
	t.Fatalf("no %s(%s) in %+v", name, args, rs)
	return model.Routine{}
}

// liveDDL reads r's DDL through the workspace, failing on an error.
func liveDDL(t *testing.T, w *Workspace, r model.Routine) string {
	t.Helper()
	st, err := w.RoutineDDL(r)
	if err != nil {
		t.Fatal(err)
	}
	ev := st.Job().(*RoutineDDL)
	if ev.Err != nil {
		t.Fatalf("DDL of %s: %v", r.QName(), ev.Err)
	}
	return ev.DDL
}

// THE SIDEBAR'S ROUTINES ON A REAL POSTGRES: switched on, the list is the
// schema the tables are of — public's, not dbc_live_wsl's — with each
// overload its own row; a schema pick reads the other schema's. Each
// row's DDL is the server's CREATE OR REPLACE, the right overload's, ending
// in a semicolon; an aggregate's is refused in words. A CREATE FUNCTION
// run through the workspace relists, and the new function is in the list.
func TestLiveWorkspaceSidebarRoutines(t *testing.T) {
	pgOnly(t, func(t *testing.T, e liveEngine) {
		w, obs := liveWorkspace(t, e)
		drop := []string{
			`DROP SCHEMA IF EXISTS dbc_live_wsl CASCADE`,
			`DROP AGGREGATE IF EXISTS dbc_live_sum(integer)`,
			`DROP FUNCTION IF EXISTS dbc_live_tot(integer), dbc_live_tot(integer, boolean), dbc_live_new()`,
			`DROP PROCEDURE IF EXISTS dbc_live_arch(date)`,
		}
		obsExec(t, obs, drop...)
		obsExec(t, obs,
			`CREATE SCHEMA dbc_live_wsl`,
			`CREATE TABLE dbc_live_wsl.t (id int)`,
			`CREATE FUNCTION dbc_live_tot(o integer) RETURNS numeric LANGUAGE sql AS 'SELECT 1::numeric'`,
			`CREATE FUNCTION dbc_live_tot(o integer, tax boolean) RETURNS numeric LANGUAGE sql AS 'SELECT 2::numeric'`,
			`CREATE PROCEDURE dbc_live_arch(before date) LANGUAGE sql AS 'SELECT 1'`,
			`CREATE AGGREGATE dbc_live_sum(integer) (SFUNC = int4pl, STYPE = integer)`,
			`CREATE FUNCTION dbc_live_wsl.slug(s text) RETURNS text LANGUAGE sql AS 'SELECT lower(s)'`)
		t.Cleanup(func() {
			for _, s := range drop {
				_, _ = obs.Run("live", s)
			}
		})
		reconnectAndLoad(t, w) // a schema list with dbc_live_wsl in it

		j := w.ShowRoutines(true)
		if j == nil {
			t.Fatal("no Job switching the list on")
		}
		ev := j().(*RoutinesLoaded)
		if ev.Stale || len(ev.Notes) > 0 || ev.Schema != w.CatalogSchema() {
			t.Fatalf("landed %+v (catalog schema %q)", ev, w.CatalogSchema())
		}
		rs := w.Routines()
		one := liveRoutine(t, rs, "dbc_live_tot", "o integer")
		two := liveRoutine(t, rs, "dbc_live_tot", "o integer, tax boolean")
		arch := liveRoutine(t, rs, "dbc_live_arch", "IN before date") // a procedure's modes are spelled out
		agg := liveRoutine(t, rs, "dbc_live_sum", "integer")
		for _, r := range rs {
			if r.Schema != w.CatalogSchema() {
				t.Errorf("%s listed beside %s's tables", r.QName(), w.CatalogSchema())
			}
		}

		if d := liveDDL(t, w, one); !strings.HasPrefix(d, "CREATE OR REPLACE FUNCTION public.dbc_live_tot(o integer)") ||
			!strings.Contains(d, "SELECT 1::numeric") || !strings.HasSuffix(d, ";") {
			t.Errorf("one-argument DDL:\n%s", d)
		}
		if d := liveDDL(t, w, two); !strings.Contains(d, "tax boolean") || !strings.Contains(d, "SELECT 2::numeric") {
			t.Errorf("two-argument DDL:\n%s", d)
		}
		if d := liveDDL(t, w, arch); !strings.HasPrefix(d, "CREATE OR REPLACE PROCEDURE public.dbc_live_arch") {
			t.Errorf("procedure DDL:\n%s", d)
		}
		st, err := w.RoutineDDL(agg)
		if err != nil {
			t.Fatal(err)
		}
		if ev := st.Job().(*RoutineDDL); ev.Err == nil || !strings.Contains(ev.Err.Error(), "aggregate") {
			t.Errorf("aggregate: %+v", ev)
		}

		// another schema's tables: its routines come with them
		st, err = w.PickSchema(SchemaPick{Name: "dbc_live_wsl"})
		if err != nil {
			t.Fatal(err)
		}
		sl := st.Job().(*SchemaLoaded)
		if sl.Routines == nil || w.Routines() != nil {
			t.Fatalf("pick: job %v, routines before it ran %+v", sl.Routines != nil, w.Routines())
		}
		sl.Routines()
		if rs := w.Routines(); len(rs) != 1 || rs[0].QName() != "dbc_live_wsl.slug" {
			t.Fatalf("dbc_live_wsl's routines: %+v", rs)
		}

		// a CREATE FUNCTION relists, routines and all
		st, err = w.PickSchema(SchemaPick{Name: "public"})
		if err != nil {
			t.Fatal(err)
		}
		st.Job().(*SchemaLoaded).Routines()
		done := run(t, w, `CREATE FUNCTION dbc_live_new() RETURNS int LANGUAGE sql AS 'SELECT 1'`)
		if done.Relist == nil {
			t.Fatal("CREATE FUNCTION did not relist")
		}
		re := done.Relist().(*Connected)
		if re.Routines == nil {
			t.Fatal("the relist made no routines read")
		}
		re.Routines()
		liveRoutine(t, w.Routines(), "dbc_live_new", "")
	})
}

// THE SIDEBAR'S ROUTINES ON A REAL MYSQL: the database's own, and each
// one's DDL from SHOW CREATE, FUNCTION or PROCEDURE by kind.
func TestLiveWorkspaceSidebarRoutinesMySQL(t *testing.T) {
	e := liveEngines[1]
	w, obs := liveWorkspace(t, e)
	drop := []string{"DROP FUNCTION IF EXISTS dbc_live_tot", "DROP PROCEDURE IF EXISTS dbc_live_arch"}
	obsExec(t, obs, drop...)
	obsExec(t, obs,
		"CREATE FUNCTION dbc_live_tot(o INT) RETURNS INT DETERMINISTIC RETURN o + 1",
		"CREATE PROCEDURE dbc_live_arch(IN since DATE) SELECT since")
	t.Cleanup(func() {
		for _, s := range drop {
			_, _ = obs.Run("live", s)
		}
	})
	reconnectAndLoad(t, w)
	j := w.ShowRoutines(true)
	if j == nil {
		t.Fatal("no Job switching the list on")
	}
	if ev := j().(*RoutinesLoaded); ev.Stale || len(ev.Notes) > 0 {
		t.Fatalf("landed %+v", ev)
	}
	fn := liveRoutine(t, w.Routines(), "dbc_live_tot", "o int")
	proc := liveRoutine(t, w.Routines(), "dbc_live_arch", "IN since date")
	if d := liveDDL(t, w, fn); !strings.Contains(d, "FUNCTION `dbc_live_tot`") || !strings.Contains(d, "RETURN o + 1") ||
		!strings.HasSuffix(d, ";") {
		t.Errorf("function DDL:\n%s", d)
	}
	if d := liveDDL(t, w, proc); !strings.Contains(d, "PROCEDURE `dbc_live_arch`") {
		t.Errorf("procedure DDL:\n%s", d)
	}
}
