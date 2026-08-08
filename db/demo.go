package db

import (
	"fmt"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/serr"
)

// demoStmts is the seed script for the built-in demo connections. It is one
// script for every engine, so the two demos really are comparable: the same
// rows, spelled the same way.
//
// The boolean column is written with the true/false keywords rather than 1/0
// because bytdb types its columns the way Postgres does and rejects an integer
// in a boolean column. SQLite accepts the keywords (3.23+) and stores them as
// 1/0, which is what it always showed — so the SQLite demo is unchanged by
// this, and the same script now also loads on bytdb.
var demoStmts = []string{
	`CREATE TABLE IF NOT EXISTS cats (
		id INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		breed TEXT NOT NULL,
		age INTEGER NOT NULL,
		adopted BOOLEAN NOT NULL DEFAULT false
	)`,
	`DELETE FROM cats`,
	`INSERT INTO cats (id, name, breed, age, adopted) VALUES
		(1, 'Whiskers', 'Tabby',      3, true),
		(2, 'Luna',     'Siamese',    2, false),
		(3, 'Bella',    'Maine Coon', 5, true),
		(4, 'Oliver',   'Tabby',      1, false),
		(5, 'Leo',      'Bengal',     4, true),
		(6, 'Milo',     'Siamese',    7, false),
		(7, 'Cleo',     'Sphynx',     2, true),
		(8, 'Simba',    'Maine Coon', 6, false)`,
}

// SeedDemo populates one built-in demo connection with a small cats table so
// the app has something to query out of the box.
//
// The DELETE is what makes this safe to re-run: the SQLite demo lives in
// memory and starts empty every time, but the bytdb demo is a file that
// survives restarts, so without it a second launch would collide with the rows
// the first one inserted.
func SeedDemo(m *Manager, name string) error {
	dbh, err := m.DB(name)
	if err != nil {
		return serr.Wrap(err, "phase", "open demo")
	}
	for _, s := range demoStmts {
		if _, err = dbh.Exec(s); err != nil {
			return serr.Wrap(err, "phase", "seed demo", "conn", name)
		}
	}
	return nil
}

// SeedDemos seeds every connection in a demo config, and reports which ones
// could not be seeded rather than failing the launch over them.
//
// One demo failing should not take the other down with it. The bytdb demo is a
// file, so it has failure modes the in-memory SQLite one does not: a second
// dbc already holds the engine open, the cache directory is read-only, the
// file is left over from an incompatible version. In any of those cases the
// connection is dropped from the config — leaving it listed would only offer
// the user a connection that errors on every query — a warning is recorded,
// and if it was the active default the surviving demo takes over.
//
// Returns an error only when nothing at all could be seeded, since at that
// point there is no usable connection to start on.
func SeedDemos(m *Manager, cfg *config.Config) error {
	kept := make([]config.Connection, 0, len(cfg.Connections))
	for _, cn := range cfg.Connections {
		if err := SeedDemo(m, cn.Name); err != nil {
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf(
				"built-in %q demo unavailable, skipping it: %v", cn.Name, err))
			m.Drop(cn.Name) // do not leave a half-open handle in the pool
			continue
		}
		kept = append(kept, cn)
	}
	if len(kept) == 0 {
		return serr.New("no built-in demo connection could be opened",
			"warnings", fmt.Sprint(cfg.Warnings))
	}
	cfg.Connections = kept
	if _, ok := cfg.ConnByName(cfg.DefaultConnection); !ok {
		cfg.DefaultConnection = kept[0].Name
	}
	return nil
}
