package pgdump

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/rohanthewiz/serr"
	"golang.org/x/sync/errgroup"
)

// The split format: plain SQL in several files, one per table's data, which
// psql restores and a person can read, grep or diff table by table.
//
//	OUT/
//	  restore.sql           \ir's the rest in order: psql -d NEWDB -f OUT/restore.sql
//	  1-pre-data.sql        schemas, types, tables, functions, sequences (no data)
//	  2-data/
//	    public.cats.sql     one table's rows (COPY, or INSERTs with --inserts)
//	    public.orders.sql
//	  2-data-other.sql      sequence positions, large objects (when there are any)
//	  3-post-data.sql       indexes, constraints, foreign keys, triggers, matview data
//	  archive/              with --keep-archive: what it was made from, for pg_restore
//
// HOW. pg_dump has no such format, and dumping each table with its own
// pg_dump would lose the dump's single snapshot (tables written seconds
// apart, under live writes, need not agree with each other). So pg_dump
// writes ONE directory-format archive — consistent, filtered, parallel
// with --jobs — and pg_restore, which can write any part of an archive as
// SQL without a server, writes each file from it:
//
//	pg_dump --format=directory ─► OUT/archive ─► pg_restore --list ─► TOC
//	                                    │
//	   ┌────────────────────────────────┼──────────────────────────────┐
//	   ▼                                ▼                              ▼
//	pg_restore --section=pre-data   pg_restore --use-list=<one       pg_restore --section=post-data
//	  ─► 1-pre-data.sql               TABLE DATA entry> per table      ─► 3-post-data.sql
//	                                  ─► 2-data/<schema>.<table>.sql
//
// The archive is then removed, unless --keep-archive asked for it.
//
// The three sections are pg_dump's own split of a restore (see pg_dump
// --section): everything a row needs before it can be loaded, the rows,
// and what is faster to build after the rows are in. Restoring them in
// that order is what pg_restore itself does.

// The parts' names. Numbered so a directory listing shows them in restore
// order.
const (
	preDataFile   = "1-pre-data.sql"
	dataDir       = "2-data"
	dataOtherFile = "2-data-other.sql"
	postDataFile  = "3-post-data.sql"
	restoreFile   = "restore.sql"
	archiveName   = "archive"
)

// archiveDir is where split's pg_dump writes its archive.
func (r *Run) archiveDir() string { return filepath.Join(r.Opts.Out, archiveName) }

// splitDumpArgs are pg_dump's arguments for split's archive.
func (r *Run) splitDumpArgs(archive string) []string {
	return r.Opts.dumpArgs(Directory, archive, conninfo, r.Opts.Jobs)
}

// tocEntry is one line of `pg_restore --list`:
//
//	3345; 0 16390 TABLE DATA public cats postgres
//	└id┘  └catalog┘ └desc────┘ └schema┘└name┘└owner┘
type tocEntry struct {
	id           int
	line         string // as listed: a --use-list file is made of these
	desc         string // "TABLE DATA", "SEQUENCE SET", …; "" for any other
	schema, name string // for TABLE DATA
}

var tocLineRE = regexp.MustCompile(`^(\d+);\s+\d+\s+\d+\s+(.*)$`)

// dataDescs are the TOC entry kinds split writes into files of their own,
// all from pg_dump's data section. Large objects' data is "BLOBS" (and
// "LARGE OBJECTS" is matched too, should a later pg_dump rename it).
var dataDescs = []string{"TABLE DATA", "SEQUENCE SET", "BLOBS", "LARGE OBJECTS"}

// parseTOC reads `pg_restore --list` output. Lines starting with ';' are
// comments. Only the entry kinds in dataDescs are told apart; the rest are
// written by section and need no reading.
//
// The schema and table name come from splitting on blanks, which a name
// with a blank in it defeats — they only name a file, though: pg_restore
// finds the entry by its id, from the line as listed.
func parseTOC(toc string) []tocEntry {
	var out []tocEntry
	for line := range strings.SplitSeq(toc, "\n") {
		line = strings.TrimRight(line, "\r")
		m := tocLineRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		e := tocEntry{line: line}
		e.id, _ = strconv.Atoi(m[1])
		rest := m[2]
		for _, d := range dataDescs {
			if rest == d || strings.HasPrefix(rest, d+" ") {
				e.desc = d
				rest = strings.TrimPrefix(rest, d)
				break
			}
		}
		if e.desc == "TABLE DATA" {
			f := strings.Fields(rest) // schema, name…, owner
			switch {
			case len(f) >= 3:
				e.schema, e.name = f[0], strings.Join(f[1:len(f)-1], " ")
			case len(f) == 2: // no owner listed
				e.schema, e.name = f[0], f[1]
			}
		}
		out = append(out, e)
	}
	return out
}

