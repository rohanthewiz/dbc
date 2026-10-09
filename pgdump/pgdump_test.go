package pgdump

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/db"
)

func TestParseFormat(t *testing.T) {
	for in, want := range map[string]Format{"": Plain, "text": Plain, "SQL": Plain, "p": Plain, "c": Custom,
		"dir": Directory, "d": Directory, "tar": Tar, "split": Split} {
		if got, err := ParseFormat(in); err != nil || got != want {
			t.Errorf("%q: %v %v, want %v", in, got, err, want)
		}
	}
	if _, err := ParseFormat("zip"); err == nil {
		t.Error("zip parsed")
	}
}

func TestCheck(t *testing.T) {
	ok := []Options{
		{Format: Plain},
		{Format: Plain, Clean: true, IfExists: true, Create: true, NoOwner: true},
		{Format: Directory, Out: "d", Jobs: 4},
		{Format: Split, Out: "d", Jobs: 4, KeepArchive: true, Compress: "zstd:3"},
		{Format: Split, Out: "d", Clean: true, Create: true},
		{Format: Plain, Extra: []string{"--no-comments", "--lock-wait-timeout=10s", "-x"}},
	}
	for _, o := range ok {
		if err := o.Check(); err != nil {
			t.Errorf("%+v: %v", o, err)
		}
	}
	bad := []struct {
		o    Options
		want string
	}{
		{Options{Format: Plain, SchemaOnly: true, DataOnly: true}, "cannot both"},
		{Options{Format: Directory}, "name it with -o"},
		{Options{Format: Plain, Jobs: 2}, "--jobs needs"},
		{Options{Format: Custom, KeepArchive: true}, "--keep-archive is for -t split"},
		{Options{Format: Split, Out: "d", Compress: "6"}, "needs --keep-archive"},
		{Options{Format: Plain, IfExists: true}, "goes with --clean"},
		{Options{Format: Split, Out: "d", Clean: true}, "only with --create"},
		{Options{Format: Custom, NoOwner: true}, "--no-owner does nothing in a custom archive"},
		{Options{Format: Tar, Create: true}, "--create does nothing"},
		{Options{Format: Plain, Extra: []string{"--file=x"}}, "dbc sets --file"},
		{Options{Format: Plain, Extra: []string{"-Fc"}}, "dbc sets --format"},
		{Options{Format: Plain, Extra: []string{"-h", "db"}}, "dbc sets --host"},
		{Options{Format: Plain, Extra: []string{"--jobs", "3"}}, "dbc sets --jobs"},
	}
	for _, b := range bad {
		if err := b.o.Check(); err == nil || !strings.Contains(err.Error(), b.want) {
			t.Errorf("%+v: got %v, want %q", b.o, err, b.want)
		}
	}
}

func TestDumpArgs(t *testing.T) {
	o := Options{SchemaOnly: true, Schemas: []string{"sales"}, ExcludeTables: []string{"*_log"},
		Clean: true, IfExists: true, NoOwner: true, NoPrivileges: true, ColumnInserts: true,
		Compress: "zstd:3", Encoding: "UTF8", Extra: []string{"--no-comments"}}
	got := o.dumpArgs(Plain, "out.sql", conninfo, 1)
	want := []string{"--format=plain", "--file=out.sql", "--schema-only", "--schema=sales", "--exclude-table=*_log",
		"--clean", "--if-exists", "--no-owner", "--no-privileges", "--column-inserts", "--compress=zstd:3",
		"--encoding=UTF8", "--no-comments", "--no-password", "--dbname=service=dbc"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("plain\n got %q\nwant %q", got, want)
	}
	// an archive leaves the restore-time options to pg_restore
	got = o.dumpArgs(Directory, "d", conninfo, 3)
	for _, a := range got {
		if a == "--clean" || a == "--if-exists" || a == "--no-owner" {
			t.Fatalf("%s in an archive's args %q", a, got)
		}
	}
	if got[2] != "--jobs=3" {
		t.Fatalf("jobs: %q", got)
	}
}

