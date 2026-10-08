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
