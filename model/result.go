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
	Affected  int64         // rows affected, when IsExec
}

// RowCount returns the number of data rows.
func (r *Result) RowCount() int {
	return len(r.Rows)
}
