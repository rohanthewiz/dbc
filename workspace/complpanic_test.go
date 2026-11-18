package workspace

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/erd"
)

// A panic inside a completion schema read is that read's error, and the
// cache is not left "loading" (N-323): the load returns, a second load
// does not wait on a channel that never closes, and once the read is
// fixed the next load after the retry window reads the schema.
func TestCompletionReadPanics(t *testing.T) {
	w := newTestWorkspace(t)
	prev := readCompletionsFn
	readCompletionsFn = func(context.Context, *db.Manager, bool, string, string, bool) (*erd.Schema, []string, []string, bool, error) {
		panic("boom")
	}
	defer func() { readCompletionsFn = prev }()

	err := w.LoadCompletions(context.Background())
	if err == nil || !strings.Contains(err.Error(), "internal error reading the schema for completion: boom") {
		t.Fatalf("load = %v, want the panic as its error", err)
	}
	// a second ask must not hang on the first's channel
	done := make(chan error, 1)
	go func() { done <- w.LoadCompletions(context.Background()) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a second load waited on a cache left loading")
	}

	// the same through ConnCompletions, dbc web's per-connection cache
	c := NewConnCompletions(w.cfg, w.mgr)
	if _, err := c.Tables(context.Background(), demo); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("conn completions = %v, want the panic as its error", err)
	}
	go func() { _, err := c.Tables(context.Background(), demo); done <- err }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a second conn-completion ask waited for good")
	}
}
