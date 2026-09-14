// Package migrate applies versioned SQL migrations, in the file format and
// with the version table that goose uses — so a project that has been
// migrating with the goose binary can switch to `dbc migrate` without
// rewriting a single file or touching its goose_db_version table.
//
// A migration file is NNN_name.sql with two annotated sections:
//
//	-- +goose Up
//	CREATE TABLE users (...);
//
//	-- +goose Down
//	DROP TABLE users;
//
// Statements are split on semicolons by the same scanner the editor uses, so
// a $$-quoted function body is one statement without help. For anything the
// scanner cannot know is one statement, the goose escape hatch still works:
//
//	-- +goose StatementBegin
//	... verbatim, semicolons and all ...
//	-- +goose StatementEnd
//
// `-- +goose NO TRANSACTION` at the top runs the file's statements outside a
// transaction, for DDL that refuses to run inside one (CREATE INDEX
// CONCURRENTLY, say).
package migrate

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/sqlsplit"
)

// Migration is one parsed migration file.
type Migration struct {
	Version int64  // leading number of the file name; the ordering key
	Name    string // file name, e.g. 20170419004813_CreateUsersTable.sql
	Path    string // where it was read from
	Up      []string
	Down    []string
	NoTx    bool // run outside a transaction (-- +goose NO TRANSACTION)
}

// fileName matches NNN_anything.sql. Goose also accepts NNN.sql with no
// underscore-name; we do too, since the name part is optional here.
var fileName = regexp.MustCompile(`^(\d+)(?:_(.*))?\.sql$`)

// Load reads every migration in dir, sorted by version. Files that do not
// look like migrations and subdirectories are ignored, so a stash of ad-hoc
// SQL beside the migrations does not break the run. Two files sharing a
// version is an error: with one applied and the other not, status would be a
// lie.
func Load(dir string) ([]Migration, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, serr.Wrap(err, "migrations_dir", dir)
	}
	var out []Migration
	seen := map[int64]string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := fileName.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		v, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return nil, serr.Wrap(err, "file", e.Name(), "msg", "version does not fit in int64")
		}
		if other, dup := seen[v]; dup {
			return nil, serr.New("two migrations share a version", "version", m[1],
				"files", other+", "+e.Name())
		}
		seen[v] = e.Name()

		mig, err := ParseFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		mig.Version = v
		out = append(out, mig)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// ParseFile parses one migration file. Version is left to the caller, which
// has the file name.
func ParseFile(path string) (Migration, error) {
	f, err := os.Open(path)
	if err != nil {
		return Migration{}, serr.Wrap(err, "file", path)
	}
	defer f.Close()

	m, err := parse(bufio.NewReader(f))
	if err != nil {
		return Migration{}, serr.Wrap(err, "file", path)
	}
	m.Name = filepath.Base(path)
	m.Path = path
	return m, nil
}

// part is a run of lines from one section. A verbatim part (between
// StatementBegin/End) is exactly one statement; a free part is split on
// semicolons.
type part struct {
	text     strings.Builder
	verbatim bool
}

