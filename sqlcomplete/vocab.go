package sqlcomplete

import (
	"strings"
	"sync"
)

// dialect is one engine's vocabulary and naming rules.
//
// Postgres gets the fullest vocabulary: it is the engine dbc is used with
// most, and its function library is large enough that remembering the exact
// name (jsonb_array_elements_text? regexp_matches?) is what a completion is
// for. MySQL and SQLite get the common core plus their own everyday
// functions.
//
// bytdb speaks Postgres's SQL — its keywords, clauses and quoting are
// Postgres's — but implements a small fraction of its functions, and not
// even all of the common core (no trim, replace, substr, abs or round). It
// therefore gets its own function and type lists, of exactly what it
// evaluates; TestBytdbFuncsEvaluate runs every one against an embedded
// bytdb, so a bytdb upgrade that drops one, or a name added here that it
// never had, fails the build rather than a user's query.
type dialect struct {
	name     string
	starts   []string // what a statement begins with
	clauses  []string // what follows a finished phrase, offered first there
	keywords []string // the rest of the vocabulary
	funcs    []fn
	types    []string

	// backtick quotes names with `…` (MySQL) rather than "…"
	backtick bool
	// fold is the case an unquoted name is folded to: 'l' lower
	// (Postgres), 0 none (MySQL and SQLite compare names as written or
	// case-insensitively, so a capital needs no quotes).
	fold byte
	// defaultSchemas are the schemas a bare table name resolves to without
	// a search_path change; nil means every schema (one per connection).
	defaultSchemas []string
}

// fn is one function: its name, its signature with the result type, and a
// one-line description. bare functions are called without parentheses.
type fn struct {
	name, sig, doc string
	bare           bool
}

