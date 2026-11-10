package sqlsplit

import (
	"strings"
	"testing"
)

// TestBodySplit is N-248: a statement whose body holds semicolons is one
// statement. The first case is the item's repro — the scan used to cut
// it into `CREATE TRIGGER … BEGIN DELETE FROM order_items …` and a bare
// `END`.
func TestBodySplit(t *testing.T) {
	trigger := "CREATE TRIGGER del_items AFTER DELETE ON orders BEGIN\n" +
		"  DELETE FROM order_items WHERE order_id = old.id;\n" +
		"  UPDATE stats SET n = n - 1;\n" +
		"END"
	proc := "CREATE PROCEDURE p(IN n INT, IN begin INT)\nBEGIN\n" +
		"  DECLARE done INT DEFAULT 0;\n" +
		"  DECLARE CONTINUE HANDLER FOR NOT FOUND BEGIN SET done = 1; END;\n" +
		// a parameter called begin, used bare: a name, not a block
		"  IF begin > 0 THEN\n    SELECT 1;\n  ELSE\n    SELECT IF(done, 1, 2);\n  END IF;\n" +
		"  lbl: LOOP\n    LEAVE lbl;\n  END LOOP lbl;\n" +
		"  CASE n WHEN 1 THEN SELECT 'one'; ELSE BEGIN END; END CASE;\n" +
		// an IF inside a CASE statement: END IF does not close the CASE
		"  CASE WHEN n > 1 THEN IF done THEN SELECT 2; END IF; ELSE SELECT 3; END CASE;\n" +
		"  WHILE done = 0 DO SET done = 1; END WHILE;\n" +
		"  REPEAT SET done = done + 1; UNTIL done > 3 END REPEAT;\n" +
		"  inner_block: BEGIN SELECT 4; END inner_block;\n" +
		// a column called end: not the body's END
		"  INSERT INTO log (start, end) VALUES (1, 2);\n" +
		"  SELECT start, end FROM periods;\n" +
		"  UPDATE periods p SET p.end = NOW();\n" +
		"END"
	atomic := "CREATE FUNCTION f(a int) RETURNS int LANGUAGE sql\nBEGIN ATOMIC\n" +
		"  INSERT INTO t VALUES (a);\n" +
		"  SELECT CASE WHEN a > 0 THEN 1 ELSE 0 END;\n" +
		"END"
	cases := []struct {
		name string
		d    Dialect
		sql  string
		want []string
	}{
		{"sqlite trigger", SQLite, trigger + ";\nSELECT 1;", []string{trigger, "SELECT 1"}},
		{"sqlite trigger with a case", SQLite,
			"CREATE TEMP TRIGGER t AFTER INSERT ON a WHEN CASE WHEN new.y THEN 1 END BEGIN UPDATE b SET x = CASE WHEN new.y > 0 THEN 1 ELSE 0 END; INSERT INTO c VALUES (new.id); END; SELECT 2",
			[]string{"CREATE TEMP TRIGGER t AFTER INSERT ON a WHEN CASE WHEN new.y THEN 1 END BEGIN UPDATE b SET x = CASE WHEN new.y > 0 THEN 1 ELSE 0 END; INSERT INTO c VALUES (new.id); END", "SELECT 2"}},
		{"sqlite trigger if not exists", SQLite,
			"CREATE TRIGGER IF NOT EXISTS t AFTER INSERT ON a BEGIN SELECT 1; SELECT 2; END; SELECT 3",
			[]string{"CREATE TRIGGER IF NOT EXISTS t AFTER INSERT ON a BEGIN SELECT 1; SELECT 2; END", "SELECT 3"}},
		{"mysql procedure", MySQL, proc + ";\nSELECT 3;", []string{proc, "SELECT 3"}},
		{"mysql trigger with definer", MySQL,
			"CREATE DEFINER=`root`@`localhost` TRIGGER t BEFORE INSERT ON a FOR EACH ROW BEGIN SET NEW.x = 1; SET NEW.y = 2; END; SELECT 2",
			[]string{"CREATE DEFINER=`root`@`localhost` TRIGGER t BEFORE INSERT ON a FOR EACH ROW BEGIN SET NEW.x = 1; SET NEW.y = 2; END", "SELECT 2"}},
		{"mysql unquoted definer", MySQL,
			"CREATE DEFINER = admin@localhost PROCEDURE p() BEGIN SELECT 1; END; SELECT 2",
			[]string{"CREATE DEFINER = admin@localhost PROCEDURE p() BEGIN SELECT 1; END", "SELECT 2"}},
		{"mysql current_user definer", MySQL,
			"CREATE OR REPLACE DEFINER=CURRENT_USER() FUNCTION f() RETURNS INT BEGIN RETURN 1; END; SELECT 2",
			[]string{"CREATE OR REPLACE DEFINER=CURRENT_USER() FUNCTION f() RETURNS INT BEGIN RETURN 1; END", "SELECT 2"}},
		{"mysql one-statement trigger", MySQL,
			"CREATE TRIGGER t BEFORE INSERT ON a FOR EACH ROW SET NEW.x = 1; SELECT 2",
			[]string{"CREATE TRIGGER t BEFORE INSERT ON a FOR EACH ROW SET NEW.x = 1", "SELECT 2"}},
		{"mysql event", MySQL,
			"CREATE EVENT e ON SCHEDULE EVERY 1 DAY DO BEGIN DELETE FROM x WHERE a < NOW(); OPTIMIZE TABLE x; END; SELECT 2",
			[]string{"CREATE EVENT e ON SCHEDULE EVERY 1 DAY DO BEGIN DELETE FROM x WHERE a < NOW(); OPTIMIZE TABLE x; END", "SELECT 2"}},
		{"postgres begin atomic", Postgres, atomic + ";\nSELECT 2;", []string{atomic, "SELECT 2"}},
		{"postgres procedure", Postgres,
			"CREATE OR REPLACE PROCEDURE p() LANGUAGE sql BEGIN ATOMIC INSERT INTO t VALUES (1); END; CALL p();",
			[]string{"CREATE OR REPLACE PROCEDURE p() LANGUAGE sql BEGIN ATOMIC INSERT INTO t VALUES (1); END", "CALL p()"}},
		{"postgres rule", Postgres,
			"CREATE RULE r AS ON INSERT TO t DO ALSO (INSERT INTO a VALUES (1); INSERT INTO b VALUES (2)); SELECT 1",
			[]string{"CREATE RULE r AS ON INSERT TO t DO ALSO (INSERT INTO a VALUES (1); INSERT INTO b VALUES (2))", "SELECT 1"}},
		{"postgres dollar body", Postgres,
			"CREATE FUNCTION g() RETURNS trigger AS $$ BEGIN RETURN NEW; END; $$ LANGUAGE plpgsql; SELECT 1",
			[]string{"CREATE FUNCTION g() RETURNS trigger AS $$ BEGIN RETURN NEW; END; $$ LANGUAGE plpgsql", "SELECT 1"}},
		{"generic trigger", Generic, trigger + ";\nSELECT 1;", []string{trigger, "SELECT 1"}},

		// statements with no body split as they always did
		{"transaction", Postgres, "BEGIN; UPDATE t SET a = 1; COMMIT;", []string{"BEGIN", "UPDATE t SET a = 1", "COMMIT"}},
		{"case outside a body", MySQL, "SELECT CASE WHEN a THEN 1 END FROM t; SELECT 2",
			[]string{"SELECT CASE WHEN a THEN 1 END FROM t", "SELECT 2"}},
		{"table named function", Postgres, "CREATE TABLE function (a int); SELECT 1",
			[]string{"CREATE TABLE function (a int)", "SELECT 1"}},
		{"definer view", MySQL, "CREATE DEFINER = a@b SQL SECURITY INVOKER VIEW v AS SELECT 1; SELECT 2",
			[]string{"CREATE DEFINER = a@b SQL SECURITY INVOKER VIEW v AS SELECT 1", "SELECT 2"}},
		{"parens outside a rule", Postgres, "SELECT count(*) FROM t; SELECT (1); SELECT 2",
			[]string{"SELECT count(*) FROM t", "SELECT (1)", "SELECT 2"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			eq(t, texts(c.d.Split(c.sql)), c.want)
		})
	}
}

