// Package pgdump dumps a Postgres database by running PostgreSQL's own
// pg_dump (and, for the split format, pg_restore) against a dbc connection
// — `dbc dump` on the command line.
//
// It is UI-free, like pgdocker: the CLI (dumpcmd.go) turns flags into
// Options, finds the tools with Locate, and hands both to a Run.
//
// WHY pg_dump AND NOT A DUMPER OF dbc's OWN. A correct dump is a lot more
// than tables and rows: dependency ordering, ownership and grants,
// extensions, sequences' positions, large objects, a consistent snapshot
// across parallel workers, and a format pg_restore reads back. pg_dump does
// all of it and is the format every Postgres user already trusts. What dbc
// adds is the connection — the one already configured, TLS and derived
// databases included (db.LibpqFor) — finding a pg_dump new enough for the
// server, and a split format pg_dump does not have.
//
// THE FORMATS:
//
//	plain      one .sql file (or stdout), for psql
//	custom     one compressed archive, for pg_restore (selective restore)
//	tar        one tar archive, for pg_restore
//	directory  one file per table plus a table of contents, for pg_restore;
//	           the only format pg_dump can write with parallel --jobs
//	split      dbc's: plain SQL, one file per table, for psql and for
//	           reading or diffing — see split.go
package pgdump

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/db"
)

// Format is the shape of a dump.
type Format string

const (
	Plain     Format = "plain"
	Custom    Format = "custom"
	Tar       Format = "tar"
	Directory Format = "directory"
	Split     Format = "split"
)

// Formats lists every format, for help text and messages.
var Formats = []Format{Plain, Custom, Directory, Tar, Split}

// ParseFormat reads a format name, pg_dump's one-letter ones included.
// "text", the global -t flag's default, is plain: it is the dump's text form.
func ParseFormat(s string) (Format, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "text", "plain", "p", "sql":
		return Plain, nil
	case "custom", "c":
		return Custom, nil
	case "directory", "dir", "d":
		return Directory, nil
	case "tar", "t":
		return Tar, nil
	case "split":
		return Split, nil
	}
	return "", fmt.Errorf("dump writes plain, custom, directory, tar or split — not %q", s)
}

// Archive reports whether f is one of pg_dump's archive formats, read back
// with pg_restore rather than psql.
func (f Format) Archive() bool { return f == Custom || f == Tar || f == Directory }

// Binary reports whether f is not text, and so must not go to a terminal.
func (f Format) Binary() bool { return f == Custom || f == Tar }

// ToDir reports whether f writes a directory rather than one file.
func (f Format) ToDir() bool { return f == Directory || f == Split }

// Options are what to dump and how. The fields mirror pg_dump's options of
// the same names; the comments say only where dbc's meaning differs.
type Options struct {
	Format Format
	// Out is the file, or for directory and split the directory, to write.
	// "" is stdout, for the single-file formats.
	Out string
	// Jobs dumps that many tables at once (directory and split only).
	// pg_dump's workers share one snapshot, so the dump stays consistent.
	Jobs int

	SchemaOnly, DataOnly bool
	// The filters take pg_dump's patterns: "public", "sales.*", "*log*".
	Schemas, ExcludeSchemas            []string
	Tables, ExcludeTables, ExcludeData []string

	Clean, IfExists, Create bool
	NoOwner, NoPrivileges   bool
	Inserts, ColumnInserts  bool
	// Compress is pg_dump's --compress: a level ("6") or, from
	// PostgreSQL 16, method[:detail] ("zstd:3", "lz4").
	Compress string
	Encoding string

	// KeepArchive keeps, for split, the directory-format archive the SQL
	// files were made from, as Out/archive — restorable with pg_restore.
	KeepArchive bool

	// Extra are pg_dump options dbc has no flag for, passed after its own:
	// `dbc dump -- --no-comments --lock-wait-timeout=10s`.
	Extra []string
}

