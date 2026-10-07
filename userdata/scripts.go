package userdata

import (
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rohanthewiz/serr"
)

// Scripts: the Go files in the scripts directory (config.Config.ScriptsDir,
// already resolved to an absolute path). Plain files on purpose, not
// records in a store: a script has to stay runnable as `dbc script f.go`
// from cron and editable in any editor, and a file is the one form all of
// those read.
//
// Only the top level is listed, and only *.go that is a script: a
// dot-directory (.trash) and anything else beside the scripts are not
// scripts, and neither is a Go file of some other package (the repo's
// scripts/embed.go, when scripts_dir is a checkout's ./scripts).
//
// The store mirrors the consoles' (console.go): a save names the revision
// it was made from (textRev, a content hash), and is refused as a conflict
// when the file has changed since — whoever changed it (another dbc web
// window, the TUI, vim). A delete is a move into .trash, which keeps the
// most recent trashMax:
//
//	<scripts dir>/
//	    copy_mytable.go                  ◄─ a script: its NAME is the file name
//	    nightly_report.go
//	    .trash/
//	        old_copy.1759769402123.go    ◄─ old_copy.go, trashed at that Unix ms
//
// Every function takes the directory and a NAME, never a path, and checks
// the name with ValidScriptName: names come from the browser, and must not
// be able to climb out of the directory or hide.

// ScriptInfo is one script, as a list shows it.
type ScriptInfo struct {
	Name string    `json:"name"` // file name, with .go
	Desc string    `json:"desc"` // its opening comment's first sentence; "" if none
	Size int64     `json:"size"`
	Mod  time.Time `json:"mod"`
	Rev  string    `json:"rev"` // textRev of its content: what a save names as its base
}

// ValidScriptName reports whether name may name a script: the console-name
// rule (console.go) for the stem, plus a required .go suffix. So a plain
// file name with nothing that could climb out of the directory, hide (a
// leading dot) or trip up another filesystem; ".go" alone has no stem, and
// "a..go" is refused as a console name ending in a dot is.
func ValidScriptName(name string) bool {
	stem, ok := strings.CutSuffix(name, ".go")
	return ok && ValidConsoleName(stem)
}

// ErrScriptExists is a create, rename or restore onto a name the scripts
// directory already has.
var ErrScriptExists = errors.New("a script by that name already exists")

// ErrBadScriptName is a name ValidScriptName refuses.
var ErrBadScriptName = errors.New("not a script name: letters, digits, . _ - ending in .go")

// MaxScriptBytes bounds what a save writes (and what dbc web checks). A
// script is a page of Go; a megabyte is a mistake (a pasted data file),
// not a script.
const MaxScriptBytes = 1 << 20

// scriptMu serialises this process's read-compare-write sequences, so two
// dbc web requests saving one script cannot both pass the revision check.
// Another process (the TUI, vim) is not covered: its write lands between
// our check and rename only in a window of microseconds, and the next save
// from the loser is then refused as a conflict, so nothing is lost silently
// for long. One lock for every directory: saves are rare and quick.
var scriptMu sync.Mutex

