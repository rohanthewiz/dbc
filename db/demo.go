package db

import "github.com/rohanthewiz/serr"

// SeedDemo populates the built-in in-memory demo connection with a small
// cats table so the app has something to query out of the box.
func SeedDemo(m *Manager, name string) error {
	dbh, err := m.DB(name)
	if err != nil {
		return serr.Wrap(err, "phase", "open demo")
	}
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS cats (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			breed TEXT NOT NULL,
			age INTEGER NOT NULL,
			adopted BOOLEAN NOT NULL DEFAULT 0
		)`,
		`DELETE FROM cats`,
		`INSERT INTO cats (id, name, breed, age, adopted) VALUES
			(1, 'Whiskers', 'Tabby',      3, 1),
			(2, 'Luna',     'Siamese',    2, 0),
			(3, 'Bella',    'Maine Coon', 5, 1),
			(4, 'Oliver',   'Tabby',      1, 0),
			(5, 'Leo',      'Bengal',     4, 1),
			(6, 'Milo',     'Siamese',    7, 0),
			(7, 'Cleo',     'Sphynx',     2, 1),
			(8, 'Simba',    'Maine Coon', 6, 0)`,
	}
	for _, s := range stmts {
		if _, err = dbh.Exec(s); err != nil {
			return serr.Wrap(err, "phase", "seed demo")
		}
	}
	return nil
}