// managedOptions are pg_dump options dbc sets itself (where to connect, the
// format, the output, the workers); one in Extra would fight them.
var managedOptions = []struct{ long, short string }{
	{"--file", "-f"}, {"--format", "-F"}, {"--dbname", "-d"}, {"--host", "-h"}, {"--port", "-p"},
	{"--username", "-U"}, {"--password", "-W"}, {"--no-password", "-w"}, {"--jobs", "-j"},
}

// Check turns away options that cannot work together, before anything is
// run. Following dbc's rule for flags, one that pg_dump would silently
// ignore is refused too: a flag that does nothing reads as a bug.
func (o Options) Check() error {
	switch {
	case !slices.Contains(Formats, o.Format):
		return serr.New("unknown dump format", "format", string(o.Format))
	case o.SchemaOnly && o.DataOnly:
		return serr.New("--schema-only and --data-only cannot both be given")
	case o.Format.ToDir() && o.Out == "":
		return serr.New(fmt.Sprintf("the %s format writes a directory — name it with -o", o.Format))
	case o.Jobs < 0:
		return serr.New("--jobs is a count of workers, 1 or more")
	case o.Jobs > 1 && !o.Format.ToDir():
		return serr.New(fmt.Sprintf("--jobs needs -t directory or -t split: pg_dump writes %s with one worker", o.Format))
	case o.KeepArchive && o.Format != Split:
		return serr.New("--keep-archive is for -t split (the other formats are the archive, or have none)")
	case o.Compress != "" && o.Format == Split && !o.KeepArchive:
		return serr.New("-t split writes plain SQL; --compress applies only to its archive, so it needs --keep-archive")
	case o.IfExists && !o.Clean:
		return serr.New("--if-exists goes with --clean")
	case o.Format == Split && o.Clean && !o.Create:
		// Each file is restored on its own, so --clean's DROPs could not be
		// ordered across them: pre-data would drop a table the old post-data
		// foreign keys still depend on. With --create the whole database
		// is dropped and made anew, which needs no ordering.
		return serr.New("-t split can --clean only with --create (drop and recreate the database); " +
			"to drop objects one by one use -t plain --clean")
	}
	if o.Format.Archive() {
		// pg_dump ignores these for an archive: they are restore-time
		// choices, given to pg_restore instead
		for _, f := range []struct {
			on   bool
			name string
		}{{o.Clean, "--clean"}, {o.Create, "--create"}, {o.NoOwner, "--no-owner"}} {
			if f.on {
				return serr.New(fmt.Sprintf("%s does nothing in a %s archive — give it to pg_restore when restoring "+
					"(or dump with -t plain or -t split)", f.name, o.Format))
			}
		}
	}
	for _, a := range o.Extra {
		for _, m := range managedOptions {
			if a == m.long || strings.HasPrefix(a, m.long+"=") || (!strings.HasPrefix(a, "--") && strings.HasPrefix(a, m.short)) {
				return serr.New(fmt.Sprintf("dbc sets %s itself — use dbc's own flags (-c, -t, -o, --jobs) "+
					"rather than passing it to pg_dump", m.long), "arg", a)
			}
		}
	}
	return nil
}

// dumpArgs are pg_dump's arguments for o, writing format to file ("" =
// stdout), with conninfo as the database. Long option names throughout, so
// --dry-run prints a command a person can read. The SQL-shaping options
// that pg_dump ignores for an archive are left out for split's archive:
// pg_restore applies them when it writes the SQL (split.go).
func (o Options) dumpArgs(format Format, file, conninfo string, jobs int) []string {
	a := []string{"--format=" + string(format)}
	if file != "" {
		a = append(a, "--file="+file)
	}
	if jobs > 1 {
		a = append(a, "--jobs="+strconv.Itoa(jobs))
	}
	flag := func(on bool, name string) {
		if on {
			a = append(a, name)
		}
	}
	each := func(name string, vs []string) {
		for _, v := range vs {
			a = append(a, name+"="+v)
		}
	}
	flag(o.SchemaOnly, "--schema-only")
	flag(o.DataOnly, "--data-only")
	each("--schema", o.Schemas)
	each("--exclude-schema", o.ExcludeSchemas)
	each("--table", o.Tables)
	each("--exclude-table", o.ExcludeTables)
	each("--exclude-table-data", o.ExcludeData)
	if !format.Archive() {
		flag(o.Clean, "--clean")
		flag(o.IfExists, "--if-exists")
		flag(o.Create, "--create")
		flag(o.NoOwner, "--no-owner")
	}
	flag(o.NoPrivileges, "--no-privileges")
	flag(o.Inserts, "--inserts")
	flag(o.ColumnInserts, "--column-inserts")
	if o.Compress != "" {
		a = append(a, "--compress="+o.Compress)
	}
	if o.Encoding != "" {
		a = append(a, "--encoding="+o.Encoding)
	}
	a = append(a, o.Extra...)
	// dbc never answers a password prompt: with one, a run with a stdin
	// that is not a terminal would hang rather than fail
	return append(a, "--no-password", "--dbname="+conninfo)
}

