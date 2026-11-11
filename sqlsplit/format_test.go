package sqlsplit

import (
	"strings"
	"testing"
)

// logQuery is a query as a program logged it: a label in front, the whole
// thing quoted as a string literal, its newlines escaped.
const logQuery = `"SQL query after template replacement:\nupdate lymbic_stg.stg_dim_item set service_account=  (string_to_array(name ,'_'))[1];\n\nwith tmp as\n (\n  select g.*,ga.location_id as locationid\n    FROM  lymbic_stg.stg_dim_group g\n    JOIN   (select  distinct  group_location_membership_id_ as group_location_id , location_id\n       FROM  es_envizi.location_group_allocation alloc\n          JOIN    es_envizi.dim_location l  on alloc.location = l.location ) ga\n         ON (g.group_location_id = ga.group_location_id )\n  )\n update    lymbic_stg.stg_dim_group s  set location_id = tmp.locationid\n from tmp\n where tmp.group_location_id = s.group_location_id;"`

func TestFormatLogQuery(t *testing.T) {
	want := `UPDATE lymbic_stg.stg_dim_item
SET service_account = (string_to_array(name, '_'))[1];

WITH tmp AS (
  SELECT g.*, ga.location_id AS locationid
  FROM lymbic_stg.stg_dim_group g
  JOIN (
    SELECT DISTINCT
      group_location_membership_id_ AS group_location_id,
      location_id
    FROM es_envizi.location_group_allocation alloc
    JOIN es_envizi.dim_location l ON alloc.location = l.location
  ) ga ON (g.group_location_id = ga.group_location_id)
)
UPDATE lymbic_stg.stg_dim_group s
SET location_id = tmp.locationid
FROM tmp
WHERE tmp.group_location_id = s.group_location_id;`
	if got := Format(logQuery); got != want {
		t.Errorf("Format =\n%s\n\nwant\n%s", got, want)
	}
}

func TestFormatLayout(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"short select stays on its lines", "select a,b from t where x=1",
			"SELECT a, b\nFROM t\nWHERE x = 1"},
		{"long list: an item per line; long condition: an AND per line",
			"select aaaaaaaaaaaaaaaaaaaa, bbbbbbbbbbbbbbbbbbbbbbbbb, cccccccccccccccccccccccccc, ddddddddddd from t " +
				"where aaaaaaaaaaaaaaaaaaaa = 1 and bbbbbbbbbbbbbbbbbbbbbbbbb between 1 and 2 or cccccccccccccccccccccccccc is null",
			"SELECT\n  aaaaaaaaaaaaaaaaaaaa,\n  bbbbbbbbbbbbbbbbbbbbbbbbb,\n  cccccccccccccccccccccccccc,\n  ddddddddddd\nFROM t\n" +
				"WHERE aaaaaaaaaaaaaaaaaaaa = 1\n  AND bbbbbbbbbbbbbbbbbbbbbbbbb BETWEEN 1 AND 2\n  OR cccccccccccccccccccccccccc IS NULL"},
		{"a subquery of three clauses is a block", "select * from t where id in (select id from u where ok)",
			"SELECT *\nFROM t\nWHERE id IN (\n  SELECT id\n  FROM u\n  WHERE ok\n)"},
		{"a two-clause subquery stays inline when it fits", "select * from t where id in (select id from u)",
			"SELECT *\nFROM t\nWHERE id IN (SELECT id FROM u)"},
		{"create table: a column per line",
			"create table t (id serial primary key, price numeric(10,2) default 0)",
			"CREATE TABLE t (\n  id serial PRIMARY KEY,\n  price numeric(10, 2) DEFAULT 0\n)"},
		{"insert, upsert, returning",
			"insert into t (a, b) values (1, 'x'), (2, 'y') on conflict (a) do update set b = excluded.b returning *",
			"INSERT INTO t (a, b)\nVALUES (1, 'x'), (2, 'y')\nON CONFLICT (a) DO UPDATE\nSET b = excluded.b\nRETURNING *"},
		{"words that look like clauses but are not",
			"select a from t where a is distinct from b for update",
			"SELECT a\nFROM t\nWHERE a IS DISTINCT FROM b\nFOR UPDATE"},
		{"spacing: casts, arrays, signs, calls, dotted keywords",
			"select a :: int,arr [1],- x,count( * ),t.key,left(s,2),x=-1 from t",
			"SELECT a::int, arr[1], -x, count(*), t.key, LEFT(s, 2), x = -1\nFROM t"},
		{"joins", "select * from a left outer join b using(id) inner join c on c.id=a.id",
			"SELECT *\nFROM a\nLEFT OUTER JOIN b USING (id)\nINNER JOIN c ON c.id = a.id"},
		{"union", "select 1 union all select 2",
			"SELECT 1\nUNION ALL\nSELECT 2"},
		{"comments stay where they were", "-- top\nselect 1; -- one\nselect a, -- the a\n b from t",
			"-- top\nSELECT 1; -- one\n\nSELECT\n  a, -- the a\n  b\nFROM t"},
		{"a long CASE breaks per WHEN",
			"select case when aaaaaaaaaaaaaaaaaaaaaaaaaa > 1 then 'bigggggggggggg' when aaaaaaaaaaaaaaaaaaaaaaaaaa < -1 then 'negative' else 'small' end from t",
			"SELECT CASE\n  WHEN aaaaaaaaaaaaaaaaaaaaaaaaaa > 1 THEN 'bigggggggggggg'\n  WHEN aaaaaaaaaaaaaaaaaaaaaaaaaa < -1 THEN 'negative'\n  ELSE 'small'\nEND\nFROM t"},
		{"strings and quoted names are left as written", `select 'a  ;  b', "Mixed Case", $$ x  y $$ from "T"`,
			`SELECT 'a  ;  b', "Mixed Case", $$ x  y $$` + "\nFROM \"T\""},
		{"no statement words: left as one run", "foo bar  baz", "foo bar baz"},
	}
	for _, c := range cases {
		if got := Format(c.in); got != c.want {
			t.Errorf("%s:\nFormat(%q) =\n%s\n\nwant\n%s", c.name, c.in, got, c.want)
		}
	}
}

