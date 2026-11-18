package sqlsplit

import "strings"

// Reserved words, per engine (N-299): a name that is one of these must be
// quoted to be read as a name. Completion (sqlcomplete's dialect.quote)
// and the query designer (designer's dialect.ident) both had one small
// list for every engine, made from Postgres's clause words, so on MySQL
// completion inserted groups, rank, condition, range and interval bare,
// and the designer wrote `FROM rank r`, all errors on MySQL 8; and on
// SQLite the designer's `SELECT r.transaction` failed with "near
// "transaction": syntax error".
//
// The lists are the engines' own: MySQL 8.4's reserved words (the ones
// marked (R) in its keyword list, those of 8.0 kept, which quoting costs
// nothing to keep), SQLite's keywords (all of them: SQLite lets some be
// bare names by a fallback rule, but which ones depends on where they
// stand, and a quoted one is always right), and Postgres's reserved and
// "reserved (can be function or type)" words, which bytdb, speaking
// Postgres's SQL, shares. Over-quoting is harmless — a quoted name means
// the same as a bare one that needed no quotes — so each list errs long.

var (
	mysqlReserved = wordSet(`accessible add all alter analyze and as asc asensitive before between
		bigint binary blob both by call cascade case change char character check collate column
		condition constraint continue convert create cross cube cume_dist current_date current_time
		current_timestamp current_user cursor database databases day_hour day_microsecond day_minute
		day_second dec decimal declare default delayed delete dense_rank desc describe deterministic
		distinct distinctrow div double drop dual each else elseif empty enclosed escaped except exists
		exit explain false fetch first_value float float4 float8 for force foreign from fulltext
		function generated get grant group grouping groups having high_priority hour_microsecond
		hour_minute hour_second if ignore in index infile inner inout insensitive insert int int1 int2
		int3 int4 int8 integer intersect interval into io_after_gtids io_before_gtids is iterate join
		json_table key keys kill lag last_value lateral lead leading leave left like limit linear lines
		load localtime localtimestamp lock long longblob longtext loop low_priority manual
		master_bind master_ssl_verify_server_cert match maxvalue mediumblob mediumint mediumtext
		middleint minute_microsecond minute_second mod modifies natural not no_write_to_binlog
		nth_value ntile null numeric of on optimize optimizer_costs option optionally or order out
		outer outfile over parallel partition percent_rank precision primary procedure purge qualify
		range rank read reads read_write real recursive references regexp release rename repeat
		replace require resignal restrict return revoke right rlike row rows row_number schema
		schemas second_microsecond select sensitive separator set show signal smallint spatial
		specific sql sqlexception sqlstate sqlwarning sql_big_result sql_calc_found_rows
		sql_small_result ssl starting stored straight_join system table tablesample terminated then
		tinyblob tinyint tinytext to trailing trigger true undo union unique unlock unsigned update
		usage use using utc_date utc_time utc_timestamp values varbinary varchar varcharacter varying
		virtual when where while window with write xor year_month zerofill`)

	sqliteReserved = wordSet(`abort action add after all alter always analyze and as asc attach
		autoincrement before begin between by cascade case cast check collate column commit conflict
		constraint create cross current current_date current_time current_timestamp database default
		deferrable deferred delete desc detach distinct do drop each else end escape except exclude
		exclusive exists explain fail filter first following for foreign from full generated glob
		group groups having if ignore immediate in index indexed initially inner insert instead
		intersect into is isnull join key last left like limit match materialized natural no not
		nothing notnull null nulls of offset on or order others outer over partition plan pragma
		preceding primary query raise range recursive references regexp reindex release rename
		replace restrict returning right rollback row rows savepoint select set table temp temporary
		then ties to transaction trigger unbounded union unique update using vacuum values view
		virtual when where window with without`)

	postgresReserved = wordSet(`all analyse analyze and any array as asc asymmetric both case cast
		check collate column constraint create current_catalog current_date current_role
		current_time current_timestamp current_user default deferrable desc distinct do else end
		except false fetch for foreign from grant group having in initially intersect into lateral
		leading limit localtime localtimestamp not null offset on only or order placing primary
		references returning select session_user some symmetric system_user table then to trailing
		true union unique user using variadic when where window with
		authorization binary collation concurrently cross current_schema freeze full ilike inner is
		isnull join left like natural notnull outer overlaps right similar tablesample verbose`)
)

// IsReserved reports whether word (any case) is reserved on engine — a
// driver name: postgres, mysql (or mariadb), sqlite (or sqlite3), bytdb.
// An unknown engine is read as Postgres, as the vocabularies read it.
func IsReserved(engine, word string) bool {
	w := strings.ToLower(word)
	switch strings.ToLower(engine) {
	case "mysql", "mariadb":
		return mysqlReserved[w]
	case "sqlite", "sqlite3":
		return sqliteReserved[w]
	}
	return postgresReserved[w]
}

func wordSet(s string) map[string]bool {
	m := map[string]bool{}
	for w := range strings.FieldsSeq(s) {
		m[w] = true
	}
	return m
}