// Tools are the PostgreSQL client programs a dump runs.
type Tools struct {
	Dump    string // pg_dump's path
	Restore string // pg_restore's, beside it; "" if not there
	Version string // "17.2", as pg_dump reports it
	Major   int
}

// probeDirs are where PostgreSQL's client tools are commonly installed but
// not on PATH: Homebrew's libpq and postgresql@N are keg-only (deliberately
// left off PATH), and Postgres.app, Debian and RHEL keep each major
// version's binaries in a directory of their own.
var probeDirs = []string{
	"/opt/homebrew/opt/libpq/bin",
	"/usr/local/opt/libpq/bin",
	"/opt/homebrew/opt/postgresql@*/bin",
	"/usr/local/opt/postgresql@*/bin",
	"/Applications/Postgres.app/Contents/Versions/*/bin",
	"/usr/lib/postgresql/*/bin",
	"/usr/pgsql-*/bin",
}

// versionRE finds the version in `pg_dump --version`'s
// "pg_dump (PostgreSQL) 17.2 (Homebrew)".
var versionRE = regexp.MustCompile(`\)\s+(\d+)(?:\.(\d+))?`)

// Locate picks the pg_dump to run: from binDir when one is given, else the
// first on PATH, else from probeDirs.
//
// pg_dump refuses a server of a newer major version than its own ("aborting
// because of server version mismatch"), and a machine often has several —
// an old one on PATH, a newer keg from Homebrew. So with serverMajor known
// (> 0) every candidate is asked its version, and the first that is new
// enough wins; a newer pg_dump reads an older server fine. needRestore
// skips a directory without pg_restore (the split format runs both).
func Locate(ctx context.Context, binDir string, serverMajor int, needRestore bool) (Tools, error) {
	var dirs []string
	if binDir != "" {
		dirs = []string{binDir}
	} else {
		if p, err := exec.LookPath("pg_dump"); err == nil {
			dirs = append(dirs, filepath.Dir(p))
		}
		for _, pat := range probeDirs {
			m, _ := filepath.Glob(pat)
			// newest version first within a pattern: postgresql@17 before @16
			slices.SortFunc(m, func(a, b string) int { return versionKey(b) - versionKey(a) })
			dirs = append(dirs, m...)
		}
	}

	var found []Tools
	seen := map[string]bool{}
	for _, d := range dirs {
		dump := filepath.Join(d, "pg_dump")
		real, err := filepath.EvalSymlinks(dump)
		if err != nil || seen[real] {
			continue // not there, or a link to one already tried
		}
		seen[real] = true
		t := Tools{Dump: dump}
		if r := filepath.Join(d, "pg_restore"); isExec(r) {
			t.Restore = r
		}
		if needRestore && t.Restore == "" {
			continue
		}
		if t.Version, t.Major, err = toolVersion(ctx, dump); err != nil {
			continue
		}
		if serverMajor <= 0 || t.Major >= serverMajor {
			return t, nil
		}
		found = append(found, t)
	}

	what := "pg_dump"
	if needRestore {
		what = "pg_dump and pg_restore"
	}
	if len(found) == 0 {
		where := "on PATH or in the usual install locations"
		if binDir != "" {
			where = "in " + binDir
		}
		return Tools{}, serr.New(fmt.Sprintf("no %s found %s — install PostgreSQL's client tools "+
			"(macOS: brew install libpq; Debian/Ubuntu: apt install postgresql-client) "+
			"or point dbc at their directory: --pg-bin, $DBC_PG_BIN, or pg_bin in the config", what, where))
	}
	var vs []string
	for _, t := range found {
		vs = append(vs, t.Version+" ("+t.Dump+")")
	}
	return Tools{}, serr.New(fmt.Sprintf("the server is PostgreSQL %d, and pg_dump cannot dump a server newer than itself — "+
		"found only %s; install client tools %d or newer (macOS: brew install libpq), "+
		"or point dbc at them: --pg-bin, $DBC_PG_BIN, or pg_bin in the config",
		serverMajor, strings.Join(vs, ", "), serverMajor))
}