func TestShellJoin(t *testing.T) {
	got := shellJoin([]string{"/usr/bin/pg_dump", "--table=sales.*", "--file=my dump.sql", "it's"})
	want := `/usr/bin/pg_dump '--table=sales.*' '--file=my dump.sql' 'it'\''s'`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

// fakeTool writes an executable shell script named name into dir.
func fakeTool(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func skipOnWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake tools are shell scripts")
	}
}

// Locate takes the first pg_dump new enough for the server, so an old one
// on PATH does not shadow a newer keg-only install.
func TestLocatePicksNewEnough(t *testing.T) {
	skipOnWindows(t)
	root := t.TempDir()
	onPath := filepath.Join(root, "path")
	keg := filepath.Join(root, "opt", "postgresql@17", "bin")
	fakeTool(t, onPath, "pg_dump", `echo "pg_dump (PostgreSQL) 14.11"`)
	fakeTool(t, keg, "pg_dump", `echo "pg_dump (PostgreSQL) 17.2 (Homebrew)"`)
	t.Setenv("PATH", onPath)
	old := probeDirs
	probeDirs = []string{filepath.Join(root, "opt", "postgresql@*", "bin")}
	defer func() { probeDirs = old }()

	ctx := context.Background()
	tools, err := Locate(ctx, "", 16, false)
	if err != nil {
		t.Fatal(err)
	}
	if tools.Dump != filepath.Join(keg, "pg_dump") || tools.Version != "17.2" || tools.Major != 17 {
		t.Fatalf("picked %+v", tools)
	}
	// an older server: PATH's comes first and is new enough
	if tools, _ = Locate(ctx, "", 13, false); tools.Major != 14 {
		t.Fatalf("picked %+v", tools)
	}
	// none new enough: the error lists what there is
	if _, err = Locate(ctx, "", 18, false); err == nil || !strings.Contains(err.Error(), "14.11") ||
		!strings.Contains(err.Error(), "17.2") {
		t.Fatalf("too old: %v", err)
	}
	// split needs pg_restore beside pg_dump
	if _, err = Locate(ctx, "", 0, true); err == nil || !strings.Contains(err.Error(), "pg_dump and pg_restore") {
		t.Fatalf("no pg_restore: %v", err)
	}
	// --pg-bin is the only place looked
	if _, err = Locate(ctx, filepath.Join(root, "nowhere"), 0, false); err == nil || !strings.Contains(err.Error(), "nowhere") {
		t.Fatalf("pg-bin: %v", err)
	}
}

const sampleTOC = `;
; Archive created at 2026-10-09 12:00:00 UTC
;
3480; 2615 16385 SCHEMA - sales postgres
218; 1259 16390 TABLE public cats postgres
3486; 0 16390 TABLE DATA public cats postgres
3488; 0 16420 TABLE DATA public Cats postgres
3489; 0 16430 TABLE DATA public my table postgres
3490; 0 16440 TABLE DATA sales orders app
3495; 0 0 SEQUENCE SET public cats_id_seq postgres
3500; 2606 16399 CONSTRAINT public cats cats_pkey postgres
3510; 0 16450 MATERIALIZED VIEW DATA public cat_counts postgres
`

func TestParseTOCAndPlanSplit(t *testing.T) {
	toc := parseTOC(sampleTOC)
	if len(toc) != 9 {
		t.Fatalf("entries %d", len(toc))
	}
	if e := toc[4]; e.desc != "TABLE DATA" || e.schema != "public" || e.name != "my table" {
		t.Fatalf("%+v", e)
	}
	parts := planSplit(toc, Options{NoOwner: true, Create: true})
	var files []string
	for _, p := range parts {
		files = append(files, p.file)
	}
	want := []string{"1-pre-data.sql", "2-data/public.cats.sql", "2-data/public.Cats-3488.sql",
		"2-data/public.my_table.sql", "2-data/sales.orders.sql", "2-data-other.sql", "3-post-data.sql"}
	if !reflect.DeepEqual(files, want) {
		t.Fatalf("files\n got %q\nwant %q", files, want)
	}
	if !reflect.DeepEqual(parts[0].args, []string{"--section=pre-data", "--no-owner", "--create"}) {
		t.Fatalf("pre-data args %q", parts[0].args)
	}
	if !reflect.DeepEqual(parts[1].list, []string{"3486; 0 16390 TABLE DATA public cats postgres"}) {
		t.Fatalf("data list %q", parts[1].list)
	}
	if !reflect.DeepEqual(parts[5].list, []string{"3495; 0 0 SEQUENCE SET public cats_id_seq postgres"}) {
		t.Fatalf("other list %q", parts[5].list)
	}

	// --data-only: no pre- or post-data file; --schema-only has no data
	// entries to begin with
	parts = planSplit(toc, Options{DataOnly: true})
	if parts[0].file != "2-data/public.cats.sql" || parts[len(parts)-1].file != "2-data-other.sql" {
		t.Fatalf("data-only %+v", parts)
	}
}

