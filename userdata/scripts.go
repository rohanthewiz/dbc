package userdata

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/rohanthewiz/serr"
)

// Scripts: the Go files in the scripts directory (config.Config.ScriptsDir,
// already resolved to an absolute path). Plain files on purpose, not
// records in a store: a script has to stay runnable as `dbc script f.go`
// from cron and editable in any editor, and a file is the one form all of
// those read.
//
// Only the top level is listed, and only *.go: a dot-directory (a future
// .trash) and anything else beside the scripts are not scripts.

// ScriptInfo is one script, as a list shows it.
type ScriptInfo struct {
	Name string    `json:"name"` // file name, with .go
	Desc string    `json:"desc"` // its opening comment's first sentence; "" if none
	Size int64     `json:"size"`
	Mod  time.Time `json:"mod"`
}

// ListScripts lists the scripts in dir by name. A missing dir is none and
// no error: it is created on the first save, and most users never make one.
func ListScripts(dir string) ([]ScriptInfo, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, serr.Wrap(err, "op", "list scripts", "dir", dir)
	}
	var out []ScriptInfo
	for _, e := range ents {
		// a symlink to a script counts (the Stat below follows it); a
		// symlink to a directory is dropped there
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		st, err := os.Stat(p)
		if err != nil || !st.Mode().IsRegular() {
			continue // vanished since ReadDir, or a link to something else
		}
		out = append(out, ScriptInfo{Name: e.Name(), Desc: ScriptDesc(p), Size: st.Size(), Mod: st.ModTime()})
	}
	slices.SortFunc(out, func(a, b ScriptInfo) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// descMax caps a description: it is a line in a list, not the comment.
const descMax = 100

// ScriptDesc is a script's description, taken from the script itself so
// there is no sidecar file to keep in step: the first comment in the file,
// before the package clause, cut to its first sentence. Directives
// (//go:build) are not part of it — CommentGroup.Text drops them.
//
//	// Copy myschema.mytable from ProdDr to dev.   ─► "Copy myschema.mytable from ProdDr to dev."
//	// More detail on the next line.
//	//go:build ignore
//
// Only the header is parsed (PackageClauseOnly), so a script with a syntax
// error further down still has its description, and a large one costs no
// more than its first lines.
func ScriptDesc(path string) string {
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.PackageClauseOnly|parser.ParseComments)
	if err != nil || f == nil || len(f.Comments) == 0 {
		return ""
	}
	first := f.Comments[0]
	if first.Pos() > f.Package {
		return "" // the first comment is below the package clause
	}
	text := strings.Join(strings.Fields(first.Text()), " ")
	return firstSentence(text)
}

// firstSentence cuts s after its first ". " (or at the end), then to
// descMax runes with an ellipsis. A period inside a word (sdb.S, v1.2) is
// not a sentence end: only one followed by a space or the end counts.
func firstSentence(s string) string {
	if i := strings.Index(s, ". "); i >= 0 {
		s = s[:i+1]
	}
	if r := []rune(s); len(r) > descMax {
		s = strings.TrimRight(string(r[:descMax-1]), " ") + "…"
	}
	return s
}
