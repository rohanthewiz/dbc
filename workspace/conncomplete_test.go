package workspace

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/rohanthewiz/dbc/sqlcomplete"
)

// ConnCompletions completes against a connection by name, with no
// workspace connected to it: the schema read once and kept (a table made
// meanwhile is not offered until the entry is stale or dropped), the
// tables listed for a table field, an unknown connection refused.
func TestConnCompletions(t *testing.T) {
	w := newTestWorkspace(t)
	c := NewConnCompletions(w.cfg, w.mgr)
	ctx := context.Background()

	buf := "SELECT * FROM c"
	res, err := c.Complete(ctx, demo, buf, len(buf))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(res.Items, func(it sqlcomplete.Item) bool { return it.Label == "cats" }) {
		t.Errorf("cats not offered: %+v", res.Items)
	}
	buf = "SELECT b FROM cats"
	if res, _ = c.Complete(ctx, demo, buf, len("SELECT b")); !slices.ContainsFunc(res.Items, func(it sqlcomplete.Item) bool { return it.Label == "breed" }) {
		t.Errorf("cats' breed not offered: %+v", res.Items)
	}
	tables, err := c.Tables(ctx, demo)
	if err != nil || !slices.Contains(tables, "cats") {
		t.Fatalf("tables = %v, %v", tables, err)
	}

	// read once: a table made since is not listed until the entry goes
	if _, err := w.mgr.Run(demo, "CREATE TABLE conn_compl_new (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	if tables, _ = c.Tables(ctx, demo); slices.Contains(tables, "conn_compl_new") {
		t.Error("read again within the TTL")
	}
	c.Drop(demo)
	if tables, _ = c.Tables(ctx, demo); !slices.Contains(tables, "conn_compl_new") {
		t.Errorf("after Drop: %v", tables)
	}

	if _, err := c.Tables(ctx, "nope"); err == nil || err.Error() != "no connection named nope" {
		t.Errorf("an unknown connection: %v", err)
	}
	if c.schemaOf("nope") != nil {
		t.Error("an unknown connection was cached")
	}
}

// Ask is Complete without a read: before Load, the vocabulary marked
// Incomplete; after it, the schema. An unknown connection is ready at once
// with its error, and an off one with the words alone, neither read.
// Load waits for a read already in flight instead of starting another.
func TestConnCompletionsAsk(t *testing.T) {
	w := newTestWorkspace(t)
	c := NewConnCompletions(w.cfg, w.mgr)
	has := func(r sqlcomplete.Result, l string) bool {
		return slices.ContainsFunc(r.Items, func(it sqlcomplete.Item) bool { return it.Label == l })
	}

	res, ready, err := c.Ask(demo, "SELECT * FROM c", 15)
	if ready || err != nil || !res.Incomplete || has(res, "cats") {
		t.Fatalf("before Load: ready=%v err=%v incomplete=%v cats=%v", ready, err, res.Incomplete, has(res, "cats"))
	}
	if c.schemaOf(demo) != nil {
		t.Fatal("Ask read the schema")
	}
	if err = c.Load(context.Background(), demo); err != nil {
		t.Fatal(err)
	}
	if res, ready, err = c.Ask(demo, "SELECT * FROM c", 15); !ready || err != nil || res.Incomplete || !has(res, "cats") {
		t.Fatalf("after Load: ready=%v err=%v incomplete=%v cats=%v", ready, err, res.Incomplete, has(res, "cats"))
	}

	if res, ready, err = c.Ask("nope", "SELECT coa", 10); !ready || err == nil || res.Incomplete || !has(res, "coalesce") {
		t.Fatalf("unknown connection: ready=%v err=%v incomplete=%v", ready, err, res.Incomplete)
	}

	// a read in flight (an entry still loading, set directly): Ask is not
	// ready, and Load waits on that read rather than starting its own; here
	// the read never lands, so Load gives up when its context does
	c.mu.Lock()
	c.entries[demo] = &complState{conn: demo, loading: make(chan struct{})}
	c.mu.Unlock()
	if _, ready, _ = c.Ask(demo, "SELECT ", 7); ready {
		t.Fatal("Ask was ready while a read was in flight")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err = c.Load(ctx, demo); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Load during a held read: %v, want it to wait (deadline exceeded)", err)
	}
}