// TestBodyCaret: a caret on a body line picks the whole CREATE — before,
// it picked the inner statement alone, which ran it for real.
func TestBodyCaret(t *testing.T) {
	sql := "SELECT 0;\nCREATE TRIGGER t AFTER DELETE ON o BEGIN\n  DELETE FROM i WHERE oid = old.id;\n  DELETE FROM j;\nEND;\nSELECT 9;"
	stmts := SQLite.Split(sql)
	if len(stmts) != 3 {
		t.Fatalf("SQLite.Split = %q, want 3 statements", texts(stmts))
	}
	for _, at := range []string{"DELETE FROM i", "DELETE FROM j", "END;"} {
		if i := IndexAt(stmts, strings.Index(sql, at)); i != 1 {
			t.Errorf("caret at %q picks statement %d (%q), want the trigger", at, i, stmts[i].Text)
		}
	}
}

// TestDelimiter: MySQL's client command, which the dumps and every MySQL
// tutorial use around routines. Its lines are the client's, never sent;
// between them only the new delimiter ends a statement.
func TestDelimiter(t *testing.T) {
	cases := []struct {
		name string
		d    Dialect
		sql  string
		want []string
	}{
		{"slashes", MySQL,
			"DELIMITER //\nCREATE PROCEDURE p()\nBEGIN\n  SELECT 1;\nEND //\nDELIMITER ;\nSELECT 2;",
			[]string{"CREATE PROCEDURE p()\nBEGIN\n  SELECT 1;\nEND", "SELECT 2"}},
		// $$ is the delimiter here, not a quote
		{"dollars", MySQL,
			"delimiter $$\nCREATE FUNCTION f() RETURNS INT\nBEGIN\n  RETURN 1;\nEND$$\ndelimiter ;\nSELECT f();",
			[]string{"CREATE FUNCTION f() RETURNS INT\nBEGIN\n  RETURN 1;\nEND", "SELECT f()"}},
		// mysqldump's triggers: ;; around executable comments
		{"mysqldump", MySQL,
			"DELIMITER ;;\n/*!50003 CREATE*/ /*!50017 DEFINER=`root`@`localhost`*/ /*!50003 TRIGGER t BEFORE INSERT ON a FOR EACH ROW BEGIN\n  SET NEW.x = 1;\nEND */;;\nDELIMITER ;\nSELECT 1;",
			[]string{"/*!50003 CREATE*/ /*!50017 DEFINER=`root`@`localhost`*/ /*!50003 TRIGGER t BEFORE INSERT ON a FOR EACH ROW BEGIN\n  SET NEW.x = 1;\nEND */", "SELECT 1"}},
		// a ; is plain text under another delimiter
		{"two under one", MySQL, "DELIMITER //\nSELECT 1; SELECT 2 //\nSELECT 3 //\nDELIMITER ;",
			[]string{"SELECT 1; SELECT 2", "SELECT 3"}},
		{"headed by a comment", MySQL, "-- make p\nDELIMITER //\nCREATE PROCEDURE p() BEGIN SELECT 1; END //",
			[]string{"CREATE PROCEDURE p() BEGIN SELECT 1; END"}},
		// not a command: after code on its line, inside a statement, with
		// nothing after it, or on another engine
		{"after code on its line", MySQL, "SELECT 1; DELIMITER //\nSELECT 2;",
			[]string{"SELECT 1", "DELIMITER //\nSELECT 2"}},
		{"inside a statement", MySQL, "SELECT 1\nDELIMITER //\n;",
			[]string{"SELECT 1\nDELIMITER //"}},
		{"no delimiter given", MySQL, "DELIMITER\nSELECT 1;", []string{"DELIMITER\nSELECT 1"}},
		{"postgres", Postgres, "DELIMITER //\nSELECT 1;", []string{"DELIMITER //\nSELECT 1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			eq(t, texts(c.d.Split(c.sql)), c.want)
		})
	}

	// a caret on a DELIMITER line belongs to the statement below it, as
	// one on a comment between statements does
	sql := "SELECT 1;\nDELIMITER //\nSELECT 2 //"
	stmts := MySQL.Split(sql)
	if i := IndexAt(stmts, strings.Index(sql, "DELIMITER")); i != 1 {
		t.Errorf("caret on the DELIMITER line picks statement %d, want 1", i)
	}
	// Start, End and CodeStart stay offsets of the buffer, the delimiter
	// past End
	if s := stmts[1]; sql[s.Start:s.End] != "SELECT 2" || s.CodeStart != s.Start {
		t.Errorf("statement 2 spans %q (CodeStart %d, Start %d)", sql[s.Start:s.End], s.CodeStart, s.Start)
	}
}

