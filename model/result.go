package model

import "time"

// Result holds one tabular result set from a query or exec statement.
type Result struct {
	Conn      string        // connection name the statement ran on
	Query     string        // the statement text
	Columns   []string      // column names
	Rows      [][]string    // display-ready values (NULL rendered as "NULL")
	Raw       [][]any       // typed values, used for JSON export
	Duration  time.Duration // wall time for the statement
	Truncated bool          // true when the row limit was hit
	IsExec    bool          // true for non-query statements (INSERT/UPDATE/...)
	// Affected is rows affected, when IsExec. A result with rows has 0
	// here, except where Postgres counted them in the command tag of a
	// statement whose verb did not say it would return rows: FETCH 10 has
	// 10, every row fetched, past a max_rows cut too, and so has an
	// EXECUTE of a prepared SELECT. It is what a script's s.Exec of such a
	// statement returns, as it did when one ran as an Exec. Every UI shows
	// the rows of a result that has them and reads Affected only when
	// IsExec.
	Affected int64

	// Transposed marks a result turned on its side (export.Transpose). Each
	// row is one of the source's columns, its name in column 0, so the HTML
	// renderings draw that column as row headers rather than as data.
	//
	// (The first sentence of each field's comment here is what the script
	// API summary given to the assistant quotes — sdb/sdbapi, which keeps to
	// a size budget — so it is kept short, and the rest says the detail.)
	Transposed bool

	// More is the statement's result sets after this one. The Result itself
	// is the first, and these follow in the order the server sent them. One
	// statement has several when it is a procedure call that runs several
	// SELECTs (MySQL's `CALL monthly_report()`), each of which the server
	// answers with a set of its own. Nil for every other statement. The
	// sets are whole Results rather than a slice of column/row pairs so
	// that whatever shows, exports or lands one Result does the same with
	// each of these, unchanged.
	More []*Result
	// MoreCut says sets past More's cap were dropped. The server sent more
	// result sets than db keeps for one statement (max_result_sets in the
	// config, 51 by default, this set included); the ones past the cap were
	// read off the wire and not kept.
	MoreCut bool
}

// Sets is the statement's result sets in order: r, then r.More. It is what
// a caller ranges over to treat a statement's every set alike. A script
// need not range it to show them: s.Show(r) shows each set already.
func (r *Result) Sets() []*Result {
	return append([]*Result{r}, r.More...)
}

// RowCount returns the number of data rows.
func (r *Result) RowCount() int {
	return len(r.Rows)
}
