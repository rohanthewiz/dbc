package db

import (
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/model"
)

// BuildRoutines maps Postgres's prokind letters, keeps MySQL's words, marks
// trigger functions, and drops what no statement can call.
func TestBuildRoutines(t *testing.T) {
	rows := [][]string{
		{"public", "order_total", "f", "o integer", "numeric"},
		{"public", "archive", "p", "before date", ""},
		{"public", "my_sum", "a", "integer", "integer"},
		{"public", "touch", "f", "", "trigger"},
		{"public", "on_ddl", "f", "", "event_trigger"},
		{"public", "gtrgm_in", "f", "cstring", "gtrgm"},
		{"public", "gtrgm_out", "f", "gtrgm", "cstring"},
		{"public", "gtrgm_consistent", "f", "internal, text, smallint, oid, internal", "boolean"},
		{"public", "plv8_call_handler", "f", "", "language_handler"},
		{"public", "keeps", "f", "internal_id integer", "integer"}, // a parameter named internal…
		{"shop", "total", "function", "o INT", "decimal(9,2)"},     // MySQL
		{"shop", "rebuild", "procedure", "IN since DATE", ""},
		{"short"},
	}
	got := BuildRoutines(rows)
	want := []model.Routine{
		{Schema: "public", Name: "order_total", Kind: model.RoutineFunction, Args: "o integer", Result: "numeric"},
		{Schema: "public", Name: "archive", Kind: model.RoutineProcedure, Args: "before date"},
		{Schema: "public", Name: "my_sum", Kind: model.RoutineAggregate, Args: "integer", Result: "integer"},
		{Schema: "public", Name: "touch", Kind: model.RoutineTrigger, Result: "trigger"},
		{Schema: "public", Name: "on_ddl", Kind: model.RoutineTrigger, Result: "event_trigger"},
		{Schema: "public", Name: "keeps", Kind: model.RoutineFunction, Args: "internal_id integer", Result: "integer"},
		{Schema: "shop", Name: "total", Kind: model.RoutineFunction, Args: "o INT", Result: "decimal(9,2)"},
		{Schema: "shop", Name: "rebuild", Kind: model.RoutineProcedure, Args: "IN since DATE"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d routines, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("routine %d: %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestHasServerArg(t *testing.T) {
	for args, want := range map[string]bool{
		"":                                   false,
		"internal":                           true,
		"a integer, b internal":              true,
		"internal_id integer":                false,
		"x internal DEFAULT NULL::internal":  true,
		"note text DEFAULT 'internal'::text": false,
		"cstring":                            true,
		"cstring_note text":                  false,
	} {
		if got := hasServerArg(args); got != want {
			t.Errorf("hasServerArg(%q) = %v, want %v", args, got, want)
		}
	}
}

// Each engine has a routine listing, or none to read; the Postgres one
// narrows to a scope.
func TestRoutinesQuery(t *testing.T) {
	for _, d := range []string{"postgres", "mysql"} {
		if q, err := RoutinesQuery(d); err != nil || q == "" {
			t.Errorf("%s: %q, %v", d, q, err)
		}
	}
	for _, d := range []string{"sqlite", "bytdb"} {
		if q, err := RoutinesQuery(d); err != nil || q != "" {
			t.Errorf("%s: want no query, got %q, %v", d, q, err)
		}
	}
	q, _ := routinesQuery("postgres", []string{"sales", "public"})
	if !strings.Contains(q, "n.nspname IN ('sales', 'public')") {
		t.Errorf("scoped query: %s", q)
	}
}

// The id column, sixth, is the routine's ID; rows without it still read.
func TestBuildRoutinesID(t *testing.T) {
	got := BuildRoutines([][]string{
		{"public", "total", "f", "o integer", "numeric", "16384"},
		{"shop", "total", "function", "o INT", "int", ""},
		{"public", "old", "f", "", "integer"},
	})
	if len(got) != 3 || got[0].ID != "16384" || got[1].ID != "" || got[2].ID != "" {
		t.Fatalf("ids: %+v", got)
	}
	if got[0].QName() != "public.total" || (model.Routine{Name: "bare"}).QName() != "bare" {
		t.Errorf("qname: %q", got[0].QName())
	}
}

// RoutineDDLQuery names a Postgres routine by its oid — refusing an
// aggregate, which pg_get_functiondef cannot render, and an id that is not
// an oid, which would otherwise go into the statement — and a MySQL one by
// its quoted schema and name, FUNCTION or PROCEDURE by kind.
func TestRoutineDDLQuery(t *testing.T) {
	for _, c := range []struct {
		driver string
		r      model.Routine
		want   string // "" when refused
		col    int
	}{
		{"postgres", model.Routine{Schema: "public", Name: "total", Kind: model.RoutineFunction, ID: "16384"},
			"SELECT pg_catalog.pg_get_functiondef(16384::oid)", 0},
		{"pg", model.Routine{Name: "archive", Kind: model.RoutineProcedure, ID: "7"},
			"SELECT pg_catalog.pg_get_functiondef(7::oid)", 0},
		{"pg", model.Routine{Name: "touch", Kind: model.RoutineTrigger, ID: "9"},
			"SELECT pg_catalog.pg_get_functiondef(9::oid)", 0},
		{"pg", model.Routine{Name: "my_sum", Kind: model.RoutineAggregate, ID: "8"}, "", 0},
		{"pg", model.Routine{Name: "total", Kind: model.RoutineFunction, ID: "1); DROP TABLE x; --"}, "", 0},
		{"pg", model.Routine{Name: "total", Kind: model.RoutineFunction}, "", 0},
		{"mysql", model.Routine{Schema: "shop", Name: "total", Kind: model.RoutineFunction},
			"SHOW CREATE FUNCTION `shop`.`total`", 2},
		{"mysql", model.Routine{Schema: "shop", Name: "re`build", Kind: model.RoutineProcedure},
			"SHOW CREATE PROCEDURE `shop`.`re``build`", 2},
		{"mysql", model.Routine{Name: "total", Kind: model.RoutineFunction},
			"SHOW CREATE FUNCTION `total`", 2},
		{"sqlite", model.Routine{Name: "x", Kind: model.RoutineFunction}, "", 0},
	} {
		got, err := RoutineDDLQuery(c.driver, c.r)
		if c.want == "" {
			if err == nil {
				t.Errorf("%s %+v: %q, want a refusal", c.driver, c.r, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s %+v: %q, %v; want %q", c.driver, c.r, got, err, c.want)
		}
		if col := RoutineDDLColumn(c.driver); col != c.col {
			t.Errorf("%s: column %d, want %d", c.driver, col, c.col)
		}
	}
}

func TestHasRoutines(t *testing.T) {
	for d, want := range map[string]bool{"postgres": true, "pg": true, "mysql": true, "mariadb": true,
		"sqlite": false, "bytdb": false, "nope": false} {
		if HasRoutines(d) != want {
			t.Errorf("HasRoutines(%q) = %v", d, !want)
		}
	}
}