func TestUnwrap(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain SQL is untouched", "  select 1  ", "select 1"},
		{"a quoted log string", `"select 1;\nselect \"x\""`, "select 1;\nselect \"x\""},
		{"single quotes", `'select 1\n from t'`, "select 1\n from t"},
		{"escaped newlines, no quotes", `select 1\nfrom t`, "select 1\nfrom t"},
		{"a \\n inside a SQL string is SQL's", `select 'a\nb' from t`, `select 'a\nb' from t`},
		{"a label before the query", "SQL query after template replacement:\nupdate t set a = 1", "update t set a = 1"},
		{"a label on the same line", "executing: select 1", "select 1"},
		{"a label that starts with a statement word", "Update query: update t set a = 1", "update t set a = 1"},
		{"a label with no space", "query:select 1", "select 1"},
		{"a log line, prose then a line of SQL", "2026-10-24 INFO runner\nselect 1", "select 1"},
		{"a cast's :: is not a label", "select a::text from t", "select a::text from t"},
		{"prose with no query stays", "hello there", "hello there"},
	}
	for _, c := range cases {
		if got := Unwrap(c.in); got != c.want {
			t.Errorf("%s: Unwrap(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

// Formatting is layout only: the output's tokens are the input's (keywords
// in any case), in the same order, comments included — and formatting it
// again changes nothing.
func TestFormatKeepsTokensAndSettles(t *testing.T) {
	inputs := []string{
		Unwrap(logQuery),
		"select a, b, case when x > 1 then 'big' else 'small' end as size, count(*) from t where a between 1 and 2 and b in (select id from u where z = 1 and q = 2) group by a, b order by 1 desc limit 10",
		"-- top\nselect 1; -- one\n/* two */ select a::int, arr[1], -x from t left outer join u using (id) where a is distinct from b",
		"create table t (id serial primary key, name text not null, price numeric(10,2) default 0)",
		"with recursive r(n) as (select 1 union all select n + 1 from r where n < 10) select * from r",
		"select json_col->>'k', j #> '{a,b}', x @> y, @var, @@global.v, E'it''s' from t where s ~* '^a' and n <> 1",
		"update t set a = (select max(b) from u where u.id = t.id and u.ok), c = default where x in (1, 2, 3)",
		"select * from t where (a = 1 or (b = 2 and c = 3)", // unbalanced: kept, not judged
		"select case when a then 1 from t", // CASE with no END
		"delete from t where id = $1 and name = :name and z = ?",
	}
	for _, in := range inputs {
		out := Format(in)
		if a, b := tokenTexts(in), tokenTexts(out); strings.Join(a, " ") != strings.Join(b, " ") {
			t.Errorf("tokens changed:\nin  %q\nout %q", a, b)
		}
		if again := Format(out); again != out {
			t.Errorf("Format is not settled for %q:\nonce\n%s\n\ntwice\n%s", in, out, again)
		}
		// The same holds in every keyword case, and keeping the case is
		// layout alone in the strictest sense: not one token's text differs.
		for _, k := range []KeywordCase{LowerKeywords, KeepKeywords} {
			o := FormatOptions{Keywords: k}
			out := Generic.FormatWith(in, o)
			if a, b := tokenTexts(in), tokenTexts(out); strings.Join(a, " ") != strings.Join(b, " ") {
				t.Errorf("%s: tokens changed:\nin  %q\nout %q", k, a, b)
			}
			if again := Generic.FormatWith(out, o); again != out {
				t.Errorf("%s: not settled for %q:\nonce\n%s\n\ntwice\n%s", k, in, out, again)
			}
			if k == KeepKeywords {
				if a, b := rawTokenTexts(in), rawTokenTexts(out); strings.Join(a, " ") != strings.Join(b, " ") {
					t.Errorf("keep: a token's text changed:\nin  %q\nout %q", a, b)
				}
			}
		}
	}
}

// rawTokenTexts is the texts of sql's tokens exactly as written, keywords'
// case included (tokenTexts upper-cases the keywords so any case compares).
func rawTokenTexts(sql string) []string {
	var out []string
	for _, t := range fmtLex(sql, Generic) {
		out = append(out, t.text)
	}
	return out
}

// A KeywordCase changes the keywords' case and nothing else: the lines
// break in the same places, and identifiers, strings, comments and a
// keyword in a dotted name keep theirs in every case.
func TestFormatKeywordCase(t *testing.T) {
	in := "Select a, T.Key, 'Select' From t -- From here\nwhere b = 1 And c"
	want := map[KeywordCase]string{
		UpperKeywords: "SELECT a, T.Key, 'Select'\nFROM t -- From here\nWHERE b = 1 AND c",
		LowerKeywords: "select a, T.Key, 'Select'\nfrom t -- From here\nwhere b = 1 and c",
		KeepKeywords:  "Select a, T.Key, 'Select'\nFrom t -- From here\nwhere b = 1 And c",
	}
	for k, w := range want {
		if got := Generic.FormatWith(in, FormatOptions{Keywords: k}); got != w {
			t.Errorf("%s:\ngot\n%s\n\nwant\n%s", k, got, w)
		}
	}
	// the zero options are Format's own output, and so is a case outside
	// the three
	if a, b := Format(in), Generic.FormatWith(in, FormatOptions{}); a != b {
		t.Errorf("Format and FormatWith(zero) differ:\n%s\n\n%s", a, b)
	}
	if got := Generic.FormatWith(in, FormatOptions{Keywords: KeywordCase(9)}); got != want[UpperKeywords] {
		t.Errorf("KeywordCase(9) = %q, want the upper-case layout", got)
	}
	// FormatSpanWith keeps the blanks around a span in every case
	if got := Generic.FormatSpanWith("\n  select 1\n", FormatOptions{Keywords: KeepKeywords}); got != "\n  select 1\n" {
		t.Errorf("FormatSpanWith keep = %q", got)
	}
	// A lower-case buffer that is already laid out is unchanged when its
	// case is kept or lowered — the editors then say "already formatted"
	// rather than making an undo step out of nothing.
	laid := "select a\nfrom t\nwhere b = 1"
	for _, k := range []KeywordCase{LowerKeywords, KeepKeywords} {
		if got := Generic.FormatWith(laid, FormatOptions{Keywords: k}); got != laid {
			t.Errorf("%s changed a laid-out buffer:\n%s", k, got)
		}
	}
}

// ParseKeywordCase reads the config's spellings, and String writes them
// back; a typo reads as the default with ok false.
func TestParseKeywordCase(t *testing.T) {
	cases := []struct {
		in   string
		want KeywordCase
		ok   bool
	}{
		{"", UpperKeywords, true},
		{"upper", UpperKeywords, true},
		{" Lower ", LowerKeywords, true},
		{"KEEP", KeepKeywords, true},
		{"preserve", UpperKeywords, false},
		{"lowercase", UpperKeywords, false},
	}
	for _, c := range cases {
		got, ok := ParseKeywordCase(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("ParseKeywordCase(%q) = %s, %v; want %s, %v", c.in, got, ok, c.want, c.ok)
		}
	}
	for _, k := range []KeywordCase{UpperKeywords, LowerKeywords, KeepKeywords} {
		if back, ok := ParseKeywordCase(k.String()); back != k || !ok {
			t.Errorf("%s does not read back: %s, %v", k, back, ok)
		}
	}
}

func tokenTexts(sql string) []string {
	var out []string
	for _, t := range fmtLex(sql, Generic) {
		s := t.text
		if t.kind == ftKeyword {
			s = strings.ToUpper(s)
		}
		out = append(out, s)
	}
	return out
}

// TestFormatOperators is N-262: the formatter knew a fixed list of
// operators and cut any other into single characters, spaced apart — a
// change of meaning — and glued two signs into a line comment.
func TestFormatOperators(t *testing.T) {
	for _, c := range []struct {
		d         Dialect
		sql, want string
	}{
		{MySQL, "select a <=> b from t", "SELECT a <=> b\nFROM t"},
		{Postgres, "select v <-> '[1,2]' from t", "SELECT v <-> '[1,2]'\nFROM t"},
		{Postgres, "select v <#> '[1,2]', j ?| array['a'], j ?& array['b'], j #- '{a}', j @? '$.a' from t",
			"SELECT v <#> '[1,2]', j ?| ARRAY['a'], j ?& ARRAY['b'], j #- '{a}', j @? '$.a'\nFROM t"},
		{Postgres, "select a from t where x ~~* 'a%' and r <<= '10/8'", "SELECT a\nFROM t\nWHERE x ~~* 'a%' AND r <<= '10/8'"},
		{Postgres, "select j ? 'k' from t", "SELECT j ? 'k'\nFROM t"},
		{SQLite, "select 1 where a == 2", "SELECT 1\nWHERE a == 2"},
		{Postgres, "select U&'d\\0061t\\+000061', U&\"x\" from t", "SELECT U&'d\\0061t\\+000061', U&\"x\"\nFROM t"},
		// two signs never glue into a comment
		{Generic, "SELECT - -1, 2", "SELECT - -1, 2"},
		{Generic, "select a from t where y = - -3", "SELECT a\nFROM t\nWHERE y = - -3"},
		// a sign after an operator still attaches to its number
		{Generic, "select a from t where a=-1 and b*-2 > 0", "SELECT a\nFROM t\nWHERE a = -1 AND b * -2 > 0"},
		// placeholders stay placeholders where the engine has them
		{MySQL, "select a from t where a=? and b=@v", "SELECT a\nFROM t\nWHERE a = ? AND b = @v"},
		{SQLite, "select a from t where a=?", "SELECT a\nFROM t\nWHERE a = ?"},
		// a comment inside what looks like an operator run ends it
		{Generic, "select 1+-- note\n2", "SELECT 1 + -- note\n2"},
	} {
		got := c.d.Format(c.sql)
		if got != c.want {
			t.Errorf("%v.Format(%q):\n%s\nwant\n%s", c.d, c.sql, got, c.want)
		}
		// and whatever the layout, the code reads the same: the same
		// statements, the same tokens
		if a, b := len(c.d.Split(c.sql)), len(c.d.Split(got)); a != b {
			t.Errorf("%v: %d statements before, %d after", c.d, a, b)
		}
	}
}