// quote writes name as the dialect needs it: as is when it is a plain
// identifier the engine reads back unchanged, quoted otherwise — a capital on
// Postgres (it would fold to lower case), a space or a symbol anywhere, or a
// reserved word.
func (d *dialect) quote(name string) string {
	plain := name != "" && !isDigit(name[0])
	for i := 0; i < len(name) && plain; i++ {
		b := name[i]
		switch {
		case b == '_' || isDigit(b) || (b >= 'a' && b <= 'z'):
		case b >= 'A' && b <= 'Z':
			plain = d.fold == 0
		case b == '$':
			plain = !d.backtick && i > 0
		default:
			plain = false
		}
	}
	if plain && !reserved[strings.ToLower(name)] && !quoteWords[strings.ToLower(name)] {
		return name
	}
	if d.backtick {
		return "`" + strings.ReplaceAll(name, "`", "``") + "`"
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// quoteWords are reserved words a column is commonly named after that are not
// in reserved (which is about aliases): a column called "user" or "order"
// must be quoted to be read as a column.
var quoteWords = map[string]bool{
	"user": true, "table": true, "column": true, "check": true, "primary": true,
	"references": true, "unique": true, "constraint": true, "grant": true, "only": true,
	"analyse": true, "analyze": true, "collate": true, "current_user": true, "session_user": true,
	"current_date": true, "current_time": true, "current_timestamp": true, "desc": true, "key": true,
}

// dialectFor maps a configured driver name to its dialect. An unknown driver
// gets Postgres's, the richest and the most likely.
func dialectFor(driver string) *dialect {
	dialectsOnce.Do(buildDialects)
	switch strings.ToLower(driver) {
	case "mysql", "mariadb":
		return mysqlD
	case "sqlite", "sqlite3":
		return sqliteD
	case "bytdb":
		return bytdbD
	}
	return pgD
}

var (
	dialectsOnce                 sync.Once
	pgD, mysqlD, sqliteD, bytdbD *dialect
)

// words splits a newline-separated list; a line may hold a phrase ("GROUP BY").
func words(s string) []string {
	var out []string
	for l := range strings.SplitSeq(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// funcs reads a table of functions, one per line:
//
//	name(args) → result | what it does
//
// A line starting with "=" is a bare function (CURRENT_DATE).
func funcs(s string) []fn {
	var out []fn
	for _, l := range words(s) {
		bare := strings.HasPrefix(l, "=")
		l = strings.TrimPrefix(l, "=")
		sig, doc, _ := strings.Cut(l, "|")
		sig, doc = strings.TrimSpace(sig), strings.TrimSpace(doc)
		name := sig
		if i := strings.IndexAny(sig, "( "); i > 0 {
			name = sig[:i]
		}
		out = append(out, fn{name: name, sig: sig, doc: doc, bare: bare})
	}
	return out
}

func buildDialects() {
	pgD = &dialect{
		name: "postgres", fold: 'l', defaultSchemas: []string{"public"},
		starts:   append(words(commonStarts), words(pgStarts)...),
		clauses:  append(words(commonClauses), words(pgClauses)...),
		keywords: append(words(commonKeywords), words(pgKeywords)...),
		funcs:    append(funcs(commonFuncs), funcs(pgFuncs)...),
		types:    words(pgTypes),
	}
	bytdbD = &dialect{
		name: "bytdb", fold: 'l', defaultSchemas: []string{"public"},
		starts:   pgD.starts,
		clauses:  pgD.clauses,
		keywords: pgD.keywords,
		funcs:    funcs(bytdbFuncs),
		types:    words(bytdbTypes),
	}
	mysqlD = &dialect{
		name: "mysql", backtick: true,
		starts:   append(words(commonStarts), words(mysqlStarts)...),
		clauses:  append(words(commonClauses), words(mysqlClauses)...),
		keywords: append(words(commonKeywords), words(mysqlKeywords)...),
		funcs:    append(funcs(commonFuncs), funcs(mysqlFuncs)...),
		types:    words(mysqlTypes),
	}
	sqliteD = &dialect{
		name:     "sqlite",
		starts:   append(words(commonStarts), words(sqliteStarts)...),
		clauses:  append(words(commonClauses), words(sqliteClauses)...),
		keywords: append(words(commonKeywords), words(sqliteKeywords)...),
		funcs:    append(funcs(commonFuncs), funcs(sqliteFuncs)...),
		types:    words(sqliteTypes),
	}
}

// ---------------------------------------------------------------------------
// Every dialect
// ---------------------------------------------------------------------------

const commonStarts = `
SELECT
INSERT INTO
UPDATE
DELETE FROM
WITH
CREATE TABLE
CREATE INDEX
CREATE VIEW
ALTER TABLE
DROP TABLE
EXPLAIN
BEGIN
COMMIT
ROLLBACK
`

const commonClauses = `
FROM
WHERE
JOIN
LEFT JOIN
INNER JOIN
RIGHT JOIN
FULL JOIN
CROSS JOIN
ON
USING
GROUP BY
ORDER BY
HAVING
LIMIT
OFFSET
UNION
UNION ALL
INTERSECT
EXCEPT
AS
AND
OR
NOT
IN
IS NULL
IS NOT NULL
LIKE
BETWEEN
ASC
DESC
SET
VALUES
`

const commonKeywords = `
SELECT
DISTINCT
FROM
WHERE
AND
OR
NOT
NULL
TRUE
FALSE
CASE
WHEN
THEN
ELSE
END
EXISTS
CAST
OVER
PARTITION BY
ROWS BETWEEN
UNBOUNDED PRECEDING
CURRENT ROW
PRIMARY KEY
FOREIGN KEY
REFERENCES
DEFAULT
UNIQUE
CHECK
CONSTRAINT
INDEX
ADD COLUMN
DROP COLUMN
RENAME TO
IF EXISTS
IF NOT EXISTS
RECURSIVE
ALL
ANY
`

const commonFuncs = `
count(expr) → bigint | rows where expr is not null; count(*) counts every row
sum(expr) → numeric | the sum of the non-null values
avg(expr) → numeric | the mean of the non-null values
min(expr) → same type | the smallest non-null value
max(expr) → same type | the largest non-null value
coalesce(a, b, …) → same type | the first argument that is not null
nullif(a, b) → same type | null when a = b, else a
lower(text) → text | lower-cased
upper(text) → text | upper-cased
length(text) → integer | the number of characters
trim(text) → text | without leading and trailing spaces
replace(text, from, to) → text | every from replaced with to
substr(text, start [, count]) → text | count characters from start (1-based)
abs(x) → same type | the absolute value
round(x [, digits]) → numeric | rounded to digits after the point
row_number() → bigint | the row's number within its window partition, from 1
rank() → bigint | the rank within the partition, with gaps for ties
dense_rank() → bigint | the rank within the partition, without gaps
lag(expr [, offset [, default]]) → same type | expr from offset rows before, in the window
lead(expr [, offset [, default]]) → same type | expr from offset rows after, in the window
first_value(expr) → same type | expr at the window frame's first row
last_value(expr) → same type | expr at the window frame's last row
=CURRENT_DATE → date | today's date
=CURRENT_TIMESTAMP → timestamp | the transaction's start time
`

// ---------------------------------------------------------------------------
// Postgres (and bytdb)
// ---------------------------------------------------------------------------

const pgStarts = `
TABLE
CALL
VALUES
CREATE MATERIALIZED VIEW
CREATE SCHEMA
CREATE FUNCTION
CREATE EXTENSION
CREATE SEQUENCE
CREATE TYPE
REFRESH MATERIALIZED VIEW
TRUNCATE
COPY
VACUUM
ANALYZE
EXPLAIN ANALYZE
SET
SHOW
RESET
GRANT
REVOKE
COMMENT ON
LISTEN
NOTIFY
DO
`

const pgClauses = `
ILIKE
NOT ILIKE
SIMILAR TO
IS DISTINCT FROM
IS NOT DISTINCT FROM
RETURNING
ON CONFLICT
DO NOTHING
DO UPDATE SET
FETCH FIRST
FOR UPDATE
FOR SHARE
SKIP LOCKED
NOWAIT
WINDOW
NULLS FIRST
NULLS LAST
LATERAL
FILTER (WHERE
TABLESAMPLE
`

const pgKeywords = `
DISTINCT ON
ILIKE
SIMILAR TO
IS DISTINCT FROM
RETURNING
ON CONFLICT
DO NOTHING
DO UPDATE SET
EXCLUDED
LATERAL
FILTER
WITHIN GROUP
MATERIALIZED
NOT MATERIALIZED
GENERATED ALWAYS AS IDENTITY
GENERATED BY DEFAULT AS IDENTITY
CONCURRENTLY
CASCADE
RESTRICT
ONLY
INHERITS
PARTITION OF
FOR VALUES
TABLESPACE
OWNER TO
SEARCH_PATH
SESSION
LOCAL
ISOLATION LEVEL
SERIALIZABLE
REPEATABLE READ
READ COMMITTED
LOCK TABLE
ARRAY
ROW
INTERVAL
AT TIME ZONE
SYMMETRIC
COLLATE
USING
SECURITY DEFINER
LANGUAGE
RETURNS
VOLATILE
STABLE
IMMUTABLE
CURRENT_USER
SESSION_USER
`

const pgFuncs = `
array_agg(expr [ORDER BY …]) → array | the values collected into an array
string_agg(text, delimiter [ORDER BY …]) → text | the values joined with delimiter
json_agg(expr) → json | the values as a JSON array
jsonb_agg(expr) → jsonb | the values as a JSONB array
json_object_agg(key, value) → json | the pairs as a JSON object
jsonb_object_agg(key, value) → jsonb | the pairs as a JSONB object
bool_and(bool) → boolean | true when every value is true
bool_or(bool) → boolean | true when any value is true
every(bool) → boolean | the SQL-standard bool_and
percentile_cont(fraction) WITHIN GROUP (ORDER BY expr) → double precision | the interpolated percentile
percentile_disc(fraction) WITHIN GROUP (ORDER BY expr) → same type | the first value at or past the percentile
mode() WITHIN GROUP (ORDER BY expr) → same type | the most frequent value
stddev(expr) → numeric | the sample standard deviation
variance(expr) → numeric | the sample variance
percent_rank() → double precision | the relative rank, 0 to 1
cume_dist() → double precision | the cumulative distribution, 0 to 1
ntile(buckets) → integer | which of buckets equal groups the row falls in
nth_value(expr, n) → same type | expr at the window frame's nth row
char_length(text) → integer | the number of characters
octet_length(text) → integer | the number of bytes
initcap(text) → text | each word's first letter upper-cased
ltrim(text [, chars]) → text | without leading chars (spaces)
rtrim(text [, chars]) → text | without trailing chars (spaces)
btrim(text [, chars]) → text | without leading and trailing chars (spaces)
substring(text FROM start FOR count) → text | count characters from start; or FROM pattern
position(sub IN text) → integer | where sub starts in text, 0 when absent
strpos(text, sub) → integer | where sub starts in text, 0 when absent
concat(a, b, …) → text | the arguments joined, nulls skipped
concat_ws(sep, a, b, …) → text | the arguments joined with sep, nulls skipped
left(text, n) → text | the first n characters (all but the last -n when negative)
right(text, n) → text | the last n characters
lpad(text, length [, fill]) → text | padded on the left to length
rpad(text, length [, fill]) → text | padded on the right to length
repeat(text, n) → text | text n times
reverse(text) → text | the characters in reverse
split_part(text, delimiter, n) → text | the nth field (1-based; negative counts from the end)
starts_with(text, prefix) → boolean | whether text begins with prefix
regexp_replace(text, pattern, replacement [, flags]) → text | matches of a POSIX regex replaced ('g' for all)
regexp_match(text, pattern [, flags]) → text[] | the first match's captures
regexp_matches(text, pattern [, flags]) → setof text[] | every match's captures ('g')
regexp_split_to_table(text, pattern) → setof text | text split on a regex, one row each
regexp_split_to_array(text, pattern) → text[] | text split on a regex
format(fmt, args…) → text | sprintf-style: %s, %I (identifier), %L (literal)
to_char(value, fmt) → text | a date, time or number formatted ('YYYY-MM-DD')
to_number(text, fmt) → numeric | text parsed as a number by fmt
md5(text) → text | the MD5 hash, hex
encode(bytea, format) → text | bytes as 'hex', 'base64' or 'escape'
decode(text, format) → bytea | text in 'hex', 'base64' or 'escape' as bytes
quote_ident(text) → text | text quoted as an identifier when needed
quote_literal(text) → text | text quoted as a string literal
quote_nullable(text) → text | quote_literal, or NULL for null
ceil(x) → same type | the nearest integer at or above x
floor(x) → same type | the nearest integer at or below x
trunc(x [, digits]) → numeric | truncated toward zero
mod(a, b) → same type | the remainder of a / b
power(a, b) → double precision | a raised to b
sqrt(x) → double precision | the square root
random() → double precision | a random value in [0, 1)
greatest(a, b, …) → same type | the largest argument, nulls ignored
least(a, b, …) → same type | the smallest argument, nulls ignored
now() → timestamptz | the transaction's start time
clock_timestamp() → timestamptz | the current time, changing during a statement
statement_timestamp() → timestamptz | the statement's start time
date_trunc(field, source [, zone]) → timestamp | source truncated to 'day', 'month', 'hour' …
date_part(field, source) → double precision | a field of a date or time ('year', 'dow', 'epoch' …)
extract(field FROM source) → numeric | a field of a date or time (YEAR, MONTH, EPOCH …)
age(timestamp [, timestamp]) → interval | the difference, in years, months and days
to_date(text, fmt) → date | text parsed as a date by fmt
to_timestamp(text, fmt) → timestamptz | text parsed as a timestamp by fmt; or from Unix seconds
make_date(year, month, day) → date | a date from its parts
make_timestamp(y, mo, d, h, mi, s) → timestamp | a timestamp from its parts
make_interval(years, months, weeks, days, hours, mins, secs) → interval | an interval from its parts
date_bin(stride, source, origin) → timestamp | source binned into stride-long buckets
generate_series(start, stop [, step]) → setof | the values from start to stop, one row each
jsonb_build_object(key, value, …) → jsonb | a JSONB object from key/value pairs
json_build_object(key, value, …) → json | a JSON object from key/value pairs
jsonb_build_array(a, b, …) → jsonb | a JSONB array of the arguments
json_build_array(a, b, …) → json | a JSON array of the arguments
to_json(value) → json | the value as JSON
to_jsonb(value) → jsonb | the value as JSONB
row_to_json(record) → json | a row as a JSON object
jsonb_extract_path(jsonb, key, …) → jsonb | the value at a path (the #> operator)
jsonb_extract_path_text(jsonb, key, …) → text | the value at a path as text (#>>)
jsonb_array_elements(jsonb) → setof jsonb | an array's elements, one row each
jsonb_array_elements_text(jsonb) → setof text | an array's elements as text, one row each
jsonb_array_length(jsonb) → integer | the number of elements in an array
jsonb_each(jsonb) → setof (key, value) | an object's pairs, one row each
jsonb_each_text(jsonb) → setof (key, value) | an object's pairs as text, one row each
jsonb_object_keys(jsonb) → setof text | an object's keys, one row each
jsonb_set(target, path, new [, create]) → jsonb | target with the value at path replaced
jsonb_insert(target, path, new [, after]) → jsonb | target with new inserted at path
jsonb_strip_nulls(jsonb) → jsonb | without object fields that are null
jsonb_typeof(jsonb) → text | 'object', 'array', 'string', 'number', 'boolean' or 'null'
jsonb_pretty(jsonb) → text | indented JSON text
jsonb_path_query(target, path) → setof jsonb | the items a JSON path returns
jsonb_path_exists(target, path) → boolean | whether a JSON path returns any item
array_length(array, dim) → integer | the length of a dimension (1 for a plain array)
cardinality(array) → integer | the total number of elements
array_append(array, elem) → array | the array with elem added at the end
array_prepend(elem, array) → array | the array with elem added at the start
array_cat(a, b) → array | the two arrays concatenated
array_remove(array, elem) → array | the array without every elem
array_position(array, elem) → integer | the first elem's index, null when absent
array_to_string(array, delimiter [, null]) → text | the elements joined
string_to_array(text, delimiter) → text[] | text split into an array
unnest(array) → setof | an array's elements, one row each
gen_random_uuid() → uuid | a random (version 4) UUID
nextval(sequence) → bigint | the sequence's next value
currval(sequence) → bigint | the value nextval last gave this session
setval(sequence, value) → bigint | sets the sequence's current value
pg_typeof(any) → regtype | the value's type
pg_size_pretty(bytes) → text | a byte count as kB, MB, GB …
pg_total_relation_size(regclass) → bigint | a table's bytes with its indexes and TOAST
pg_relation_size(regclass) → bigint | a table's or index's main bytes
pg_table_size(regclass) → bigint | a table's bytes without its indexes
pg_indexes_size(regclass) → bigint | the bytes of a table's indexes
pg_database_size(name) → bigint | a database's bytes
current_setting(name) → text | a setting's current value
set_config(name, value, is_local) → text | sets a setting; is_local: for the transaction only
pg_sleep(seconds) → void | waits
pg_cancel_backend(pid) → boolean | cancels a backend's current query
pg_terminate_backend(pid) → boolean | ends a backend's session
pg_backend_pid() → integer | this session's backend process id
txid_current() → bigint | the current transaction's id
version() → text | the server's version string
current_database() → name | the database connected to
current_schema() → name | the first schema on the search path
to_tsvector([config,] text) → tsvector | text as a full-text search document
to_tsquery([config,] text) → tsquery | text as a full-text query (& | ! operators)
plainto_tsquery([config,] text) → tsquery | plain text as a query, words ANDed
websearch_to_tsquery([config,] text) → tsquery | web-search syntax as a query
ts_rank(tsvector, tsquery) → real | how well a document matches a query
`

const pgTypes = `
integer
bigint
smallint
numeric
decimal
real
double precision
serial
bigserial
boolean
text
varchar
char
uuid
date
time
timestamp
timestamptz
interval
json
jsonb
bytea
inet
cidr
macaddr
money
tsvector
tsquery
int4range
int8range
numrange
tstzrange
daterange
point
xml
regclass
oid
text[]
integer[]
`

// ---------------------------------------------------------------------------
// bytdb
// ---------------------------------------------------------------------------

// bytdbFuncs is what bytdb evaluates (its sql package: aggNames, winNames,
// evalFunc, sysFuncs), with Postgres's signatures. Left out on purpose:
// the pg_*_size functions, which bytdb answers with a constant 0 for psql's
// sake (suggesting them would suggest a wrong answer), and the pg_get_*
// catalog helpers psql calls, which are no one's to type.
const bytdbFuncs = `
count(expr) → bigint | rows where expr is not null; count(*) counts every row
sum(expr) → numeric | the sum of the non-null values
avg(expr) → numeric | the mean of the non-null values
min(expr) → same type | the smallest non-null value
max(expr) → same type | the largest non-null value
row_number() → bigint | the row's number within its window partition, from 1
rank() → bigint | the rank within the partition, with gaps for ties
dense_rank() → bigint | the rank within the partition, without gaps
lag(expr [, offset [, default]]) → same type | expr from offset rows before, in the window
lead(expr [, offset [, default]]) → same type | expr from offset rows after, in the window
first_value(expr) → same type | expr at the window frame's first row
last_value(expr) → same type | expr at the window frame's last row
nth_value(expr, n) → same type | expr at the window frame's nth row
coalesce(a, b, …) → same type | the first argument that is not null
nullif(a, b) → same type | null when a = b, else a
lower(text) → text | lower-cased
upper(text) → text | upper-cased
length(text) → integer | the number of characters
char_length(text) → integer | the number of characters
array_to_string(array, delimiter [, null]) → text | the elements joined
array_length(array, dim) → integer | the length of a dimension (1 for a plain array)
now() → timestamptz | the transaction's start time
transaction_timestamp() → timestamptz | the transaction's start time
statement_timestamp() → timestamptz | the statement's start time
clock_timestamp() → timestamptz | the current time
gen_random_uuid() → uuid | a random (version 4) UUID
nextval(sequence) → bigint | the sequence's next value
currval(sequence) → bigint | the value nextval last gave this session
setval(sequence, value) → bigint | sets the sequence's current value
lastval() → bigint | the value nextval last gave this session, of any sequence
version() → text | the server's version string
current_database() → name | the database connected to
current_schema() → name | the schema names resolve in (public)
=CURRENT_DATE → date | today's date
=CURRENT_TIMESTAMP → timestamptz | the transaction's start time
=LOCALTIMESTAMP → timestamp | the transaction's start time, without a zone
`

// bytdbTypes are the casts bytdb gives a type of its own; any other name
// casts to text, so offering one (interval, inet …) would promise a
// conversion that does not happen.
const bytdbTypes = `
integer
bigint
smallint
int
boolean
real
float8
numeric
decimal
text
varchar
char
timestamp
timestamptz
date
uuid
json
jsonb
oid
regclass
`

// ---------------------------------------------------------------------------
// MySQL
// ---------------------------------------------------------------------------

const mysqlStarts = `
REPLACE INTO
CALL
INSERT IGNORE INTO
SHOW TABLES
SHOW CREATE TABLE
SHOW COLUMNS FROM
SHOW INDEX FROM
SHOW PROCESSLIST
DESCRIBE
USE
TRUNCATE TABLE
START TRANSACTION
SET
`

const mysqlClauses = `
ON DUPLICATE KEY UPDATE
REGEXP
SOUNDS LIKE
FOR UPDATE
WITH ROLLUP
`

const mysqlKeywords = `
AUTO_INCREMENT
ENGINE
CHARSET
COLLATE
UNSIGNED
STRAIGHT_JOIN
IGNORE
DUPLICATE
INTERVAL
DIV
XOR
`

const mysqlFuncs = `
group_concat(expr [ORDER BY …] [SEPARATOR sep]) → text | the values joined (',' by default)
json_arrayagg(expr) → json | the values as a JSON array
json_objectagg(key, value) → json | the pairs as a JSON object
concat(a, b, …) → text | the arguments joined; null if any is null
concat_ws(sep, a, b, …) → text | the arguments joined with sep, nulls skipped
substring(text, start [, count]) → text | count characters from start (1-based)
substring_index(text, delim, count) → text | text before the count-th delim (after it, when negative)
char_length(text) → integer | the number of characters
locate(sub, text [, start]) → integer | where sub starts in text, 0 when absent
instr(text, sub) → integer | where sub starts in text, 0 when absent
left(text, n) → text | the first n characters
right(text, n) → text | the last n characters
lpad(text, length, fill) → text | padded on the left to length
rpad(text, length, fill) → text | padded on the right to length
ifnull(a, b) → same type | b when a is null, else a
if(cond, then, else) → same type | then when cond is true, else otherwise
ceil(x) → integer | the nearest integer at or above x
floor(x) → integer | the nearest integer at or below x
rand() → double | a random value in [0, 1)
greatest(a, b, …) → same type | the largest argument
least(a, b, …) → same type | the smallest argument
now() → datetime | the statement's start time
curdate() → date | today's date
curtime() → time | the current time
date_format(date, fmt) → text | a date formatted ('%Y-%m-%d')
date_add(date, INTERVAL n unit) → datetime | date plus an interval
date_sub(date, INTERVAL n unit) → datetime | date minus an interval
datediff(a, b) → integer | days from b to a
timestampdiff(unit, a, b) → integer | b minus a in unit (DAY, HOUR …)
str_to_date(text, fmt) → datetime | text parsed by fmt
unix_timestamp([date]) → integer | seconds since the epoch
from_unixtime(seconds [, fmt]) → datetime | a Unix time as a date
year(date) → integer | the year
month(date) → integer | the month, 1–12
day(date) → integer | the day of the month
json_extract(doc, path, …) → json | the value at a path ('$.a.b'; the -> operator)
json_unquote(json) → text | a JSON string without its quotes (->> is both)
json_object(key, value, …) → json | a JSON object from key/value pairs
json_array(a, b, …) → json | a JSON array of the arguments
json_contains(target, candidate [, path]) → integer | 1 when candidate is in target
uuid() → text | a version 1 UUID
last_insert_id() → bigint | the AUTO_INCREMENT value the last insert made
found_rows() → bigint | rows the last SELECT would have returned without LIMIT
database() → text | the default database
`

const mysqlTypes = `
int
bigint
smallint
tinyint
decimal
double
float
varchar
char
text
mediumtext
longtext
blob
date
datetime
timestamp
time
year
json
enum
boolean
binary
`

// ---------------------------------------------------------------------------
// SQLite
// ---------------------------------------------------------------------------

const sqliteStarts = `
REPLACE INTO
INSERT OR REPLACE INTO
INSERT OR IGNORE INTO
PRAGMA
VACUUM
ATTACH DATABASE
`

const sqliteClauses = `
GLOB
ON CONFLICT
RETURNING
COLLATE NOCASE
`

const sqliteKeywords = `
AUTOINCREMENT
WITHOUT ROWID
STRICT
ROWID
GLOB
`

const sqliteFuncs = `
total(expr) → real | the sum as a real, 0.0 for no rows
group_concat(expr [, sep]) → text | the values joined (',' by default)
ltrim(text [, chars]) → text | without leading chars (spaces)
rtrim(text [, chars]) → text | without trailing chars (spaces)
instr(text, sub) → integer | where sub starts in text, 0 when absent
printf(fmt, args…) → text | sprintf-style formatting
ifnull(a, b) → same type | b when a is null, else a
iif(cond, then, else) → same type | then when cond is true, else otherwise
random() → integer | a random 64-bit integer
date(time, modifiers…) → text | a date as YYYY-MM-DD ('now', '+1 day' …)
time(time, modifiers…) → text | a time as HH:MM:SS
datetime(time, modifiers…) → text | YYYY-MM-DD HH:MM:SS
julianday(time, modifiers…) → real | the Julian day number
strftime(fmt, time, modifiers…) → text | a time formatted ('%Y-%m-%d')
unixepoch(time, modifiers…) → integer | seconds since the epoch
json(text) → text | text checked and minified as JSON
json_extract(json, path, …) → any | the value at a path ('$.a.b')
json_object(key, value, …) → text | a JSON object from key/value pairs
json_array(a, b, …) → text | a JSON array of the arguments
json_group_array(expr) → text | the values as a JSON array
json_group_object(key, value) → text | the pairs as a JSON object
json_each(json) → table | an array's or object's members, one row each
typeof(x) → text | 'null', 'integer', 'real', 'text' or 'blob'
last_insert_rowid() → integer | the rowid the last insert made
changes() → integer | rows the last statement changed
hex(x) → text | x as upper-case hex
quote(x) → text | x as a SQL literal
`

const sqliteTypes = `
INTEGER
REAL
TEXT
BLOB
NUMERIC
`
