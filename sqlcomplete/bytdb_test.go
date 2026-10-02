package sqlcomplete

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	bytdbdrv "github.com/rohanthewiz/bytdb/stdlib"
)

// bytdbCalls is a sample call of each function bytdb is offered, in a
// statement it can run against the table and sequence the test makes. Every
// function in bytdbFuncs must have one: a function added to the list without
// a call here fails, so nothing is suggested that was never run.
var bytdbCalls = map[string]string{
	"count":                 "SELECT count(*) FROM t",
	"sum":                   "SELECT sum(n) FROM t",
	"avg":                   "SELECT avg(n) FROM t",
	"min":                   "SELECT min(n) FROM t",
	"max":                   "SELECT max(n) FROM t",
	"row_number":            "SELECT row_number() OVER (ORDER BY n) FROM t",
	"rank":                  "SELECT rank() OVER (ORDER BY n) FROM t",
	"dense_rank":            "SELECT dense_rank() OVER (ORDER BY n) FROM t",
	"lag":                   "SELECT lag(n) OVER (ORDER BY n) FROM t",
	"lead":                  "SELECT lead(n, 1, 0) OVER (ORDER BY n) FROM t",
	"first_value":           "SELECT first_value(n) OVER (ORDER BY n) FROM t",
	"last_value":            "SELECT last_value(n) OVER (ORDER BY n) FROM t",
	"nth_value":             "SELECT nth_value(n, 1) OVER (ORDER BY n) FROM t",
	"coalesce":              "SELECT coalesce(s, 'x') FROM t",
	"nullif":                "SELECT nullif(n, 1) FROM t",
	"lower":                 "SELECT lower(s) FROM t",
	"upper":                 "SELECT upper(s) FROM t",
	"length":                "SELECT length(s) FROM t",
	"char_length":           "SELECT char_length(s) FROM t",
	"array_to_string":       "SELECT array_to_string(tags, ',') FROM t",
	"array_length":          "SELECT array_length(tags, 1) FROM t",
	"now":                   "SELECT now()",
	"transaction_timestamp": "SELECT transaction_timestamp()",
	"statement_timestamp":   "SELECT statement_timestamp()",
	"clock_timestamp":       "SELECT clock_timestamp()",
	"gen_random_uuid":       "SELECT gen_random_uuid()",
	"nextval":               "SELECT nextval('sq')",
	"currval":               "SELECT currval('sq')", // after nextval: the calls run in this order
	"setval":                "SELECT setval('sq', 10)",
	"lastval":               "SELECT lastval()",
	"version":               "SELECT version()",
	"current_database":      "SELECT current_database()",
	"current_schema":        "SELECT current_schema()",
	"CURRENT_DATE":          "SELECT CURRENT_DATE",
	"CURRENT_TIMESTAMP":     "SELECT CURRENT_TIMESTAMP",
	"LOCALTIMESTAMP":        "SELECT LOCALTIMESTAMP",
}

// TestBytdbFuncsEvaluate runs every function completion offers on bytdb
// against an embedded bytdb, in the list's order, on one connection (currval
// needs the session's nextval). An "unknown function" — bytdb's answer for a
// name it does not implement — or any other error fails it.
func TestBytdbFuncsEvaluate(t *testing.T) {
	dbh, err := sql.Open(bytdbdrv.DriverName, filepath.Join(t.TempDir(), "fn.bytdb"))
	if err != nil {
		t.Fatal(err)
	}
	defer dbh.Close()
	dbh.SetMaxOpenConns(1) // one session, for the sequence's currval
	for _, q := range []string{
		"CREATE TABLE t (id int PRIMARY KEY, n int, s text, tags text[])",
		"INSERT INTO t VALUES (1, 1, 'a', '{x,y}'), (2, 2, NULL, '{z}')",
		"CREATE SEQUENCE sq",
	} {
		if _, err := dbh.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for _, f := range dialectFor("bytdb").funcs {
		q, ok := bytdbCalls[f.name]
		if !ok {
			t.Errorf("%s is offered on bytdb with no sample call in bytdbCalls", f.name)
			continue
		}
		rows, err := dbh.Query(q)
		if err == nil {
			for rows.Next() {
			}
			err = rows.Err()
			rows.Close()
		}
		if err != nil {
			t.Errorf("%s: %s: %v", f.name, q, err)
		}
	}
}

// bytdb gets its own list, not Postgres's: what it lacks is not offered, and
// its keywords stay Postgres's.
func TestBytdbVocabulary(t *testing.T) {
	r := Complete(Request{Driver: "bytdb", Buffer: "SELECT ", Caret: 7})
	labels := map[string]bool{}
	for _, it := range r.Items {
		labels[it.Label] = true
	}
	for _, missing := range []string{"jsonb_agg", "trim", "round", "pg_size_pretty", "date_trunc"} {
		if labels[missing] {
			t.Errorf("%s is offered on bytdb, which does not implement it", missing)
		}
	}
	for _, want := range []string{"coalesce", "gen_random_uuid", "row_number", "ILIKE"} {
		if !labels[want] && !labels[strings.ToLower(want)] {
			t.Errorf("%s is not offered on bytdb", want)
		}
	}
	r = Complete(Request{Driver: "bytdb", Buffer: "SELECT x::interv", Caret: 16})
	if len(r.Items) != 0 {
		t.Errorf("bytdb has no interval type to cast to: %v", r.Items)
	}
}
