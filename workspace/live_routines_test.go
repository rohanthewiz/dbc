package workspace

import (
	"context"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/sqlcomplete"
)

// routineAt is Workspace.Complete at the ▮ in buf, as label → item for the
// routine suggestions only, failing when the cache is cold.
func routineAt(t *testing.T, w *Workspace, buf string) map[string]sqlcomplete.Item {
	t.Helper()
	i := strings.Index(buf, "▮")
	res, ready := w.Complete(buf[:i]+buf[i+len("▮"):], i)
	if !ready {
		t.Fatal("completion's cache is cold")
	}
	out := map[string]sqlcomplete.Item{}
	for _, it := range res.Items {
		if it.Kind == sqlcomplete.KindFunction || it.Kind == sqlcomplete.KindProcedure {
			out[it.Label] = it
		}
	}
	return out
}

// reconnectAndLoad connects w again (so the next load is the first on a
// connection that has what the test just made) and loads completion.
func reconnectAndLoad(t *testing.T, w *Workspace) {
	t.Helper()
	_, st, _ := w.Disconnect()
	st.Job()
	if ev := w.Connect("live").Job().(*Connected); ev.Err != nil {
		t.Fatalf("reconnect: %+v", ev)
	}
	if err := w.LoadCompletions(context.Background()); err != nil {
		t.Fatalf("load completions: %v", err)
	}
}

// ROUTINES ON A REAL POSTGRES: the load reads pg_proc beside the tables. A
// function in public is offered bare in an expression with its signature,
// overloads as one; one in another schema qualified, and bare after that
// schema's name; a procedure after CALL only; a set-returning function
// after FROM; a trigger function after EXECUTE FUNCTION only. A CREATE
// FUNCTION run drops the cache, and the reload has the new function.
func TestLiveWorkspaceCompletionRoutines(t *testing.T) {
	pgOnly(t, func(t *testing.T, e liveEngine) {
		w, obs := liveWorkspace(t, e)
		drop := []string{
			`DROP SCHEMA IF EXISTS dbc_live_wsr CASCADE`,
			`DROP FUNCTION IF EXISTS dbc_live_total(integer), dbc_live_total(integer, boolean),
			   dbc_live_rows(integer), dbc_live_touch(), dbc_live_later()`,
			`DROP PROCEDURE IF EXISTS dbc_live_archive(date)`,
		}
		obsExec(t, obs, drop...)
		obsExec(t, obs,
			`CREATE SCHEMA dbc_live_wsr`,
			`CREATE FUNCTION dbc_live_total(o integer) RETURNS numeric LANGUAGE sql AS 'SELECT 1::numeric'`,
			`CREATE FUNCTION dbc_live_total(o integer, tax boolean) RETURNS numeric LANGUAGE sql AS 'SELECT 2::numeric'`,
			`CREATE FUNCTION dbc_live_rows(n integer) RETURNS SETOF integer LANGUAGE sql AS 'SELECT generate_series(1, n)'`,
			`CREATE FUNCTION dbc_live_touch() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END'`,
			`CREATE PROCEDURE dbc_live_archive(before date) LANGUAGE sql AS 'SELECT 1'`,
			`CREATE FUNCTION dbc_live_wsr.slug(s text) RETURNS text LANGUAGE sql AS 'SELECT lower(s)'`)
		t.Cleanup(func() {
			for _, s := range drop {
				_, _ = obs.Run("live", s)
			}
		})
		reconnectAndLoad(t, w)

		got := routineAt(t, w, "SELECT dbc_live▮ FROM "+liveTable)
		if it, ok := got["dbc_live_total"]; !ok || it.Insert != "dbc_live_total()" ||
			!strings.HasPrefix(it.Detail, "public · dbc_live_total(o integer) → numeric") || !strings.Contains(it.Detail, "+1 overload") {
			t.Errorf("dbc_live_total: %+v", it)
		}
		for _, l := range []string{"dbc_live_archive", "dbc_live_touch"} {
			if _, ok := got[l]; ok {
				t.Errorf("%s offered in an expression", l)
			}
		}
		if it, ok := routineAt(t, w, "SELECT slu▮")["slug"]; !ok || it.Insert != "dbc_live_wsr.slug()" {
			t.Errorf("slug, off the path: %+v", it)
		}
		if it, ok := routineAt(t, w, "SELECT dbc_live_wsr.▮")["slug"]; !ok || it.Insert != "slug()" {
			t.Errorf("dbc_live_wsr.slug: %+v", it)
		}

		got = routineAt(t, w, "CALL dbc_live▮")
		if it, ok := got["dbc_live_archive"]; !ok || it.Kind != sqlcomplete.KindProcedure || it.Insert != "dbc_live_archive()" {
			t.Errorf("CALL: dbc_live_archive %+v", it)
		}
		if _, ok := got["dbc_live_total"]; ok {
			t.Error("CALL offered a function")
		}
		if it, ok := routineAt(t, w, "SELECT * FROM dbc_live▮")["dbc_live_rows"]; !ok || it.Insert != "dbc_live_rows()" {
			t.Errorf("FROM: dbc_live_rows %+v", it)
		}
		if _, ok := routineAt(t, w, "CREATE TRIGGER x BEFORE UPDATE ON t FOR EACH ROW EXECUTE FUNCTION dbc▮")["dbc_live_touch"]; !ok {
			t.Error("EXECUTE FUNCTION: dbc_live_touch not offered")
		}

		// a CREATE FUNCTION through the workspace drops the cache; the
		// reload knows the new function
		run(t, w, `CREATE FUNCTION dbc_live_later() RETURNS int LANGUAGE sql AS 'SELECT 1'`)
		if _, ready := w.Complete("SELECT ", 7); ready {
			t.Fatal("CREATE FUNCTION kept the cache")
		}
		if err := w.LoadCompletions(context.Background()); err != nil {
			t.Fatal(err)
		}
		if it, ok := routineAt(t, w, "SELECT dbc_live_la▮")["dbc_live_later"]; !ok || it.Cursor != -1 {
			t.Errorf("after reload: dbc_live_later %+v (no arguments: the caret after the parens)", it)
		}
	})
}

