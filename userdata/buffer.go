package userdata

import (
	"os"
	"path/filepath"
)

// BufferFile is where the editor buffer persists between sessions, under the
// user's config directory. It returns "" when no home directory is known, and
// the buffer simply does not persist.
func BufferFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "dbc", "buffer.sql")
}

// LoadBuffer returns the persisted editor buffer, or "" when there is none.
func LoadBuffer(path string) string {
	if path == "" {
		return ""
	}
	bs, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(bs)
}

// SaveBuffer persists the editor buffer. The file is user-only: query
// scratchpads have a way of accumulating real values.
func SaveBuffer(path, text string) error {
	if path == "" {
		return nil
	}
	// atomically: a console written in place and cut short — a crash, a
	// full disk — lost the user's SQL, the old text with the new (N-255)
	return writeAtomic(path, []byte(text), 0o600)
}
