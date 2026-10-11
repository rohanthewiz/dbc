package workspace

import (
	"context"
	"slices"
	"testing"

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
