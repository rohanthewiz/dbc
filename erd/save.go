package erd

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rohanthewiz/serr"
)

// FileName is the name a saved or downloaded rendering takes:
// erd-<conn>-<yyyymmdd-hhmmss>.<ext>, or erd-<stamp>.<ext> with no
// connection — the plan files' pattern (explain.Plan.FileName), so the two
// sort side by side in a downloads folder.
func (s *Schema) FileName(ext string) string {
	stamp := time.Now().Format("20060102-150405")
	if c := SafeFileName(s.Conn); c != "" {
		return "erd-" + c + "-" + stamp + "." + ext
	}
	return "erd-" + stamp + "." + ext
}

// WriteFile saves one rendering into dir (created if need be) under
// FileName and returns its path. 0600: a schema is the user's business, and
// dir is usually the shared temp directory.
func (s *Schema) WriteFile(dir, ext string, data []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", serr.Wrap(err, "dir", dir)
	}
	path := filepath.Join(dir, s.FileName(ext))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", serr.Wrap(err, "path", path)
	}
	return path, nil
}

// Dir is where the TUI keeps the diagrams it saves and opens.
func Dir() string { return filepath.Join(os.TempDir(), "dbc-erd") }

// SafeFileName keeps a connection name usable in a file name (and in a
// Content-Disposition header): anything but letters, digits, - and _
// becomes _.
func SafeFileName(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '-' || r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '_'
	}, s)
}
