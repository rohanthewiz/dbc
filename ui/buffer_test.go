package ui

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBufferRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg", "buffer.sql")
	const text = "SELECT 1;\n-- scratch\nSELECT 2;"

	if err := saveBuffer(path, text); err != nil {
		t.Fatalf("saveBuffer: %v", err)
	}
	if got := loadBuffer(path); got != text {
		t.Errorf("loaded %q, want %q", got, text)
	}

	// the scratchpad can hold real values, so it must be user-only
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("buffer file mode %o, want 600", perm)
	}
}

func TestBufferMissingOrDisabled(t *testing.T) {
	if got := loadBuffer(filepath.Join(t.TempDir(), "absent.sql")); got != "" {
		t.Errorf("missing file loaded as %q, want empty", got)
	}
	// "" means no home directory: persistence is off, not an error
	if got := loadBuffer(""); got != "" {
		t.Errorf("empty path loaded as %q, want empty", got)
	}
	if err := saveBuffer("", "SELECT 1"); err != nil {
		t.Errorf("saveBuffer with empty path: %v", err)
	}
}
