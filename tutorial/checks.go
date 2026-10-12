package tutorial

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/rohanthewiz/serr"
)

// The check helpers. Each returns a Check.Test: nil when the state is
// right, else an error that says what was found ("3 rows, want 4"). They
// read state through dbc itself (`-t json`), as the learner would.

// dbc runs dbc in the workspace and captures its output.
func (e *Env) dbc(args ...string) (stdout, stderr string, code int, err error) {
	var out, errb bytes.Buffer
	code, err = e.Dbc(e.Ctx, e.Dir, args, &out, &errb)
	return out.String(), errb.String(), code, err
}

// query runs one SELECT on conn and decodes its rows. A single statement's
// `-t json` is an array of row objects; numbers stay json.Number so a
// count compares exactly.
func (e *Env) query(conn, sql string) ([]map[string]any, error) {
	out, errs, code, err := e.dbc("-c", conn, "-t", "json", sql)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("%s", lastLine(errs, "the query failed"))
	}
	dec := json.NewDecoder(strings.NewReader(out))
	dec.UseNumber()
	var rows []map[string]any
	if err := dec.Decode(&rows); err != nil {
		return nil, serr.Wrap(err, "op", "decode query result", "conn", conn)
	}
	return rows, nil
}

// count is SELECT count(*) on conn's table.
func (e *Env) count(conn, table string) (int64, error) {
	rows, err := e.query(conn, "SELECT count(*) AS n FROM "+table)
	if err != nil {
		return 0, fmt.Errorf("%s.%s: %w", conn, table, err)
	}
	if len(rows) != 1 {
		return 0, fmt.Errorf("count of %s gave %d rows", table, len(rows))
	}
	n, ok := rows[0]["n"].(json.Number)
	if !ok {
		return 0, fmt.Errorf("count of %s is not a number: %v", table, rows[0]["n"])
	}
	return n.Int64()
}

// rowsEqual passes when conn's table has exactly want rows.
func rowsEqual(conn, table string, want int64) func(*Env) error {
	return func(e *Env) error {
		n, err := e.count(conn, table)
		if err != nil {
			return err
		}
		if n != want {
			return fmt.Errorf("%s.%s has %d rows, want %d", conn, table, n, want)
		}
		return nil
	}
}

// rowsAtLeast passes when conn's table has min rows or more.
func rowsAtLeast(conn, table string, min int64) func(*Env) error {
	return func(e *Env) error {
		n, err := e.count(conn, table)
		if err != nil {
			return err
		}
		if n < min {
			return fmt.Errorf("%s.%s has %d rows, want at least %d", conn, table, n, min)
		}
		return nil
	}
}

// clean passes when `dbc args…` exits 0. Used for the --check verbs,
// which are silent when clean and exit 1 on an error.
func clean(args ...string) func(*Env) error {
	return func(e *Env) error {
		out, errs, code, err := e.dbc(args...)
		if err != nil {
			return err
		}
		if code != 0 {
			// the checks print their findings on stdout (path:line:col: msg)
			// or stderr (a log line); either names the problem
			return fmt.Errorf("`%s` exited %d: %s", CommandLine(args), code,
				lastLine(out+errs, "no output"))
		}
		return nil
	}
}

// filesExist passes when every path (relative to the workspace) is there.
func filesExist(paths ...string) func(*Env) error {
	return func(e *Env) error {
		var missing []string
		for _, p := range paths {
			if _, err := os.Stat(filepath.Join(e.Dir, filepath.FromSlash(p))); err != nil {
				missing = append(missing, p)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("missing %s", strings.Join(missing, ", "))
		}
		return nil
	}
}

// fileMatches passes when at least one file matches the glob pattern
// (relative to the workspace). For dated names: exports/big_orders_*.csv.
func fileMatches(pattern string) func(*Env) error {
	return func(e *Env) error {
		m, err := filepath.Glob(filepath.Join(e.Dir, filepath.FromSlash(pattern)))
		if err != nil {
			return serr.Wrap(err, "op", "glob", "pattern", pattern)
		}
		if len(m) == 0 {
			return fmt.Errorf("no file matches %s", pattern)
		}
		return nil
	}
}

// scriptsClean passes when every .go file in the workspace's scripts/
// passes `dbc script --check`. Globbed here, since no shell expands it.
func scriptsClean(e *Env) error {
	m, err := filepath.Glob(filepath.Join(e.Dir, "scripts", "*.go"))
	if err != nil {
		return serr.Wrap(err, "op", "glob scripts")
	}
	if len(m) == 0 {
		return fmt.Errorf("no scripts in scripts/")
	}
	args := []string{"script", "--check"}
	for _, p := range m {
		args = append(args, filepath.ToSlash(filepath.Join("scripts", filepath.Base(p))))
	}
	return clean(args...)(e)
}

// runs decodes `dbc runs -t json [filters…]`: the run records, newest
// first, as {id, kind, name, trigger, status, …} objects.
func (e *Env) runs(filters ...string) ([]map[string]any, error) {
	out, errs, code, err := e.dbc(append([]string{"runs", "-t", "json"}, filters...)...)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("%s", lastLine(errs, "dbc runs failed"))
	}
	var recs []map[string]any
	if err := json.Unmarshal([]byte(out), &recs); err != nil {
		return nil, serr.Wrap(err, "op", "decode run records")
	}
	return recs, nil
}

// runRecord passes when a succeeded record of the job exists, and when
// trigger is set, one with that trigger ("cli", "script").
func runRecord(job, trigger string) func(*Env) error {
	return func(e *Env) error {
		recs, err := e.runs("--job", job, "--status", "succeeded")
		if err != nil {
			return err
		}
		for _, r := range recs {
			if trigger == "" || r["trigger"] == trigger {
				return nil
			}
		}
		if trigger != "" {
			return fmt.Errorf("no succeeded run of job %s with trigger %s", job, trigger)
		}
		return fmt.Errorf("no succeeded run of job %s", job)
	}
}

// all runs tests in order and returns the first failure.
func all(tests ...func(*Env) error) func(*Env) error {
	return func(e *Env) error {
		for _, t := range tests {
			if err := t(e); err != nil {
				return err
			}
		}
		return nil
	}
}

// lastLine is the last non-empty line of s, or def. A dbc failure's last
// line on stderr is the one that names it. A structured log line
// (time=… level=error msg="…" … error="…" plus a stack of fields) is cut
// down to its msg and error, which are what a learner needs.
func lastLine(s, def string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" {
			continue
		}
		if m := logField.FindAllStringSubmatch(l, -1); m != nil {
			var msg, errText string
			for _, f := range m {
				switch f[1] {
				case "msg":
					msg = f[2]
				case "error":
					errText = f[2]
				}
			}
			if msg != "" && errText != "" && errText != msg {
				return msg + ": " + errText
			}
			if msg != "" {
				return msg
			}
		}
		return l
	}
	return def
}

// logField matches a logrus key="value" pair, \" escapes included. Only
// msg and error are read; the key is anchored at a space or the line's
// start so fields.msg is not taken for msg.
var logField = regexp.MustCompile(`(?:^| )(msg|error)="((?:[^"\\]|\\.)*)"`)
