package etl

import (
	"slices"
	"strings"

	"github.com/rohanthewiz/serr"
)

// CreateStmt is the CREATE TABLE IF NOT EXISTS for a table on dst that can
// hold rows whose columns carry the driver type names in dbTypes (what
// Reader.DBTypes reports; "" or an unknown name becomes text). It is
// Copy's createDDL with the catalog lookups left out — for a destination
// whose rows were reshaped on the way (a pipeline's transform added or
// renamed a column), where there is no source table to ask:
//
//	src Postgres, dst Postgres ─► pgTypeName: INT4 stays integer, not bigint
//	anything else              ─► by type family (engine.go columnType)
//
// key names the primary key columns; each must be among cols. bytdb
// refuses a table without one, so there a missing key is an error with a
// hint rather than a failure at the server. src may be 0 when the rows'
// origin is unknown (a Go source), which disables the Postgres fidelity.
func CreateStmt(src, dst Engine, table string, cols, dbTypes, key []string) (string, error) {
	if len(cols) == 0 {
		return "", serr.New("etl: a table needs at least one column", "table", table)
	}
	for _, k := range key {
		if !slices.Contains(cols, k) {
			return "", serr.New("etl: key column is not among the columns", "table", table, "column", k)
		}
	}
	defs := make([]string, len(cols))
	for i, name := range cols {
		t := ""
		if i < len(dbTypes) {
			t = dbTypes[i]
		}
		f := familyOf(t)
		typ := dst.columnType(f)
		switch {
		case src == Postgres && dst == Postgres && t != "":
			typ = pgTypeName(t)
		case dst == MySQL && slices.Contains(key, name) && (f == famText || f == famBytes):
			// MySQL cannot index a LONGTEXT/LONGBLOB without a prefix
			// length, so a text key gets a bounded type instead.
			typ = map[typeFamily]string{famText: "VARCHAR(255)", famBytes: "VARBINARY(255)"}[f]
		}
		defs[i] = dst.QuoteIdent(name) + " " + typ
	}
	if len(key) > 0 {
		defs = append(defs, "PRIMARY KEY ("+dst.quoteCols(key)+")")
	} else if dst == Bytdb {
		return "", serr.New("etl: bytdb tables need a primary key", "table", table,
			"hint", "name the key columns, or create the table yourself and load without Create")
	}
	return "CREATE TABLE IF NOT EXISTS " + dst.QuoteTable(table) + " (\n\t" +
		strings.Join(defs, ",\n\t") + "\n)", nil
}

// TransactionalDDL reports whether a CREATE TABLE on e can run inside the
// load's transaction and roll back with it (see transactionalDDL). A
// caller that builds its own Setup puts the CREATE there on such an
// engine, and runs it on its own first on the others.
func (e Engine) TransactionalDDL() bool { return e.transactionalDDL() }
