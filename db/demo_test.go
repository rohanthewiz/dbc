package db

import (
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
			{Name: config.DemoBytdb, Driver: "bytdb", DSN: filepath.Join(t.TempDir(), "demo.bytdb")},
			{Name: config.DemoSQLite, Driver: "sqlite", DSN: "file:seedtest?mode=memory&cache=shared"},
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
