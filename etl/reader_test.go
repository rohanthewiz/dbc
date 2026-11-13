package etl

import "testing"

// plainRead decides whether a Reader closed early may stop its query and
// roll back (N-276): only a query that writes nothing may.
func TestPlainRead(t *testing.T) {
	for _, c := range []struct {
		e     Engine
		query string
		want  bool
	}{
		{Postgres, "SELECT * FROM events", true},
		{Postgres, "  -- a note\nselect id from t where note = 'delete me'", true},
		{Postgres, "WITH r AS (SELECT 1) SELECT * FROM r", true},
		{Postgres, "VALUES (1), (2)", true},
		{Postgres, "TABLE events", true},
		{MySQL, "SHOW TABLES", true},
		{Postgres, "DELETE FROM jobs RETURNING *", false},
		{Postgres, "WITH d AS (DELETE FROM jobs RETURNING *) SELECT * FROM d", false},
		{Postgres, "SELECT * INTO events_copy FROM events", false},
		{MySQL, "SELECT id INTO @v FROM t LIMIT 1", false},
		{MySQL, "CALL report()", false},
		{Postgres, "EXPLAIN ANALYZE DELETE FROM jobs", false},
	} {
		if got := plainRead(c.query, c.e.Dialect()); got != c.want {
			t.Errorf("%s %q: plainRead = %v, want %v", c.e, c.query, got, c.want)
		}
	}
}