func TestFileSafe(t *testing.T) {
	if got := fileSafe(`a/b.c d"é`); got != "a_b_c_d_é" {
		t.Fatalf("got %q", got)
	}
}

// fakeDumpTools installs a pg_dump that makes a directory archive (and
// records its arguments, the service file and PGPASSWORD in it), and a
// pg_restore that lists sampleTOC and writes its own arguments as the SQL.
// restoreFails makes pg_restore fail for any --file.
func fakeDumpTools(t *testing.T, restoreFails bool) Tools {
	dir := filepath.Join(t.TempDir(), "bin")
	fakeTool(t, dir, "pg_dump", `
for a in "$@"; do case "$a" in --file=*) out="${a#--file=}";; esac; done
mkdir -p "$out"
echo "$@" > "$out/args"
cp "$PGSERVICEFILE" "$out/service"
printf %s "$PGPASSWORD" > "$out/password"
echo toc > "$out/toc.dat"`)
	fail := ""
	if restoreFails {
		fail = `echo "pg_restore: error: boom" >&2; exit 1`
	}
	tocFile := filepath.Join(dir, "toc.txt")
	if err := os.WriteFile(tocFile, []byte(sampleTOC), 0o644); err != nil {
		t.Fatal(err)
	}
	fakeTool(t, dir, "pg_restore", `
if [ "$1" = "--list" ]; then cat "`+tocFile+`"; exit 0; fi
`+fail+`
for a in "$@"; do case "$a" in --file=*) out="${a#--file=}";; --use-list=*) list="${a#--use-list=}";; esac; done
{ echo "-- $*"; if [ -n "$list" ]; then cat "$list"; fi; } > "$out"`)
	return Tools{Dump: filepath.Join(dir, "pg_dump"), Restore: filepath.Join(dir, "pg_restore"), Version: "17.2", Major: 17}
}

func TestSplitRun(t *testing.T) {
	skipOnWindows(t)
	out := filepath.Join(t.TempDir(), "dump")
	var stderr strings.Builder
	r := &Run{Tools: fakeDumpTools(t, false), Stderr: &stderr,
		Conn: db.LibpqConn{Settings: [][2]string{{"host", "db"}, {"dbname", "app"}}, Password: "it's secret "},
		Opts: Options{Format: Split, Out: out, Jobs: 3, NoOwner: true, KeepArchive: true}}
	if err := r.Do(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, stderr.String())
	}
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(out, rel))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	// the connection reached pg_dump as a service file and PGPASSWORD,
	// and the command line holds neither
	if s := read("archive/service"); !strings.Contains(s, "[dbc]\nhost=db\ndbname=app\n") {
		t.Fatalf("service file:\n%s", s)
	}
	if p := read("archive/password"); p != "it's secret " {
		t.Fatalf("password %q", p)
	}
	if a := read("archive/args"); !strings.Contains(a, "--format=directory") || !strings.Contains(a, "--jobs=3") ||
		!strings.Contains(a, "--dbname=service=dbc") || strings.Contains(a, "secret") || strings.Contains(a, "--no-owner") {
		t.Fatalf("pg_dump args: %s", a)
	}
	if s := read("2-data/sales.orders.sql"); !strings.Contains(s, "--no-owner") ||
		!strings.Contains(s, "3490; 0 16440 TABLE DATA sales orders app") {
		t.Fatalf("orders:\n%s", s)
	}
	if s := read("1-pre-data.sql"); !strings.Contains(s, "--section=pre-data") {
		t.Fatalf("pre-data:\n%s", s)
	}
	rs := read("restore.sql")
	for _, want := range []string{`\set ON_ERROR_STOP on`, "\\ir 1-pre-data.sql\n", "\\ir 2-data/public.my_table.sql\n",
		"\\ir 2-data-other.sql\n", "\\ir 3-post-data.sql\n"} {
		if !strings.Contains(rs, want) {
			t.Fatalf("restore.sql lacks %q:\n%s", want, rs)
		}
	}
	if strings.Index(rs, "1-pre-data") > strings.Index(rs, "2-data/") || strings.Index(rs, "2-data/") > strings.Index(rs, "3-post") {
		t.Fatalf("restore.sql out of order:\n%s", rs)
	}
	// the list files are gone; the archive stays, as asked
	ents, _ := os.ReadDir(out)
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	want := []string{"1-pre-data.sql", "2-data", "2-data-other.sql", "3-post-data.sql", "archive", "restore.sql"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("out holds %q", names)
	}

	// without --keep-archive the archive goes too
	out2 := filepath.Join(t.TempDir(), "d2")
	r.Opts.Out, r.Opts.KeepArchive = out2, false
	if err := r.Do(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out2, "archive")); !os.IsNotExist(err) {
		t.Fatalf("archive kept: %v", err)
	}
}