// TestBodyFolds: a trigger folds as the one statement it runs as.
func TestBodyFolds(t *testing.T) {
	sql := "CREATE TRIGGER t AFTER DELETE ON o BEGIN\n  DELETE FROM i;\n  DELETE FROM j;\nEND;"
	folds := SQLite.Folds(sql)
	if len(folds) != 1 || folds[0].Start != 0 {
		t.Errorf("SQLite.Folds = %v, want one fold from the CREATE", folds)
	}
}

// TestBodyConstructs puts each construct the block count must get right in
// a procedure of its own, so one misread cannot cancel another out: a
// block closed early cuts the body, one never closed swallows SELECT 3.
func TestBodyConstructs(t *testing.T) {
	for _, inner := range []string{
		"CASE n WHEN 1 THEN SELECT 1; ELSE BEGIN END; END CASE;", // CASE statement, empty block
		"CASE WHEN n THEN IF d THEN SELECT 1; END IF; END CASE;", // END IF inside a CASE
		"IF begin > 0 THEN SELECT 1; END IF;",                    // a name called begin
		"SET begin = 1, x = begin;",                              // and again
		"INSERT INTO log (start, end) VALUES (1, 2);",            // a column called end, in parens
		"SELECT start, end FROM periods;",                        // and bare
		"SELECT p.end FROM periods p;",                           // and qualified
		"lbl: LOOP LEAVE lbl; END LOOP lbl;",                     // a labelled loop
		"b: BEGIN SELECT 1; END b;",                              // a labelled block
		"WHILE x DO SET x = 0; END WHILE;",                       //
		"REPEAT SET x = x + 1; UNTIL x > 3 END REPEAT;",          //
		"FOR i IN 1..3 DO SELECT i; END FOR;",                    // MariaDB's FOR
		"SELECT a FROM t WHERE x = CASE WHEN 1 THEN 2 END FOR UPDATE;",
		"SELECT CASE WHEN a THEN 1 END AS c, (CASE b WHEN 1 THEN 2 END) FROM t;",
		"BEGIN /* nested */ SELECT 1; END; -- a remark\n",
	} {
		proc := "CREATE PROCEDURE p() BEGIN\n  " + inner + "\n  SELECT 2;\nEND"
		eq(t, texts(MySQL.Split(proc+";\nSELECT 3;")), []string{proc, "SELECT 3"})
	}
}

