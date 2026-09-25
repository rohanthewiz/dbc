package db

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/config"
)

// demoCfg mirrors the no-config fallback, but with the bytdb demo pointed at a
// temp file so the test never touches the real cache directory.
func demoCfg(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		MaxRows: 1000,
		Demo:    true,
		Connections: []config.Connection{
			{Name: config.DemoBytdb, Driver: "bytdb", DSN: filepath.Join(t.TempDir(), "demo.bytdb"), Demo: true},
			{Name: config.DemoSQLite, Driver: "sqlite", DSN: "file:" + memName(t) + "?mode=memory&cache=shared", Demo: true},
		},
		DefaultConnection: config.DemoBytdb,
	}
}

// The whole point of shipping both demos is that the same query works on
// either one, so the seed script has to load on both engines and leave the
// same rows behind. bytdb types booleans the way Postgres does, which is what
// made the old 1/0 literals a problem.
func TestSeedDemosLoadsBothEngines(t *testing.T) {
	cfg := demoCfg(t)
	mgr := NewManager(cfg)
	t.Cleanup(mgr.Close)

	if err := SeedDemos(mgr, cfg); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if len(cfg.Connections) != 2 {
		t.Fatalf("connections = %+v, want both demos kept", cfg.Connections)
	}
	if len(cfg.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", cfg.Warnings)
	}

	for _, name := range []string{config.DemoBytdb, config.DemoSQLite} {
		res, err := mgr.Run(name, "SELECT name, breed FROM cats ORDER BY id")
		if err != nil {
			t.Fatalf("%s: select: %v", name, err)
		}
		if len(res.Rows) != 8 {
			t.Errorf("%s: %d rows, want 8", name, len(res.Rows))
		}
		if res.Rows[0][0] != "Whiskers" || res.Rows[0][1] != "Tabby" {
			t.Errorf("%s: first row = %v", name, res.Rows[0])
		}
	}
}

