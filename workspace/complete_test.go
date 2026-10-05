package workspace

import (
	"context"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/sqlcomplete"
)

func hasLabel(r sqlcomplete.Result, label string) bool {
	for _, it := range r.Items {
		if it.Label == label {
			return true
		}
	}
	return false
}

// Completion answers from a cached schema: not ready until the schema is
// loaded, ready after, and not ready again once a DDL run drops the cache —
// after which the reload sees the new table.
func TestCompletionCache(t *testing.T) {
	w := newTestWorkspace(t)
	buf := "SELECT * FROM "

	if _, ready := w.Complete(buf, len(buf)); ready {
		t.Fatal("ready before any load")
	}
	if err := w.LoadCompletions(context.Background()); err != nil {
		t.Fatal(err)
	}
	r, ready := w.Complete(buf, len(buf))
	if !ready || !hasLabel(r, "cats") {
		t.Fatalf("after load: ready=%v items=%v", ready, r.Items)
	}

	// a plain query keeps the cache; DDL drops it
	run(t, w, "SELECT 1")
	if _, ready = w.Complete(buf, len(buf)); !ready {
		t.Fatal("a SELECT dropped the cache")
	}
	run(t, w, "CREATE TABLE dogs (id INTEGER PRIMARY KEY, name TEXT)")
	if _, ready = w.Complete(buf, len(buf)); ready {
		t.Fatal("CREATE TABLE kept the cache")
	}
	if err := w.LoadCompletions(context.Background()); err != nil {
		t.Fatal(err)
	}
	r, _ = w.Complete("SELECT d. FROM dogs d", len("SELECT d."))
	if !hasLabel(r, "name") {
		t.Errorf("dogs' columns after reload: %v", r.Items)
	}
}

// With no connection, completion is ready at once with the vocabulary.
func TestCompletionWithoutConnection(t *testing.T) {
	w := newTestWorkspace(t)
	if _, st, err := w.Disconnect(); err != nil {
		t.Fatal(err)
	} else if st.Job != nil {
		st.Job()
	}
	r, ready := w.Complete("SELECT coa", 10)
	if !ready || !hasLabel(r, "coalesce") {
		t.Errorf("ready=%v items=%v", ready, r.Items)
	}
}

// A schema pick installs a new sidebar catalog (setCatalogLocked) but keeps
// the completion cache, which already holds every schema; a connect, even
// back to the same connection, drops it. The pick's landing is driven
// directly here: PickSchema itself needs Postgres (live_test.go covers it).
func TestCompletionCacheAcrossPickAndConnect(t *testing.T) {
	w := newTestWorkspace(t)
	if err := w.LoadCompletions(context.Background()); err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	w.schema = "main"
	w.setCatalogLocked(w.catalog) // what a pick's landing does
	w.mu.Unlock()
	if _, ready := w.Complete("SELECT ", 7); !ready {
		t.Fatal("a schema pick dropped the completion cache")
	}

	if ev := w.Connect(w.Active()).Job().(*Connected); ev.Err != nil {
		t.Fatal(ev.Err)
	}
	if _, ready := w.Complete("SELECT ", 7); ready {
		t.Error("a reconnect kept the completion cache")
	}
}

// A scoped cache (a catalog too big to read whole) answers only while the
// sidebar shows one of the schemas it read; a pick of another makes it not
// ready, so the UI loads again for that schema. A whole cache covers every
// pick. The cache is set directly: a scoped load needs Postgres
// (live_test.go covers it).
func TestCompletionScopedCache(t *testing.T) {
	w := newTestWorkspace(t)
	if err := w.LoadCompletions(context.Background()); err != nil {
		t.Fatal(err)
	}
	set := func(scope []string, focus string) {
		w.mu.Lock()
		w.compl.scope, w.schema = scope, focus
		w.mu.Unlock()
	}
	for _, c := range []struct {
		scope []string
		focus string
		ready bool
	}{
		{nil, "billing", true}, // whole: every schema
		{[]string{"sales", "public"}, "sales", true},
		{[]string{"sales", "public"}, "public", true},
		{[]string{"sales", "public"}, "", true}, // no schema shown
		{[]string{"sales", "public"}, "billing", false},
	} {
		set(c.scope, c.focus)
		if _, ready := w.Complete("SELECT ", 7); ready != c.ready {
			t.Errorf("scope %v, sidebar on %q: ready = %v", c.scope, c.focus, ready)
		}
	}
}

func TestComplScope(t *testing.T) {
	for _, c := range []struct {
		focus string
		path  []string
		want  string
	}{
		{"sales", []string{"app", "public"}, "sales app public"},
		{"app", []string{"app", "public"}, "app public"}, // no duplicate
		{"sales", nil, "sales public"},                   // the default path
		{"", nil, "public"},
		{"sales", []string{}, "sales"}, // a path the server reported empty
	} {
		if got := strings.Join(complScope(c.focus, c.path), " "); got != c.want {
			t.Errorf("complScope(%q, %v) = %q, want %q", c.focus, c.path, got, c.want)
		}
	}
}
