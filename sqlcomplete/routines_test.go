package sqlcomplete

import (
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/model"
)

// shopRoutines is the routines of shop's database: functions in public, two
// overloads of one, a set-returning one, a procedure, a trigger function,
// and a schema (util) that holds routines but no tables.
func shopRoutines() []model.Routine {
	return []model.Routine{
		{Schema: "public", Name: "order_total", Kind: model.RoutineFunction, Args: "o bigint", Result: "numeric"},
		{Schema: "public", Name: "order_total", Kind: model.RoutineFunction, Args: "o bigint, tax boolean", Result: "numeric"},
		{Schema: "public", Name: "recent_orders", Kind: model.RoutineFunction, Args: "days integer", Result: "SETOF orders"},
		{Schema: "public", Name: "archive_orders", Kind: model.RoutineProcedure, Args: "before date"},
		{Schema: "public", Name: "touch_updated", Kind: model.RoutineTrigger, Result: "trigger"},
		{Schema: "public", Name: "next_ref", Kind: model.RoutineFunction, Result: "text"},
		{Schema: "util", Name: "slugify", Kind: model.RoutineFunction, Args: "s text", Result: "text"},
		{Schema: "util", Name: "rebuild", Kind: model.RoutineProcedure},
	}
}

// atR completes buf at the ▮ marker with shop's tables and routines.
func atR(t *testing.T, buf string, path ...string) Result {
	t.Helper()
	i := strings.Index(buf, "▮")
	return Complete(Request{Schema: shop(), Routines: shopRoutines(), Driver: "postgres", SearchPath: path,
		Buffer: buf[:i] + buf[i+len("▮"):], Caret: i})
}

// In an expression the functions are offered as calls, the caret between
// the parentheses — after them for one that takes nothing — and overloads
// are one suggestion; procedures and trigger functions are not offered.
func TestRoutinesInExpression(t *testing.T) {
	r := atR(t, "SELECT ▮ FROM orders")
	it, ok := find(r, "order_total")
	if !ok || it.Kind != KindFunction || it.Insert != "order_total()" || it.Cursor != len("order_total(") {
		t.Fatalf("order_total: %+v", it)
	}
	if !strings.Contains(it.Detail, "+1 overload") || !strings.Contains(it.Doc, "o bigint, tax boolean") {
		t.Errorf("overloads: detail %q, doc %q", it.Detail, it.Doc)
	}
	if it, _ := find(r, "next_ref"); it.Insert != "next_ref()" || it.Cursor != -1 {
		t.Errorf("a function of no arguments: %+v", it)
	}
	for _, l := range []string{"archive_orders", "touch_updated", "rebuild"} {
		if _, ok := find(r, l); ok {
			t.Errorf("%s offered in an expression", l)
		}
	}
	// util is not on the default path (public): its functions go in
	// qualified
	if it, _ := find(r, "slugify"); it.Insert != "util.slugify()" || !strings.HasPrefix(it.Detail, "util · ") {
		t.Errorf("slugify: %+v", it)
	}
	// a "(" already after the caret: the name alone
	r = atR(t, "SELECT order_t▮(id) FROM orders")
	if it, _ := find(r, "order_total"); it.Insert != "order_total" {
		t.Errorf("before a paren: %+v", it)
	}
}

// On a search path naming util, its functions go in bare.
func TestRoutinesOnSearchPath(t *testing.T) {
	r := atR(t, "SELECT slug▮", "util", "public")
	if it, _ := find(r, "slugify"); it.Insert != "slugify()" {
		t.Errorf("slugify on the path: %+v", it)
	}
}

// CALL offers the procedures, and nothing a CALL cannot run.
func TestRoutinesAfterCall(t *testing.T) {
	r := atR(t, "CALL ▮")
	it, ok := find(r, "archive_orders")
	if !ok || it.Kind != KindProcedure || it.Insert != "archive_orders()" || it.Cursor != len("archive_orders(") {
		t.Fatalf("archive_orders: %+v", it)
	}
	if it, _ := find(r, "rebuild"); it.Insert != "util.rebuild()" || it.Cursor != -1 {
		t.Errorf("rebuild: %+v", it)
	}
	if _, ok := find(r, "order_total"); ok {
		t.Error("a function offered after CALL")
	}
	// a column named call is not the keyword
	r = Complete(Request{Schema: shop(), Routines: shopRoutines(), Driver: "postgres", Buffer: "SELECT call ", Caret: 12})
	if _, ok := find(r, "archive_orders"); ok {
		t.Error("SELECT call ▮ offered procedures")
	}
}