// A failed split leaves nothing that could pass for a dump: a directory it
// made is removed, one that was empty is emptied again.
func TestSplitFailureCleansUp(t *testing.T) {
	skipOnWindows(t)
	tools := fakeDumpTools(t, true)
	var stderr strings.Builder
	made := filepath.Join(t.TempDir(), "new")
	r := &Run{Tools: tools, Stderr: &stderr, Opts: Options{Format: Split, Out: made}}
	err := r.Do(context.Background())
	if err == nil || !strings.Contains(err.Error(), "pg_restore failed") || !strings.Contains(stderr.String(), "boom") {
		t.Fatalf("err %v, stderr %q", err, stderr.String())
	}
	if _, err = os.Stat(made); !os.IsNotExist(err) {
		t.Fatalf("made dir left behind: %v", err)
	}

	empty := t.TempDir()
	r.Opts.Out = empty
	if err = r.Do(context.Background()); err == nil {
		t.Fatal("no error")
	}
	if ents, _ := os.ReadDir(empty); len(ents) != 0 {
		t.Fatalf("left %v in the empty dir", ents)
	}
	// and a directory with something in it is refused before any work
	if err = os.WriteFile(filepath.Join(empty, "keep.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err = r.Do(context.Background()); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("non-empty: %v", err)
	}
	if _, err = os.Stat(filepath.Join(empty, "keep.txt")); err != nil {
		t.Fatal("the existing file was touched")
	}
}

// A plain dump to stdout streams pg_dump's stdout through, and a failure
// that is a version mismatch says what to do about it.
func TestRunPlainAndMismatchHint(t *testing.T) {
	skipOnWindows(t)
	dir := t.TempDir()
	fakeTool(t, dir, "pg_dump", `echo "-- dump of $PGSERVICEFILE"`)
	var stdout, stderr strings.Builder
	r := &Run{Tools: Tools{Dump: filepath.Join(dir, "pg_dump")}, Stdout: &stdout, Stderr: &stderr, Opts: Options{Format: Plain}}
	if err := r.Do(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stdout.String(), "-- dump of ") {
		t.Fatalf("stdout %q", stdout.String())
	}
	// the service file is removed when the run ends
	svc := strings.TrimSpace(strings.TrimPrefix(stdout.String(), "-- dump of "))
	if _, err := os.Stat(svc); !os.IsNotExist(err) {
		t.Fatalf("service file %s left: %v", svc, err)
	}

	fakeTool(t, dir, "pg_dump", `echo "pg_dump: error: aborting because of server version mismatch" >&2; exit 1`)
	if err := r.Do(context.Background()); err == nil || !strings.Contains(err.Error(), "--pg-bin") {
		t.Fatalf("mismatch: %v", err)
	}
}