// ROUTINES ON A REAL MYSQL: information_schema.routines, the database's
// own only; a function's parameters without a mode, a procedure's with
// one; every name bare.
func TestLiveWorkspaceCompletionRoutinesMySQL(t *testing.T) {
	e := liveEngines[1]
	w, obs := liveWorkspace(t, e)
	drop := []string{"DROP FUNCTION IF EXISTS dbc_live_total", "DROP PROCEDURE IF EXISTS dbc_live_archive"}
	obsExec(t, obs, drop...)
	obsExec(t, obs,
		"CREATE FUNCTION dbc_live_total(o INT, tax BOOLEAN) RETURNS DECIMAL(9,2) DETERMINISTIC RETURN o",
		"CREATE PROCEDURE dbc_live_archive(IN since DATE, OUT n INT) SELECT 1 INTO n")
	t.Cleanup(func() {
		for _, s := range drop {
			_, _ = obs.Run("live", s)
		}
	})
	reconnectAndLoad(t, w)

	if it, ok := routineAt(t, w, "SELECT dbc_live▮")["dbc_live_total"]; !ok || it.Insert != "dbc_live_total()" ||
		it.Detail != "dbc_live_total(o int, tax tinyint(1)) → decimal(9,2)" {
		t.Errorf("dbc_live_total: %+v", it)
	}
	if it, ok := routineAt(t, w, "CALL dbc▮")["dbc_live_archive"]; !ok || it.Insert != "dbc_live_archive()" ||
		it.Detail != "dbc_live_archive(IN since date, OUT n int)" {
		t.Errorf("dbc_live_archive: %+v", it)
	}
}
