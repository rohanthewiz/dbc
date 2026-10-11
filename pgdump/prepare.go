package pgdump

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/db"
)

// What the three front ends share: the CLI (dumpcmd.go), the TUI's dump
// form (tui/dump.go) and dbc web's dialog (web/dump.go) all go
//
//	Form ─► Options ─► Prepare: connection → libpq, server version, pg_dump
//	                      └► *Run ─► Report: Do, with each line said as it comes
//
// so a dump started from any of them checks the same things, finds the
// same pg_dump and says the same words.

// ErrNotPostgres is a dump asked of a connection that is not Postgres.
var ErrNotPostgres = errors.New("dumps run pg_dump, which is for Postgres connections")

// ErrNoConn is a dump asked of a connection name the config does not have.
var ErrNoConn = errors.New("no such connection")

// IsPostgres reports whether driver (any alias db.Driver accepts) is
// Postgres — whether a connection can be dumped.
func IsPostgres(driver string) bool {
	d, err := db.Driver(driver)
	return err == nil && d == "pgx"
}

// Target is conn, from cfg, restated for pg_dump (db.LibpqFor). The errors
// wrap ErrNoConn and ErrNotPostgres, for a caller to tell bad usage apart.
func Target(cfg *config.Config, conn string) (db.LibpqConn, error) {
	cc, ok := cfg.ConnByName(conn)
	if !ok {
		return db.LibpqConn{}, fmt.Errorf("%w: %q", ErrNoConn, conn)
	}
	if !IsPostgres(cc.Driver) {
		return db.LibpqConn{}, fmt.Errorf("%w — %s is %s", ErrNotPostgres, conn, cc.Driver)
	}
	return db.LibpqFor(cc)
}

// ServerMajor asks conn's server its major version: server_version_num is
// 170002 for 17.2 and 90624 for 9.6.24 — whose major, for pg_dump's
// version check, is 9. It doubles as the connection check, so a refused
// password is said once, by dbc, rather than by every pg_dump worker.
func ServerMajor(ctx context.Context, mgr *db.Manager, conn string) (int, error) {
	dbh, err := mgr.DBContext(ctx, conn)
	if err != nil {
		return 0, err
	}
	var s string
	if err = dbh.QueryRowContext(ctx, "SHOW server_version_num").Scan(&s); err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, serr.New("unexpected server_version_num", "value", s)
	}
	return n / 10000, nil
}

// BinDir is where to look for the tools: flag (the CLI's --pg-bin, which
// reads $DBC_PG_BIN itself), else $DBC_PG_BIN, else the config's pg_bin,
// else "" — search (Locate).
func BinDir(flag string, cfg *config.Config) string {
	switch {
	case flag != "":
		return flag
	case os.Getenv("DBC_PG_BIN") != "":
		return os.Getenv("DBC_PG_BIN")
	}
	return cfg.PGBin
}

// Prepare readies a dump of conn: the options checked, the connection
// restated, the server asked its version, a pg_dump new enough found — or
// the server's image in Docker when none is (LocateOrDocker). The
// UIs call it as the dialog's Dump is pressed, so whatever is wrong is
// said in the dialog rather than later, in a log.
func Prepare(ctx context.Context, cfg *config.Config, mgr *db.Manager, conn string, opts Options) (*Run, error) {
	if err := opts.Check(); err != nil {
		return nil, err
	}
	if opts.Format.ToDir() {
		if err := CheckOutDir(opts.Out); err != nil {
			return nil, err
		}
	}
	lc, err := Target(cfg, conn)
	if err != nil {
		return nil, err
	}
	major, err := ServerMajor(ctx, mgr, conn)
	if err != nil {
		return nil, err
	}
	tools, err := LocateOrDocker(ctx, BinDir("", cfg), major, opts.Format == Split)
	if err != nil {
		return nil, err
	}
	return &Run{Name: conn, ServerMajor: major, Tools: tools, Conn: lc, Opts: opts}, nil
}

// Form is the UIs' dump dialog as typed: text as entered, checkboxes as
// ticked. Options turns it into Options; the JSON names are dbc web's.
type Form struct {
	Format string `json:"format"` // a ParseFormat name
	Out    string `json:"out"`    // ~ and relative paths allowed (ResolveOut)
	Jobs   string `json:"jobs"`   // "" for 1
	// Content is "" (everything), "schema" or "data".
	Content string `json:"content"`
	// The filters: pg_dump patterns, separated by commas or blanks; a
	// pattern with a blank in it is "quoted".
	Schemas       string `json:"schemas"`
	Tables        string `json:"tables"`
	ExcludeTables string `json:"exclude_tables"`
	ExcludeData   string `json:"exclude_data"`

	NoOwner      bool `json:"no_owner"`
	NoPrivileges bool `json:"no_privileges"`
	Inserts      bool `json:"inserts"`
	Create       bool `json:"create"`
	Clean        bool `json:"clean"`
	// Extra is any other pg_dump options, as on a command line.
	Extra string `json:"extra"`
}

