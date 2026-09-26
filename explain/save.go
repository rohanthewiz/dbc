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
	return p.WriteFile(dir, "html", []byte(page))
}

// WriteFile saves one rendering of the plan (the page, a PDF, a JPEG, the
// Mermaid source) into dir under the same what-and-when name WriteHTML
// uses, with ext as its extension, and returns its path. 0600 for the
// reason WriteHTML gives: every rendering quotes the statement.
func (p *Plan) WriteFile(dir, ext string, data []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", serr.Wrap(err, "dir", dir)
	}
	path := filepath.Join(dir, p.FileName(ext))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", serr.Wrap(err, "path", path)
	}
	return path, nil
}

// FileName is the name a saved or downloaded rendering of the plan takes:
// plan-<conn>-<yyyymmdd-hhmmss>.<ext>, or plan-<stamp>.<ext> with no
// connection.
func (p *Plan) FileName(ext string) string {
	stamp := time.Now().Format("20060102-150405")
	if c := safeFileName(p.Conn); c != "" {
		return "plan-" + c + "-" + stamp + "." + ext
	}
	return "plan-" + stamp + "." + ext
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
