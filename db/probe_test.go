package db

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rohanthewiz/bytdb"

	"github.com/rohanthewiz/dbc/config"
)

func TestProbe(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// an existing SQLite file opens and pings
	file := filepath.Join(dir, "here.db")
	if err := os.WriteFile(file, nil, 0o600); err != nil { // an empty file is a valid empty database
		t.Fatal(err)
	}
	res, err := Probe(ctx, config.Connection{Name: "f", Driver: "sqlite", DSN: "file:" + file + "?_pragma=foreign_keys(1)"}, time.Second)
	if err != nil || !res.OK || res.Note != "" {
		t.Fatalf("existing sqlite: %+v %v", res, err)
	}

	// missing embedded files are reported without creating them
	for _, cc := range []config.Connection{
		{Name: "s", Driver: "sqlite", DSN: filepath.Join(dir, "nope.db")},
		{Name: "b", Driver: "bytdb", DSN: filepath.Join(dir, "nope.bytdb")},
	} {
		res, err = Probe(ctx, cc, time.Second)
		if err != nil || !res.OK || res.Note == "" {
			t.Fatalf("%s missing: %+v %v", cc.Driver, res, err)
		}
		if _, statErr := os.Stat(cc.DSN); !os.IsNotExist(statErr) {
			t.Fatalf("%s: the probe created %s", cc.Driver, cc.DSN)
		}
	}

	// a bytdb file another engine holds is "in use", as a real connect says
	held := filepath.Join(dir, "held.bytdb")
	h, err := bytdb.Open(held)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if _, err = Probe(ctx, config.Connection{Name: "h", Driver: "bytdb", DSN: held}, time.Second); !errors.Is(err, ErrInUse) {
		t.Fatalf("held bytdb: %v", err)
	}

	if _, err = Probe(ctx, config.Connection{Driver: "oracle", DSN: "x"}, time.Second); err == nil {
		t.Fatal("unknown driver probed fine")
	}
}

func TestEmbeddedPath(t *testing.T) {
	for _, c := range []struct{ drv, dsn, want string }{
		{"sqlite", "file:a.db?mode=ro", "a.db"},
		{"sqlite", "/tmp/a.db", "/tmp/a.db"},
		{"sqlite", "file:x?mode=memory&cache=shared", ""},
		{"sqlite", ":memory:", ""},
		{"bytdb", "n.bytdb", "n.bytdb"},
		{"pgx", "postgres://h/db", ""},
	} {
		if got := embeddedPath(c.drv, c.dsn); got != c.want {
			t.Errorf("embeddedPath(%q, %q) = %q, want %q", c.drv, c.dsn, got, c.want)
		}
	}
}