// parse walks the file line by line looking for goose annotations, and hands
// everything between them to the right section. The annotation grammar is
// tiny, so a hand-rolled state machine is clearer than a real lexer:
//
//	none ──Up──▶ up ──Down──▶ down
//	         │              │
//	  StatementBegin/End toggles verbatim inside either section
func parse(r *bufio.Reader) (Migration, error) {
	var m Migration
	section := "" // "", "up", "down"
	var parts []*part
	cur := &part{}
	verbatim := false
	sawSQL := false // SQL before the first annotation is a mistake

	flush := func() {
		if cur.text.Len() > 0 || cur.verbatim {
			parts = append(parts, cur)
		}
		cur = &part{}
	}
	// endSection turns the parts collected so far into the section's statements
	endSection := func() error {
		if verbatim {
			return serr.New("missing -- +goose StatementEnd")
		}
		flush()
		var stmts []string
		for _, p := range parts {
			if p.verbatim {
				if s := strings.TrimSpace(p.text.String()); s != "" {
					stmts = append(stmts, s)
				}
				continue
			}
			for _, st := range sqlsplit.Split(p.text.String()) {
				stmts = append(stmts, st.Text)
			}
		}
		switch section {
		case "up":
			m.Up = stmts
		case "down":
			m.Down = stmts
		}
		parts = nil
		return nil
	}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024) // a data migration can be big
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		cmd, isAnn := annotation(line)
		if !isAnn {
			if section == "" && strings.TrimSpace(line) != "" && !strings.HasPrefix(strings.TrimSpace(line), "--") {
				sawSQL = true
			}
			if section != "" {
				cur.text.WriteString(line)
				cur.text.WriteByte('\n')
			}
			continue
		}
		switch strings.ToLower(cmd) {
		case "up":
			if section != "" {
				return m, serr.New("duplicate or misplaced -- +goose Up", "line", strconv.Itoa(lineNo))
			}
			if sawSQL {
				return m, serr.New("SQL before -- +goose Up", "line", strconv.Itoa(lineNo))
			}
			section = "up"
		case "down":
			if section != "up" {
				return m, serr.New("-- +goose Down must follow the Up section", "line", strconv.Itoa(lineNo))
			}
			if err := endSection(); err != nil {
				return m, serr.Wrap(err, "line", strconv.Itoa(lineNo))
			}
			section = "down"
		case "statementbegin":
			if section == "" {
				return m, serr.New("StatementBegin outside a section", "line", strconv.Itoa(lineNo))
			}
			if verbatim {
				return m, serr.New("nested -- +goose StatementBegin", "line", strconv.Itoa(lineNo))
			}
			flush()
			verbatim = true
			cur.verbatim = true
		case "statementend":
			if !verbatim {
				return m, serr.New("-- +goose StatementEnd without StatementBegin", "line", strconv.Itoa(lineNo))
			}
			flush()
			verbatim = false
		case "no transaction":
			m.NoTx = true
		default:
			// ENVSUB and any future goose directive: refusing is safer than
			// silently running the file with different semantics
			return m, serr.New("unsupported goose annotation", "annotation", cmd, "line", strconv.Itoa(lineNo))
		}
	}
	if err := scanner.Err(); err != nil {
		return m, serr.Wrap(err, "op", "read")
	}
	if section == "" {
		return m, serr.New("no -- +goose Up annotation found")
	}
	if err := endSection(); err != nil {
		return m, err
	}
	return m, nil
}

// annotation recognizes `-- +goose <command>` and returns the command. The
// spacing is forgiving (`--+goose Up` and `--  +goose  Up` both count) since
// hand-edited files drift.
func annotation(line string) (string, bool) {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, "--") {
		return "", false
	}
	s = strings.TrimSpace(strings.TrimPrefix(s, "--"))
	if !strings.HasPrefix(s, "+goose") {
		return "", false
	}
	s = strings.TrimSpace(strings.TrimPrefix(s, "+goose"))
	// collapse internal runs of whitespace so "NO  TRANSACTION" still matches
	return strings.Join(strings.Fields(s), " "), true
}

// Create writes a new empty migration file in dir, named with the current
// UTC time as goose does (YYYYMMDDHHMMSS), and returns its path. The name is
// sanitized to something file-system and shell friendly.
func Create(dir, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", serr.New("migration name required")
	}
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		}
		return '_'
	}, name)
	fn := fmt.Sprintf("%s_%s.sql", time.Now().UTC().Format("20060102150405"), safe)
	path := filepath.Join(dir, fn)
	if _, err := os.Stat(path); err == nil {
		return "", serr.New("migration already exists", "path", path)
	}
	body := "-- +goose Up\n-- SQL in this section is executed when the migration is applied\n\n\n" +
		"-- +goose Down\n-- SQL in this section is executed when the migration is rolled back\n\n"
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		return "", serr.Wrap(err, "path", path)
	}
	return path, nil
}