// TestBodyFormat: formatting keeps what the scan reads. The layout knows
// no DELIMITER, so a script using it is formatted around it — the command
// lines and the routines under them as written, the ; stretches laid out
// — where it used to space `//` out and join `DELIMITER ;` onto the END.
// A body statement is laid out as statements (it is not a procedural
// formatter) but still splits as the one statement it was.
func TestBodyFormat(t *testing.T) {
	script := "select 1;\nDELIMITER //\nCREATE PROCEDURE p()\nBEGIN\n  SELECT 1;\nEND //\nDELIMITER ;\nselect 2;"
	want := "SELECT 1;\nDELIMITER //\nCREATE PROCEDURE p()\nBEGIN\n  SELECT 1;\nEND //\nDELIMITER ;\nSELECT 2;"
	if got := MySQL.Format(script); got != want {
		t.Errorf("MySQL.Format:\n%s\nwant\n%s", got, want)
	}
	// FormatSpan, the editors' entry, keeps the blanks around it too
	if got := MySQL.FormatSpan("\n" + script + "\n"); got != "\n"+want+"\n" {
		t.Errorf("MySQL.FormatSpan:\n%q\nwant\n%q", got, "\n"+want+"\n")
	}
	// $$ is the delimiter, and the leading DELIMITER line is no log prose
	// for Unwrap to cut
	dollars := "delimiter $$\nCREATE FUNCTION f() RETURNS INT\nBEGIN\n  RETURN 1;\nEND$$\ndelimiter ;"
	if got := MySQL.Format(dollars); got != dollars {
		t.Errorf("MySQL.Format:\n%s\nwant it unchanged", got)
	}

	for _, c := range []struct {
		d   Dialect
		sql string
	}{
		{SQLite, "create trigger t after delete on o begin delete from i where oid = old.id; update s set n = n - 1; end;\nselect 1;"},
		{Postgres, "create function f(a int) returns int language sql begin atomic insert into t values (a); select case when a > 0 then 1 else 0 end; end;\nselect 2;"},
		{Postgres, "create rule r as on insert to t do also (insert into a values (1); insert into b values (2)); select 1"},
	} {
		before := len(c.d.Split(c.sql))
		if after := c.d.Split(c.d.Format(c.sql)); len(after) != before {
			t.Errorf("%v: %d statements before Format, %d after: %q", c.d, before, len(after), texts(after))
		}
	}
}