// Options reads f. The restore-time choices (owner, create, clean) are
// dropped for an archive format rather than refused, as the CLI refuses
// them: the dialogs hide those boxes for an archive, so a tick left behind
// by a format change is not something the user can see to untick.
func (f Form) Options() (Options, error) {
	format, err := ParseFormat(f.Format)
	if err != nil {
		return Options{}, err
	}
	o := Options{Format: format, NoPrivileges: f.NoPrivileges, Inserts: f.Inserts}
	if o.Out, err = ResolveOut(f.Out); err != nil {
		return Options{}, err
	}
	if j := strings.TrimSpace(f.Jobs); j != "" {
		if o.Jobs, err = strconv.Atoi(j); err != nil || o.Jobs < 1 {
			return Options{}, serr.New("jobs is a count of tables to dump at once: 1 or more")
		}
		if o.Jobs == 1 {
			o.Jobs = 0 // one worker is no --jobs, which plain and custom accept
		}
	}
	switch f.Content {
	case "schema":
		o.SchemaOnly = true
	case "data":
		o.DataOnly = true
	}
	lists := []struct {
		in  string
		out *[]string
	}{{f.Schemas, &o.Schemas}, {f.Tables, &o.Tables}, {f.ExcludeTables, &o.ExcludeTables}, {f.ExcludeData, &o.ExcludeData}}
	for _, l := range lists {
		if *l.out, err = splitWords(l.in, true); err != nil {
			return Options{}, err
		}
	}
	if !format.Archive() {
		o.NoOwner, o.Create, o.Clean, o.IfExists = f.NoOwner, f.Create, f.Clean, f.Clean
	}
	if o.Extra, err = splitWords(f.Extra, false); err != nil {
		return Options{}, err
	}
	if len(o.Extra) > 0 && !strings.HasPrefix(o.Extra[0], "-") {
		return Options{}, serr.New("more options are pg_dump options, starting with - (like --no-comments)")
	}
	return o, o.Check()
}

// ResolveOut makes a typed output path absolute: ~ is the home directory,
// and a relative path is relative to dbc's working directory. Said in full
// in the log afterwards, so where the dump went is never a guess.
func ResolveOut(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", serr.New("name the file (or, for directory and split, the directory) to write")
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", serr.Wrap(err)
		}
		p = filepath.Join(home, p[1:])
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", serr.Wrap(err)
	}
	return abs, nil
}

// suffix is what DefaultOut ends a format's name with: the file
// extension, or for the directory formats a word saying what is inside.
func (f Format) suffix() string {
	switch f {
	case Custom:
		return ".dump"
	case Tar:
		return ".tar"
	case Directory:
		return "-dir"
	case Split:
		return "-sql"
	}
	return ".sql"
}

// DefaultOut is where the dialogs suggest writing conn's dump: Downloads
// (else the home directory), named for the connection and the minute —
// "~/Downloads/prod_analytics-20261009-1430.sql" — so a second dump never
// lands on the first.
func DefaultOut(conn string, f Format, now time.Time) string {
	dir := "."
	if home, err := os.UserHomeDir(); err == nil {
		dir = home
		if fi, err := os.Stat(filepath.Join(home, "Downloads")); err == nil && fi.IsDir() {
			dir = filepath.Join(home, "Downloads")
		}
	}
	return filepath.Join(dir, fileSafe(conn)+"-"+now.Format("20060102-1504")+f.suffix())
}

// SwapSuffix moves a path written for one format to another's suffix, when
// it still ends with the first's — the dialogs call it as the format
// changes, so the suggested "x.sql" becomes "x.dump", while a name the
// user typed themselves is left alone.
func SwapSuffix(p string, from, to Format) string {
	if s := from.suffix(); strings.HasSuffix(p, s) {
		return strings.TrimSuffix(p, s) + to.suffix()
	}
	return p
}

// RestoreHint is the command that reads a dump of format f at out back
// into a database.
func RestoreHint(f Format, out string) string {
	switch f {
	case Split:
		return "psql -X -d TARGET -f " + shellJoin([]string{filepath.Join(out, restoreFile)})
	case Plain:
		return "psql -X -d TARGET -f " + shellJoin([]string{out})
	}
	return "pg_restore -d TARGET " + shellJoin([]string{out})
}

// RestoreNote is what the restore hint should add for a dump pg_dump ran
// in Docker: an archive (custom, tar, directory) is in the server's
// version's format, which an older pg_restore — this machine's, or it
// would have dumped — cannot read. "" otherwise.
func (r *Run) RestoreNote() string {
	if r.Tools.Image == "" || !r.Opts.Format.Archive() {
		return ""
	}
	return fmt.Sprintf(" (pg_restore %d or newer: this machine's is older; %s has one)", r.Tools.Major, r.Tools.Image)
}

// Size is the bytes at p, a file or a directory's files; 0 if unreadable.
func Size(p string) int64 {
	var n int64
	_ = filepath.Walk(p, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			n += fi.Size()
		}
		return nil
	})
	return n
}