var dirVersionRE = regexp.MustCompile(`(\d+)(?:\.(\d+))?`)

// versionKey orders install directories by the version in their path
// (".../postgresql@17/bin", "/usr/lib/postgresql/16/bin"); 0 for none.
func versionKey(dir string) int {
	m := dirVersionRE.FindAllStringSubmatch(dir, -1)
	if len(m) == 0 {
		return 0
	}
	last := m[len(m)-1]
	major, _ := strconv.Atoi(last[1])
	minor, _ := strconv.Atoi(last[2])
	return major*1000 + minor
}

func isExec(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0
}

// toolVersion runs `TOOL --version`. Bounded by a timeout: a wrapper script
// that hangs must not hang the dump before it starts.
func toolVersion(ctx context.Context, path string) (version string, major int, err error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return "", 0, err
	}
	m := versionRE.FindStringSubmatch(string(out))
	if m == nil {
		return "", 0, serr.New("could not read the version of "+path, "output", strings.TrimSpace(string(out)))
	}
	major, _ = strconv.Atoi(m[1])
	version = m[1]
	if m[2] != "" {
		version += "." + m[2]
	}
	return version, major, nil
}

// serviceName is the libpq service the tools are pointed at; see
// db.LibpqConn for why the connection travels in a service file.
const serviceName = "dbc"

// conninfo is the --dbname the tools are given: the service, and nothing
// secret.
const conninfo = "service=" + serviceName

// Run is one dump.
type Run struct {
	// Name is the dbc connection, for messages; ServerMajor its server's
	// version, as Prepare found it (0 when not asked).
	Name        string
	ServerMajor int

	Tools Tools
	Conn  db.LibpqConn
	Opts  Options
	// Stdout receives the dump when it goes to stdout (no Opts.Out).
	Stdout io.Writer
	// Stderr receives the tools' own messages (warnings, errors).
	Stderr io.Writer
	// Progress, if set, is told what is happening, a line at a time.
	Progress func(string)

	env  []string  // the tools' environment, set by Do
	errw io.Writer // Stderr, made safe for split's concurrent pg_restores
}

// lockedWriter serializes writes to w.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// partialSuffix marks the output of a dump still being written; see Do.
const partialSuffix = ".partial"

func (r *Run) say(format string, args ...any) {
	if r.Progress != nil {
		r.Progress(fmt.Sprintf(format, args...))
	}
}

// Commands are the command lines Do would run, for --dry-run. For split
// that is the archive's pg_dump and a description of the conversions, whose
// list of tables is only known once the archive exists.
func (r *Run) Commands() []string {
	switch r.Opts.Format {
	case Split:
		return []string{
			shellJoin(append([]string{r.Tools.Dump}, r.splitDumpArgs(r.archiveDir())...)),
			"# then pg_restore writes each part of the archive as SQL into " + r.Opts.Out +
				" (pre-data, one file per table, post-data) and dbc writes restore.sql",
		}
	default:
		return []string{shellJoin(append([]string{r.Tools.Dump}, r.Opts.dumpArgs(r.Opts.Format, r.Opts.Out, conninfo, r.Opts.Jobs)...))}
	}
}

