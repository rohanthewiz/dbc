package db

import (
	"context"
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

// SeedDemo populates one connection with the demo cats table so the app has
// something to query out of the box. It seeds unconditionally, whatever the
// connection is — the tests seed their own private databases with it — so it
// is never pointed at a user's connection. The built-in demos do not need it:
// the Manager seeds those itself on first open (see Manager.open), and
// SeedDemo on one of them only runs the script a second time, harmlessly.
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
	return seedDB(context.Background(), dbh, name)
}

// seedDB runs the demo seed script on an open database.
func seedDB(ctx context.Context, ex execQuerier, name string) error {
	for _, s := range demoStmts {
		if _, err := ex.ExecContext(ctx, s); err != nil {
			return serr.Wrap(err, "phase", "seed demo", "conn", name)
		}
	}
	return nil
}

// SeedDemos opens every built-in demo connection up front — which seeds it
// (see Manager.open) — and reports which ones could not be opened rather than
// failing the launch over them. The TUI calls it: it lists every connection,
// so an unusable one has to be found before the list is shown. A headless run
// wants only one connection and calls OpenDefaultDemo instead, or nothing.
//
// One demo failing should not take the other down with it. The bytdb demo is a
// file, so it has failure modes the in-memory SQLite one does not: a second
// dbc already holds the engine open, the cache directory is read-only, the
// file is left over from an incompatible version. In any of those cases the
// connection is dropped from the config — leaving it listed would only offer
// the user a connection that errors on every query — a warning is recorded,
// and if it was the active default the surviving demo takes over.
//
// Only connections marked Demo are touched. A demo config can also hold the
// ad-hoc --dsn connection, and that one is the user's database: it is neither
// opened here nor ever seeded.
//
// Returns an error only when no connection at all is left to start on.
func SeedDemos(m *Manager, cfg *config.Config) error {
	return openDemos(m, cfg, false)
}

// OpenDefaultDemo is SeedDemos for a headless run on the active demo: it opens
// the demos in config order — the active one first (see config.demoFallback)
// — and stops at the first that opens, so the other demo is not touched unless
// the active one is unusable. That keeps SeedDemos' fallback, where a demo
// that cannot be opened (the bytdb file held by a running TUI, say) gives way
// to the other one with a warning, without paying for it on every run.
func OpenDefaultDemo(m *Manager, cfg *config.Config) error {
	return openDemos(m, cfg, true)
}

// openDemos is SeedDemos and OpenDefaultDemo. firstOnly stops at the first
// demo that opens; the demos after it stay listed, unopened, and seed lazily
// if a run asks for them by name.
//
//	for each connection, in config order:
//	  not a demo, or firstOnly and one already open ─► keep, untouched
//	  opens (and seeds) ─────────────────────────────► keep
//	  fails ─────────────────────────────────────────► warn, drop from config
//	no demo opened and nothing else kept ───────────► error
//	default dropped ─► first opened demo, else first kept connection
func openDemos(m *Manager, cfg *config.Config, firstOnly bool) error {
	kept := make([]config.Connection, 0, len(cfg.Connections))
	firstOpened := ""
	for _, cn := range cfg.Connections {
		if !cn.Demo || (firstOnly && firstOpened != "") {
			kept = append(kept, cn)
			continue
		}
		if _, err := m.DB(cn.Name); err != nil {
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf(
				"built-in %q demo unavailable, skipping it: %v", cn.Name, err))
			// Belt and braces: a failed open caches nothing today, but a
			// handle left in the pool would be handed to the next caller.
			m.Drop(cn.Name)
			continue
		}
		if firstOpened == "" {
			firstOpened = cn.Name
		}
		kept = append(kept, cn)
	}
	if firstOpened == "" && len(kept) == 0 {
		return serr.New("no built-in demo connection could be opened",
			"warnings", fmt.Sprint(cfg.Warnings))
	}
	cfg.Connections = kept
	if _, ok := cfg.ConnByName(cfg.DefaultConnection); !ok {
		cfg.DefaultConnection = firstOpened
		if firstOpened == "" {
			cfg.DefaultConnection = kept[0].Name
		}
	}
	return nil
}