// scriptPath joins a checked name to dir.
func scriptPath(dir, name string) (string, error) {
	if dir == "" {
		return "", serr.New("no scripts directory")
	}
	if !ValidScriptName(name) {
		return "", serr.Wrap(ErrBadScriptName, "name", name)
	}
	return filepath.Join(dir, name), nil
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
		src, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		h := scriptHeaderOf(src)
		if h.otherPkg {
			continue // Go, but not a script (see the top of this file)
		}
		out = append(out, ScriptInfo{Name: e.Name(), Desc: h.desc, Size: st.Size(),
			Mod: st.ModTime(), Rev: textRev(string(src), true)})
	}
	slices.SortFunc(out, func(a, b ScriptInfo) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// ReadScript returns a script's text and revision. A missing script is an
// error that errors.Is fs.ErrNotExist.
func ReadScript(dir, name string) (text, rev string, err error) {
	p, err := scriptPath(dir, name)
	if err != nil {
		return "", "", err
	}
	bs, err := os.ReadFile(p)
	if err != nil {
		return "", "", serr.Wrap(err, "op", "read script", "name", name)
	}
	return string(bs), textRev(string(bs), true), nil
}

// SaveScript writes text as script name, made from revision base:
//
//	base   the file now           →  result
//	─────  ─────────────────────     ───────────────────────────────────
//	""     missing                   created (the directory too)
//	""     exists                    ErrScriptExists: a create never overwrites
//	rev    has revision rev          written
//	rev    changed, or deleted       conflict: nothing written, curRev is
//	                                 the file's revision now ("" if gone)
//
// On success rev is the new revision. A conflict is not an error: the caller
// shows the file's text and lets the user choose (as consoles do). To put
// back a script deleted elsewhere, save it again with base "".
//
// The write is atomic (writeAtomic) and keeps an existing file's mode. A
// symlinked script is written through to its target, so the link stays a
// link. A new script is user-only (0600), as consoles are: scripts name
// connections and hold queries, which have a way of collecting real values.
func SaveScript(dir, name, text, base string) (rev string, conflict bool, err error) {
	p, err := scriptPath(dir, name)
	if err != nil {
		return "", false, err
	}
	if len(text) > MaxScriptBytes {
		return "", false, serr.New("script too large", "name", name,
			"bytes", strconv.Itoa(len(text)), "max", strconv.Itoa(MaxScriptBytes))
	}
	scriptMu.Lock()
	defer scriptMu.Unlock()

	perm := os.FileMode(0o600)
	cur, readErr := os.ReadFile(p)
	exists := readErr == nil
	if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
		return "", false, serr.Wrap(readErr, "op", "save script", "name", name)
	}
	if curRev := textRev(string(cur), exists); curRev != base {
		if base == "" {
			return curRev, false, serr.Wrap(ErrScriptExists, "name", name)
		}
		return curRev, true, nil
	}
	if exists {
		if target, err := filepath.EvalSymlinks(p); err == nil {
			p = target
		}
		if st, err := os.Stat(p); err == nil {
			perm = st.Mode().Perm()
		}
	}
	if err = writeAtomic(p, []byte(text), perm); err != nil {
		return "", false, serr.Wrap(err, "op", "save script", "name", name)
	}
	return textRev(text, true), false, nil
}

// RenameScript renames script from to to, refusing to replace another
// script. A rename that changes only the case of the name is allowed on a
// case-insensitive filesystem (macOS's default), where "to" already
// "exists" because it is the same file.
func RenameScript(dir, from, to string) error {
	src, err := scriptPath(dir, from)
	if err != nil {
		return err
	}
	dst, err := scriptPath(dir, to)
	if err != nil {
		return err
	}
	if from == to {
		return nil
	}
	scriptMu.Lock()
	defer scriptMu.Unlock()
	srcSt, err := os.Lstat(src)
	if err != nil {
		return serr.Wrap(err, "op", "rename script", "name", from)
	}
	if dstSt, err := os.Lstat(dst); err == nil && !os.SameFile(srcSt, dstSt) {
		return serr.Wrap(ErrScriptExists, "name", to)
	}
	if err = os.Rename(src, dst); err != nil {
		return serr.Wrap(err, "op", "rename script", "from", from, "to", to)
	}
	return nil
}

// trashDir is where trashed scripts go: a dot-directory, so ListScripts and
// a shell glob of *.go skip it.
const trashDir = ".trash"

// trashMax is how many trashed scripts are kept; older ones are deleted for
// good as new ones arrive. Enough to undo a bad afternoon, not an archive.
const trashMax = 50

// trashIDRe splits a trash file name into the script's stem and the Unix
// milliseconds it was trashed at.
var trashIDRe = regexp.MustCompile(`^(.+)\.([0-9]{1,19})\.go$`)

// TrashInfo is one trashed script.
type TrashInfo struct {
	ID      string    `json:"id"`      // its file name in .trash: what RestoreScript takes
	Name    string    `json:"name"`    // the script's name when it was trashed
	Trashed time.Time `json:"trashed"` // when
}

// parseTrashID checks a trash ID and returns the script name it holds and
// when it was trashed. Like a script name it comes from the browser, so it
// must name a file directly in .trash and nothing else.
func parseTrashID(id string) (name string, at time.Time, ok bool) {
	m := trashIDRe.FindStringSubmatch(id)
	if m == nil || !ValidScriptName(m[1]+".go") {
		return "", time.Time{}, false
	}
	ms, err := strconv.ParseInt(m[2], 10, 64)
	if err != nil {
		return "", time.Time{}, false
	}
	return m[1] + ".go", time.UnixMilli(ms), true
}