// Do runs the dump. Canceling ctx interrupts the tools (SIGINT, as Ctrl+C
// would), which end their server sessions.
func (r *Run) Do(ctx context.Context) (err error) {
	if err = r.Opts.Check(); err != nil {
		return err
	}
	// The service file lives in a directory of its own that only this user
	// can enter, and goes when the run ends, however it ends.
	tmp, err := os.MkdirTemp("", "dbc-dump-")
	if err != nil {
		return serr.Wrap(err)
	}
	defer os.RemoveAll(tmp)
	svc := filepath.Join(tmp, "pg_service.conf")
	if err = os.WriteFile(svc, r.Conn.ServiceFile(serviceName), 0o600); err != nil {
		return serr.Wrap(err)
	}
	r.env = append(os.Environ(), "PGSERVICEFILE="+svc)
	if r.Conn.Password != "" {
		r.env = append(r.env, "PGPASSWORD="+r.Conn.Password)
	}

	stdout, stderr := r.Stdout, r.Stderr
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	r.errw = &lockedWriter{w: stderr}

	if r.Opts.Format == Split {
		return r.split(ctx) // cleans up after itself (split.go)
	}
	if r.Opts.Out == "" {
		return r.run(ctx, r.Tools.Dump, r.Opts.dumpArgs(r.Opts.Format, "", conninfo, r.Opts.Jobs), stdout)
	}
	// pg_dump writes its output in place as it goes, and leaves what it
	// wrote when it fails or is interrupted: a truncated file that looks
	// like a dump. So it writes OUT.partial, renamed to OUT only once it
	// has finished, and removed otherwise. A failed dump then never leaves
	// a half-written OUT — nor clobbers a good one already there.
	partial := r.Opts.Out + partialSuffix
	if err = os.RemoveAll(partial); err != nil { // one a crash left behind
		return serr.Wrap(err)
	}
	err = r.run(ctx, r.Tools.Dump, r.Opts.dumpArgs(r.Opts.Format, partial, conninfo, r.Opts.Jobs), stdout)
	if err != nil {
		_ = os.RemoveAll(partial)
		return err
	}
	if r.Opts.Format == Directory {
		// CheckOutDir let OUT be an existing empty directory; rename(2)
		// replaces one on Linux but not everywhere, so it goes first
		_ = os.Remove(r.Opts.Out)
	}
	if err = os.Rename(partial, r.Opts.Out); err != nil {
		_ = os.RemoveAll(partial)
		return serr.Wrap(err, "out", r.Opts.Out)
	}
	return nil
}

// run runs a tool with the connection's environment, its stdout to out.
// Its stderr goes to r.Stderr and is also kept, so a failure can be told
// apart (a version mismatch gets a hint).
func (r *Run) run(ctx context.Context, tool string, args []string, out io.Writer) error {
	cmd := exec.CommandContext(ctx, tool, args...)
	cmd.Env = r.env
	cmd.Stdout = out
	var errBuf bytes.Buffer
	cmd.Stderr = io.MultiWriter(r.errw, &errBuf)
	// An interrupt rather than exec's default SIGKILL, so pg_dump closes its
	// connections and pg_restore its files; killed if it lingers.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 10 * time.Second
	err := cmd.Run()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	name := filepath.Base(tool)
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if strings.Contains(errBuf.String(), "server version mismatch") {
			return serr.New(name + " is older than the server — point dbc at newer client tools " +
				"(--pg-bin, $DBC_PG_BIN, or pg_bin in the config)")
		}
		return serr.New(name+" failed (its messages are above)", "exit", strconv.Itoa(ee.ExitCode()))
	}
	return serr.Wrap(err, "tool", name)
}

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_./=:,@%+-]+$`)

// shellJoin renders argv as one line a POSIX shell would read back the
// same, for --dry-run.
func shellJoin(argv []string) string {
	out := make([]string, len(argv))
	for i, a := range argv {
		if shellSafe.MatchString(a) {
			out[i] = a
		} else {
			out[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
	}
	return strings.Join(out, " ")
}