// HumanSize is n bytes for a log line: "512 B", "3.4 MB".
func HumanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// Report runs r for a UI, saying each step, each line of the tools' own
// output, and the outcome through say(level, text) — level "info", "warn",
// "err" or "ok", the log levels both UIs draw. The error is returned too,
// for a UI that wants to tell a cancel from a failure.
func (r *Run) Report(ctx context.Context, say func(level, text string)) error {
	r.Progress = func(s string) { say("info", s) }
	lines := Lines(func(l string) { say(toolLevel(l), l) })
	r.Stderr = lines
	from := " from " + filepath.Dir(r.Tools.Dump)
	if r.Tools.Image != "" {
		from = " — no pg_dump here is new enough for the server" // Version says where (in Docker)
	}
	say("info", fmt.Sprintf("dumping %s (PostgreSQL %d) as %s to %s, with pg_dump %s%s",
		r.Name, r.ServerMajor, r.Opts.Format, r.Opts.Out, r.Tools.Version, from))
	for _, k := range r.Conn.Dropped {
		say("info", "pg_dump connects without the DSN's "+k+" (a session setting libpq does not take)")
	}
	start := time.Now()
	err := r.Do(ctx)
	lines.Flush()
	switch {
	case ctx.Err() != nil:
		say("warn", "dump of "+r.Name+" stopped — nothing was kept")
	case err != nil:
		say("err", "dump of "+r.Name+" failed: "+serr.UserMsgFromErr(err, err.Error()))
	default:
		say("ok", fmt.Sprintf("dumped %s to %s (%s, %s) — restore with %s", r.Name, r.Opts.Out,
			HumanSize(Size(r.Opts.Out)), time.Since(start).Round(time.Second), RestoreHint(r.Opts.Format, r.Opts.Out)+r.RestoreNote()))
	}
	return err
}

// toolLevel is the log level of a line pg_dump or pg_restore wrote:
// "pg_dump: error: …" and "pg_dump: warning: …" say which themselves.
func toolLevel(l string) string {
	switch {
	case strings.Contains(l, ": error:"), strings.Contains(l, ": fatal:"):
		return "err"
	case strings.Contains(l, ": warning:"):
		return "warn"
	}
	return "info"
}

// LineWriter calls a function with each line written to it, without its
// newline; blank lines are skipped. It is safe for concurrent writers (a
// split's pg_restores share it), and a line is never split between two of
// them as long as each writes whole lines, as the tools do.
type LineWriter struct {
	mu  sync.Mutex
	buf []byte
	f   func(string)
}

// Lines makes a LineWriter calling f.
func Lines(f func(string)) *LineWriter { return &LineWriter{f: f} }

func (w *LineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := strings.IndexByte(string(w.buf), '\n')
		if i < 0 {
			return len(p), nil
		}
		w.emit(string(w.buf[:i]))
		w.buf = w.buf[i+1:]
	}
}

// Flush says what is left of an unfinished last line.
func (w *LineWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.emit(string(w.buf))
	w.buf = nil
}

func (w *LineWriter) emit(l string) {
	if l = strings.TrimRight(l, "\r \t"); l != "" {
		w.f(l)
	}
}

// splitWords splits s into words the way a shell would for these simple
// needs: blanks separate them (and commas too, with commas set — the
// pattern lists), 'single quotes' keep everything literal, "double quotes"
// allow \" and \\, and a backslash outside quotes escapes the next
// character. pg_dump's patterns use double quotes themselves ("MixedCase"
// matches a name exactly), so in a pattern list double quotes are kept, for
// pg_dump to read.
func splitWords(s string, commas bool) ([]string, error) {
	var out []string
	var cur strings.Builder
	inWord := false
	end := func() {
		if inWord {
			out = append(out, cur.String())
			cur.Reset()
			inWord = false
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || (commas && c == ','):
			end()
		case c == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return nil, serr.New("a ' quote is not closed", "text", s)
			}
			cur.WriteString(s[i+1 : i+1+j])
			i += j + 1
			inWord = true
		case c == '"':
			if commas {
				cur.WriteByte('"') // pg_dump's own quoting, kept for it
			}
			closed := false
			for i++; i < len(s); i++ {
				if s[i] == '\\' && !commas && i+1 < len(s) && (s[i+1] == '"' || s[i+1] == '\\') {
					i++
					cur.WriteByte(s[i])
					continue
				}
				if s[i] == '"' {
					// "" inside a pattern is pg_dump's escaped quote
					if commas && i+1 < len(s) && s[i+1] == '"' {
						cur.WriteString(`""`)
						i++
						continue
					}
					closed = true
					break
				}
				cur.WriteByte(s[i])
			}
			if !closed {
				return nil, serr.New(`a " quote is not closed`, "text", s)
			}
			if commas {
				cur.WriteByte('"')
			}
			inWord = true
		case c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
			inWord = true
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	end()
	return out, nil
}