// TrashScript moves a script into .trash and returns its trash ID. The ID
// carries the time in milliseconds, so trashing a script, making another by
// the same name and trashing that keeps both. Past trashMax, the oldest
// trashed scripts are deleted.
func TrashScript(dir, name string) (id string, err error) {
	src, err := scriptPath(dir, name)
	if err != nil {
		return "", err
	}
	scriptMu.Lock()
	defer scriptMu.Unlock()
	if _, err = os.Lstat(src); err != nil {
		return "", serr.Wrap(err, "op", "trash script", "name", name)
	}
	td := filepath.Join(dir, trashDir)
	if err = os.MkdirAll(td, 0o755); err != nil {
		return "", serr.Wrap(err, "op", "trash script", "name", name)
	}
	// Two trashes of one name in the same millisecond (a test, a script)
	// would collide: step the stamp until it is free. The stamp is a key,
	// not a record, so a millisecond off does not matter.
	stem := strings.TrimSuffix(name, ".go")
	ms := time.Now().UnixMilli()
	for {
		id = fmt.Sprintf("%s.%d.go", stem, ms)
		if _, err := os.Lstat(filepath.Join(td, id)); errors.Is(err, fs.ErrNotExist) {
			break
		}
		ms++
	}
	if err = os.Rename(src, filepath.Join(td, id)); err != nil {
		return "", serr.Wrap(err, "op", "trash script", "name", name)
	}
	pruneTrash(td)
	return id, nil
}

// pruneTrash deletes all but the newest trashMax trashed scripts. Best
// effort: a file it cannot remove only means the trash is a bit larger.
func pruneTrash(td string) {
	items := listTrash(td)
	for _, it := range items[min(len(items), trashMax):] {
		_ = os.Remove(filepath.Join(td, it.ID))
	}
}

// ListTrash lists the trashed scripts in dir, newest first. No trash is
// none and no error.
func ListTrash(dir string) []TrashInfo {
	if dir == "" {
		return nil
	}
	return listTrash(filepath.Join(dir, trashDir))
}

func listTrash(td string) []TrashInfo {
	ents, err := os.ReadDir(td)
	if err != nil {
		return nil
	}
	var out []TrashInfo
	for _, e := range ents {
		if name, at, ok := parseTrashID(e.Name()); ok && e.Type().IsRegular() {
			out = append(out, TrashInfo{ID: e.Name(), Name: name, Trashed: at})
		}
	}
	slices.SortFunc(out, func(a, b TrashInfo) int {
		if c := b.Trashed.Compare(a.Trashed); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

// RestoreScript moves trashed script id back out of .trash, as to — or
// under its old name when to is "". It refuses to replace a script
// (ErrScriptExists): a user who made a new script by the old name restores
// the old one beside it, under another name. It returns the name used.
func RestoreScript(dir, id, to string) (string, error) {
	name, _, ok := parseTrashID(id)
	if !ok {
		return "", serr.New("not a trashed script", "id", id)
	}
	if to == "" {
		to = name
	}
	dst, err := scriptPath(dir, to)
	if err != nil {
		return "", err
	}
	scriptMu.Lock()
	defer scriptMu.Unlock()
	if _, err = os.Lstat(dst); err == nil {
		return "", serr.Wrap(ErrScriptExists, "name", to)
	}
	if err = os.Rename(filepath.Join(dir, trashDir, id), dst); err != nil {
		return "", serr.Wrap(err, "op", "restore script", "id", id)
	}
	return to, nil
}

// descMax caps a description: it is a line in a list, not the comment.
const descMax = 100

// ScriptDesc is the description of the script at path (see DescOf); ""
// when the file cannot be read.
func ScriptDesc(path string) string {
	src, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return DescOf(src)
}

// DescOf is a script's description, taken from the script itself so there
// is no sidecar file to keep in step: the first comment in the file, before
// the package clause, cut to its first sentence. Directives (//go:build)
// are not part of it — CommentGroup.Text drops them.
//
//	// Copy myschema.mytable from ProdDr to dev.   ─► "Copy myschema.mytable from ProdDr to dev."
//	// More detail on the next line.
//	//go:build ignore
//
// It takes the source rather than a path so that a script not on disk (an
// embedded example) is described the same way.
func DescOf(src []byte) string { return scriptHeaderOf(src).desc }

// scriptHeader is what the lines down to the package clause say about a
// file: its description, and whether it is plainly some other package.
type scriptHeader struct {
	desc     string
	otherPkg bool
}

// scriptHeaderOf parses only the header (PackageClauseOnly), so a script
// with a syntax error further down still has its description, and a large
// one costs no more than its first lines. A header that does not parse is
// a script being written (desc "", otherPkg false): only a clean
// `package x` with x not main rules a file out.
func scriptHeaderOf(src []byte) scriptHeader {
	f, err := parser.ParseFile(token.NewFileSet(), "", src, parser.PackageClauseOnly|parser.ParseComments)
	if err != nil || f == nil {
		return scriptHeader{}
	}
	h := scriptHeader{otherPkg: f.Name != nil && f.Name.Name != "main"}
	if len(f.Comments) == 0 || f.Comments[0].Pos() > f.Package {
		return h // no comment, or the first is below the package clause
	}
	h.desc = firstSentence(strings.Join(strings.Fields(f.Comments[0].Text()), " "))
	return h
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