// The bytdb demo is a file that outlives the process, so seeding it twice —
// which is what a second launch does — must not pile up duplicate rows.
func TestSeedDemoIsRepeatable(t *testing.T) {
	cfg := demoCfg(t)
	mgr := NewManager(cfg)
	t.Cleanup(mgr.Close)

	for i := range 3 {
		if err := SeedDemo(mgr, config.DemoBytdb); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	res, err := mgr.Run(config.DemoBytdb, "SELECT id FROM cats")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(res.Rows) != 8 {
		t.Errorf("%d rows after three seeds, want 8", len(res.Rows))
	}
}

// One demo failing must not take the launch down with it: the bad connection
// is dropped, a warning explains why, and the active default moves to the demo
// that did come up.
func TestSeedDemosPrunesUnusableDemo(t *testing.T) {
	cfg := demoCfg(t)
	// a directory where the engine expects to create a file — an open failure
	// the driver cannot work around, standing in for a locked or unwritable
	// demo file
	cfg.Connections[0].DSN = t.TempDir()
	mgr := NewManager(cfg)
	t.Cleanup(mgr.Close)

	if err := SeedDemos(mgr, cfg); err != nil {
		t.Fatalf("seed should survive one bad demo: %v", err)
	}
	if len(cfg.Connections) != 1 || cfg.Connections[0].Name != config.DemoSQLite {
		t.Fatalf("connections = %+v, want only the sqlite demo", cfg.Connections)
	}
	if cfg.DefaultConnection != config.DemoSQLite {
		t.Errorf("default = %q, want it moved to the surviving demo", cfg.DefaultConnection)
	}
	if len(cfg.Warnings) != 1 || !strings.Contains(cfg.Warnings[0], config.DemoBytdb) {
		t.Errorf("warnings = %v, want one naming the dropped demo", cfg.Warnings)
	}
	// and the surviving demo is actually usable
	if _, err := mgr.Run(config.DemoSQLite, "SELECT id FROM cats"); err != nil {
		t.Errorf("sqlite demo: %v", err)
	}
}

// With nothing seedable there is no connection to start on, so the launch
// should fail loudly rather than open a TUI with an empty connection list.
func TestSeedDemosFailsWhenNothingSeeds(t *testing.T) {
	cfg := demoCfg(t)
	cfg.Connections[0].DSN = t.TempDir()
	cfg.Connections[1].DSN = "file:" + filepath.Join(t.TempDir(), "nope") + "?mode=ro"
	mgr := NewManager(cfg)
	t.Cleanup(mgr.Close)

	if err := SeedDemos(mgr, cfg); err == nil {
		t.Fatal("no demo could be seeded, so SeedDemos should fail")
	}
}

// memName is a shared in-memory SQLite name private to one test. Such a
// database lives as long as any connection to it, so a fixed name would let
// one test's rows leak into the next while an earlier Manager is still open.
func memName(t *testing.T) string {
	return "seedtest_" + strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
}

// opened reports whether the manager has a live pool for name — whether
// anything so far has opened that connection.
func opened(m *Manager, name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.conns[name]
	return ok
}

// A demo seeds itself on first use, with nothing called up front, and a demo
// nobody uses is never opened: the bytdb file is not even created.
func TestDemoSeedsOnFirstUse(t *testing.T) {
	cfg := demoCfg(t)
	mgr := NewManager(cfg)
	t.Cleanup(mgr.Close)

	res, err := mgr.Run(config.DemoSQLite, "SELECT name FROM cats ORDER BY id")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(res.Rows) != 8 || res.Rows[0][0] != "Whiskers" {
		t.Errorf("rows = %v, want the 8 seeded cats", res.Rows)
	}
	if opened(mgr, config.DemoBytdb) {
		t.Error("the bytdb demo was opened, but nothing used it")
	}
	if _, err = os.Stat(cfg.Connections[0].DSN); !os.IsNotExist(err) {
		t.Errorf("bytdb demo file exists (stat err %v), want it untouched", err)
	}
}

// Seeding happens once per open, not once per use: rows written after the
// first use survive the next statement. Reseeding there would wipe the
// user's demo edits mid-session.
func TestDemoSeedsOncePerOpen(t *testing.T) {
	cfg := demoCfg(t)
	mgr := NewManager(cfg)
	t.Cleanup(mgr.Close)

	if _, err := mgr.Run(config.DemoBytdb,
		"INSERT INTO cats (id, name, breed, age, adopted) VALUES (9, 'Zed', 'Tabby', 1, false)"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	res, err := mgr.Run(config.DemoBytdb, "SELECT id FROM cats")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(res.Rows) != 9 {
		t.Errorf("%d rows, want the 8 seeded plus the one inserted", len(res.Rows))
	}
}

// The ad-hoc --dsn connection rides along in a demo config, and it is the
// user's database: opening the demos must not open it, and using it must not
// seed it. Seeding runs DELETE FROM cats, so getting this wrong deletes the
// user's rows in any table that happens to be called cats.
func TestDemoSeedingLeavesUserConnectionAlone(t *testing.T) {
	cfg := demoCfg(t)
	userDSN := "file:" + filepath.Join(t.TempDir(), "user.db")
	cfg.Connections = append(cfg.Connections,
		config.Connection{Name: "dsn", Driver: "sqlite", DSN: userDSN})

	// the user's own cats table, with a row the seed script would delete
	setup := NewManager(cfg)
	for _, s := range []string{
		"CREATE TABLE cats (id INTEGER PRIMARY KEY, name TEXT, breed TEXT, age INTEGER, adopted BOOLEAN)",
		"INSERT INTO cats VALUES (99, 'Mine', 'Tabby', 1, false)",
	} {
		if _, err := setup.Run("dsn", s); err != nil {
			t.Fatalf("user db: %v", err)
		}
	}
	setup.Close()

	mgr := NewManager(cfg)
	t.Cleanup(mgr.Close)
	if err := SeedDemos(mgr, cfg); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if opened(mgr, "dsn") {
		t.Error("SeedDemos opened the user's connection")
	}
	if len(cfg.Connections) != 3 {
		t.Errorf("connections = %+v, want both demos and the user's kept", cfg.Connections)
	}
	res, err := mgr.Run("dsn", "SELECT name FROM cats")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(res.Rows) != 1 || res.Rows[0][0] != "Mine" {
		t.Errorf("user's cats = %v, want only their own row", res.Rows)
	}
}

// A headless run on the active demo opens that demo alone.
func TestOpenDefaultDemoOpensOnlyTheActiveDemo(t *testing.T) {
	cfg := demoCfg(t)
	mgr := NewManager(cfg)
	t.Cleanup(mgr.Close)

	if err := OpenDefaultDemo(mgr, cfg); err != nil {
		t.Fatalf("open: %v", err)
	}
	if !opened(mgr, config.DemoBytdb) || opened(mgr, config.DemoSQLite) {
		t.Errorf("opened bytdb=%v sqlite=%v, want only the active bytdb demo",
			opened(mgr, config.DemoBytdb), opened(mgr, config.DemoSQLite))
	}
	if len(cfg.Connections) != 2 || cfg.DefaultConnection != config.DemoBytdb {
		t.Errorf("config changed: default %q, connections %+v", cfg.DefaultConnection, cfg.Connections)
	}
}

// When the active demo cannot be opened — the bytdb file held by a running
// TUI, say — a headless run falls back to the other one, as the TUI does.
func TestOpenDefaultDemoFallsBack(t *testing.T) {
	cfg := demoCfg(t)
	cfg.Connections[0].DSN = t.TempDir() // a directory: bytdb cannot open it
	mgr := NewManager(cfg)
	t.Cleanup(mgr.Close)

	if err := OpenDefaultDemo(mgr, cfg); err != nil {
		t.Fatalf("open should fall back to the sqlite demo: %v", err)
	}
	if cfg.DefaultConnection != config.DemoSQLite {
		t.Errorf("default = %q, want the sqlite demo", cfg.DefaultConnection)
	}
	if len(cfg.Warnings) != 1 || !strings.Contains(cfg.Warnings[0], config.DemoBytdb) {
		t.Errorf("warnings = %v, want one naming the dropped demo", cfg.Warnings)
	}
	if _, err := mgr.Run(config.DemoSQLite, "SELECT id FROM cats"); err != nil {
		t.Errorf("sqlite demo: %v", err)
	}
}
