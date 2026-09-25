package explain

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rohanthewiz/serr"
)

// WriteHTML saves the interactive page into dir (created if need be) under a
// name that says what and when — plan-<conn>-<yyyymmdd-hhmmss>.html — and
// returns its path. The file is 0600: a plan quotes the statement and its
// literals, which are the user's business, and dir is usually the shared
// temp directory.
func (p *Plan) WriteHTML(dir string) (string, error) {
	page, err := p.HTML()
	if err != nil {
		return "", err
	}
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return "", serr.Wrap(err, "dir", dir)
	}
	name := "plan-" + time.Now().Format("20060102-150405") + ".html"
	if c := safeFileName(p.Conn); c != "" {
		name = "plan-" + c + "-" + time.Now().Format("20060102-150405") + ".html"
	}
	path := filepath.Join(dir, name)
	if err = os.WriteFile(path, []byte(page), 0o600); err != nil {
		return "", serr.Wrap(err, "path", path)
	}
	return path, nil
}

// PlanDir is where plans opened in a browser are kept by default.
func PlanDir() string { return filepath.Join(os.TempDir(), "dbc-plans") }

// safeFileName keeps a connection name usable in a file name.
func safeFileName(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '-' || r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '_'
	}, s)
}
