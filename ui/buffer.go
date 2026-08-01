package ui

import (
	"os"
	"path/filepath"
)

// bufferFile is where the editor buffer persists between sessions, under the
// user's config directory. It returns "" when no home directory is known, and
// the buffer simply does not persist.
func bufferFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "dbc", "buffer.sql")
}

// loadBuffer returns the persisted editor buffer, or "" when there is none.
func loadBuffer(path string) string {
	if path == "" {
		return ""
	}
	bs, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(bs)
}

// saveBuffer persists the editor buffer. The file is user-only: query
// scratchpads have a way of accumulating real values.
func saveBuffer(path, text string) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(text), 0o600)
}