// part is one file split writes, and how pg_restore makes it.
type part struct {
	file  string   // relative to OUT
	args  []string // pg_restore's options, less --file and the archive
	list  []string // TOC lines for a --use-list file; nil = no list
	about string   // what it holds, for restore.sql's comment
}

// planSplit decides the files to write from the archive's TOC, in restore
// order. A section the options left empty gets no file: --data-only has no
// pre- or post-data worth a file, --schema-only no data.
func planSplit(toc []tocEntry, o Options) []part {
	var base []string
	if o.NoOwner {
		base = append(base, "--no-owner")
	}
	var parts []part
	if !o.DataOnly {
		a := append([]string{"--section=pre-data"}, base...)
		// --create and --clean belong to the first file alone: it makes (or
		// drops and remakes) the database and \connects to it, and the
		// files after it run in that session, so in the new database
		if o.Create {
			a = append(a, "--create")
		}
		if o.Clean {
			a = append(a, "--clean")
		}
		if o.IfExists {
			a = append(a, "--if-exists")
		}
		parts = append(parts, part{file: preDataFile, args: a,
			about: "schemas, types, tables, functions, sequences"})
	}

	used := map[string]bool{}
	var other []string
	for _, e := range toc {
		switch e.desc {
		case "TABLE DATA":
			p := part{file: dataDir + "/" + dataFileName(e, used), args: base, list: []string{e.line}}
			if len(used) == 1 {
				p.about = "table data, one file per table"
			}
			parts = append(parts, p)
		case "SEQUENCE SET", "BLOBS", "LARGE OBJECTS":
			other = append(other, e.line)
		}
	}
	if len(other) > 0 {
		// --section=data as a guard: whatever the list holds, nothing but
		// data can reach this file
		parts = append(parts, part{file: dataOtherFile, args: append([]string{"--section=data"}, base...),
			list: other, about: "sequence positions, large objects"})
	}
	if !o.DataOnly {
		parts = append(parts, part{file: postDataFile, args: append([]string{"--section=post-data"}, base...),
			about: "indexes, constraints, foreign keys, triggers"})
	}
	return parts
}

// dataFileName names a table's data file "<schema>.<table>.sql", with what
// a file name should not hold replaced by '_'. Two tables whose names only
// differ in case, or in replaced characters, would share a name — and on
// macOS's case-insensitive disks, a file — so the second gets its TOC id
// appended. used is shared across one dump's calls.
func dataFileName(e tocEntry, used map[string]bool) string {
	stem := "table-" + strconv.Itoa(e.id)
	if e.name != "" {
		stem = fileSafe(e.schema) + "." + fileSafe(e.name)
	}
	if len(stem) > 200 { // under any file system's 255
		stem = stem[:200]
	}
	name := stem + ".sql"
	if used[strings.ToLower(name)] {
		name = stem + "-" + strconv.Itoa(e.id) + ".sql"
	}
	used[strings.ToLower(name)] = true
	return name
}

// fileSafe keeps letters, digits, '_' and '-' (Unicode letters too: a table
// named in Greek keeps its name) and replaces the rest with '_'. '.' is
// replaced as well, since it separates schema from table in the name.
func fileSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' {
			return r
		}
		return '_'
	}, s)
}