// FROM offers the set-returning functions after the tables.
func TestRoutinesInFrom(t *testing.T) {
	r := atR(t, "SELECT * FROM ▮")
	if it, ok := find(r, "recent_orders"); !ok || it.Insert != "recent_orders()" {
		t.Fatalf("recent_orders: %+v", it)
	}
	before(t, r, "orders", "recent_orders")
	if _, ok := find(r, "order_total"); ok {
		t.Error("a scalar function offered after FROM")
	}
}

// DDL naming a routine gets the name alone; EXECUTE FUNCTION a trigger
// function's call, first.
func TestRoutinesInDDL(t *testing.T) {
	for _, c := range []struct {
		buf, want, insert string
		not               string
	}{
		{"DROP FUNCTION ▮", "touch_updated", "touch_updated", "archive_orders"},
		{"DROP FUNCTION IF EXISTS ord▮", "order_total", "order_total", ""},
		{"ALTER PROCEDURE ▮", "archive_orders", "archive_orders", "order_total"},
		{"CREATE OR REPLACE FUNCTION ▮", "order_total", "order_total", "archive_orders"},
		{"DROP ROUTINE ▮", "archive_orders", "archive_orders", ""},
		{"CREATE TRIGGER t BEFORE UPDATE ON orders FOR EACH ROW EXECUTE FUNCTION ▮", "touch_updated", "touch_updated()", "archive_orders"},
		{"CREATE TRIGGER t BEFORE UPDATE ON orders FOR EACH ROW EXECUTE PROCEDURE ▮", "touch_updated", "touch_updated()", "archive_orders"},
	} {
		r := atR(t, c.buf)
		if it, ok := find(r, c.want); !ok || it.Insert != c.insert {
			t.Errorf("%s: %s inserts %q, want %q", c.buf, c.want, it.Insert, c.insert)
		}
		if _, ok := find(r, c.not); c.not != "" && ok {
			t.Errorf("%s: %s offered", c.buf, c.not)
		}
	}
	// a trigger function ranks before the others after EXECUTE FUNCTION
	before(t, atR(t, "CREATE TRIGGER t AFTER INSERT ON orders EXECUTE FUNCTION ▮"), "touch_updated", "next_ref")
}

// schema.▮ offers that schema's routines, bare, by the context before it.
func TestRoutinesQualified(t *testing.T) {
	r := atR(t, "SELECT util.▮")
	if it, ok := find(r, "slugify"); !ok || it.Insert != "slugify()" {
		t.Errorf("util.slugify: %+v", it)
	}
	if _, ok := find(r, "rebuild"); ok {
		t.Error("util.rebuild (a procedure) offered in an expression")
	}
	r = atR(t, "CALL util.▮")
	if it, ok := find(r, "rebuild"); !ok || it.Insert != "rebuild()" {
		t.Errorf("CALL util.rebuild: %+v", it)
	}
	if _, ok := find(r, "slugify"); ok {
		t.Error("CALL util. offered a function")
	}
}

// Without routines, nothing changes: a schema-less statement still gets
// only the vocabulary.
func TestNoRoutines(t *testing.T) {
	r := Complete(Request{Driver: "postgres", Buffer: "CALL ", Caret: 5})
	for _, it := range r.Items {
		if it.Kind == KindFunction || it.Kind == KindProcedure {
			t.Fatalf("offered %+v with no routines", it)
		}
	}
}

// One schema in all, path unknown (MySQL's shape): every routine is bare,
// and the detail does not name the schema.
func TestRoutinesOneSchema(t *testing.T) {
	rs := []model.Routine{{Schema: "shop", Name: "total", Kind: model.RoutineFunction, Args: "o INT", Result: "decimal(9,2)"}}
	r := Complete(Request{Routines: rs, Driver: "mysql", Buffer: "SELECT tot", Caret: 10})
	it, ok := find(r, "total")
	if !ok || it.Insert != "total()" || it.Detail != "total(o INT) → decimal(9,2)" {
		t.Errorf("total: %+v", it)
	}
}