// split runs the split format; see the top of this file.
func (r *Run) split(ctx context.Context) (err error) {
	out := r.Opts.Out
	created, err := prepareOutDir(out)
	if err != nil {
		return err
	}
	defer func() {
		// A failed or canceled dump leaves nothing behind that could pass
		// for a whole one. Only what this run wrote is removed: OUT was
		// either made by it or empty before it.
		if err != nil {
			if created {
				_ = os.RemoveAll(out)
			} else {
				removeChildren(out)
			}
		}
	}()

	archive := r.archiveDir()
	r.say("pg_dump %s: dumping into a directory archive", r.Tools.Version)
	if err = r.run(ctx, r.Tools.Dump, r.splitDumpArgs(archive), io.Discard); err != nil {
		return err
	}
	var toc bytes.Buffer
	if err = r.run(ctx, r.Tools.Restore, []string{"--list", archive}, &toc); err != nil {
		return err
	}
	parts := planSplit(parseTOC(toc.String()), r.Opts)
	tables := 0
	for _, p := range parts {
		if strings.HasPrefix(p.file, dataDir+"/") {
			tables++
		}
	}
	if tables > 0 {
		if err = os.Mkdir(filepath.Join(out, dataDir), 0o755); err != nil {
			return serr.Wrap(err)
		}
	}
	r.say("pg_restore: writing %d file(s), %d of them table data", len(parts), tables)

	// The list files go beside the archive, which is removed after (or
	// kept, and then they are harmless). Each conversion is its own
	// pg_restore reading the archive, so they run --jobs at a time.
	listDir, err := os.MkdirTemp(out, ".lists-")
	if err != nil {
		return serr.Wrap(err)
	}
	defer os.RemoveAll(listDir)

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(max(r.Opts.Jobs, 1))
	for i, p := range parts {
		args := append([]string{}, p.args...)
		if p.list != nil {
			lf := filepath.Join(listDir, strconv.Itoa(i)+".list")
			if err = os.WriteFile(lf, []byte(strings.Join(p.list, "\n")+"\n"), 0o600); err != nil {
				return serr.Wrap(err)
			}
			args = append(args, "--use-list="+lf)
		}
		args = append(args, "--file="+filepath.Join(out, filepath.FromSlash(p.file)), archive)
		g.Go(func() error { return r.run(gctx, r.Tools.Restore, args, io.Discard) })
	}
	if err = g.Wait(); err != nil {
		return err
	}

	if err = os.WriteFile(filepath.Join(out, restoreFile), restoreScript(parts), 0o644); err != nil {
		return serr.Wrap(err)
	}
	if !r.Opts.KeepArchive {
		if err = os.RemoveAll(archive); err != nil {
			return serr.Wrap(err)
		}
	}
	return nil
}

// restoreScript is restore.sql: each part \ir'd (included relative to
// restore.sql itself, so psql may be run from anywhere), in order.
func restoreScript(parts []part) []byte {
	var b strings.Builder
	b.WriteString(`-- Restores this dump, written by dbc dump in the split format. Run it with
--   psql -X -d TARGET -f restore.sql
-- from any directory (add --single-transaction for all-or-nothing). Each file
-- below is a self-contained part of the dump; they run in this order.
\set ON_ERROR_STOP on
`)
	for _, p := range parts {
		if p.about != "" {
			fmt.Fprintf(&b, "\n-- %s\n", p.about)
		}
		fmt.Fprintf(&b, "\\ir %s\n", p.file)
	}
	return []byte(b.String())
}

// CheckOutDir reports whether a directory-writing format (directory,
// split) may write into dir: it must not exist yet or be empty, as pg_dump
// asks of a directory-format target — files already there would mix two
// dumps. The CLI calls it before connecting; split calls it again (via
// prepareOutDir) when it starts.
func CheckOutDir(dir string) error {
	ents, err := os.ReadDir(dir)
	switch {
	case os.IsNotExist(err):
		return nil
	case err != nil:
		return serr.Wrap(err, "dir", dir) // a file, or unreadable
	case len(ents) > 0:
		return serr.New("the output directory is not empty — name a new one", "dir", dir)
	}
	return nil
}

// prepareOutDir makes dir ready for split (see CheckOutDir), creating it
// when it does not exist; created says it did.
func prepareOutDir(dir string) (created bool, err error) {
	if err = CheckOutDir(dir); err != nil {
		return false, err
	}
	if _, statErr := os.Stat(dir); statErr == nil {
		return false, nil
	}
	if err = os.MkdirAll(dir, 0o755); err != nil {
		return false, serr.Wrap(err)
	}
	return true, nil
}

// removeChildren empties dir, which held nothing before this run.
func removeChildren(dir string) {
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		_ = os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}
