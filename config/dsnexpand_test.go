package config

import (
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
)

// hardValues are environment values each DSN shape has a character to trip
// over: libpq's quotes (' \), whitespace, a URL's delimiters (@ : / ? # & =
// +) and its escape (%), and go-sql-driver's split points. A '%' followed
// by two hex digits is not among them: in a URL or MySQL value that is
// taken as already encoded (see TestExpandDSNKeepsPreEncoded).
var hardValues = []string{
	`it's`, `back\slash`, `sp ace`, `p@ss`, `a:b`, `w/rd`, `100%`, `50%_off`, `%zz`,
	`#?&=+`, `all of it: ' \ @/?#&=+% "x"`, `plain`,
}

// TestExpandDSNKeepsPreEncoded: a value kept percent-encoded in the
// environment — the one way to get p@ss through a URL before ExpandDSN
// escaped anything — is left exactly as it was, and still reads as p@ss.
// A config that worked before must not break.
func TestExpandDSNKeepsPreEncoded(t *testing.T) {
	t.Setenv("DBC_T_PW", "p%40ss")

	url := `postgres://u:${DBC_T_PW}@h/d?application_name=${DBC_T_PW}`
	out, _ := ExpandDSN("c", "postgres", url)
	if want := `postgres://u:p%40ss@h/d?application_name=p%40ss`; out != want {
		t.Fatalf("url\n got %s\nwant %s", out, want)
	}
	cfg, err := pgconn.ParseConfig(out)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Password != "p@ss" || cfg.RuntimeParams["application_name"] != "p@ss" {
		t.Fatalf("url: password %q app %q", cfg.Password, cfg.RuntimeParams["application_name"])
	}

	my := `u:pw@tcp(h:3306)/d?x=${DBC_T_PW}`
	out, _ = ExpandDSN("c", "mysql", my)
	if want := `u:pw@tcp(h:3306)/d?x=p%40ss`; out != want {
		t.Fatalf("mysql\n got %s\nwant %s", out, want)
	}
	mc, err := mysql.ParseDSN(out)
	if err != nil {
		t.Fatal(err)
	}
	if mc.Params["x"] != "p@ss" {
		t.Fatalf("mysql: x %q", mc.Params["x"])
	}

	// a bare % next to an escape: only the bare one is encoded
	t.Setenv("DBC_T_PW", "50%_p%40ss")
	out, _ = ExpandDSN("c", "postgres", `postgres://u:${DBC_T_PW}@h/d`)
	if cfg, err = pgconn.ParseConfig(out); err != nil || cfg.Password != "50%_p@ss" {
		t.Fatalf("%s: password %q, %v", out, cfg.Password, err)
	}
}

// TestExpandDSNEscapesForTheDriver checks each shape by what the driver's
// own parser makes of the expanded DSN: the value must come back exactly as
// the environment holds it, whatever it holds.
func TestExpandDSNEscapesForTheDriver(t *testing.T) {
	for _, v := range hardValues {
		t.Run(v, func(t *testing.T) {
			t.Setenv("DBC_T_PW", v)

			pg := func(dsn string) *pgconn.Config {
				t.Helper()
				out, warns := ExpandDSN("c", "postgres", dsn)
				if len(warns) > 0 {
					t.Fatalf("warnings %q", warns)
				}
				cfg, err := pgconn.ParseConfig(out)
				if err != nil {
					t.Fatalf("%s → %s: %v", dsn, out, err)
				}
				return cfg
			}

			// the field form's own shape (db.BuildDSN): password quoted
			if c := pg(`host=h user=u password='${DBC_T_PW}' dbname=d`); c.Password != v || c.Database != "d" || c.User != "u" {
				t.Errorf("quoted: password %q database %q user %q", c.Password, c.Database, c.User)
			}
			// hand-written, bare: re-quoted when the value needs it
			if c := pg(`host=h password=$DBC_T_PW dbname=d`); c.Password != v || c.Database != "d" {
				t.Errorf("bare: password %q database %q", c.Password, c.Database)
			}
			// bare, with typed text (and a typed escape) around the reference
			if c := pg(`host=h password=x\'${DBC_T_PW}y dbname=d`); c.Password != "x'"+v+"y" || c.Database != "d" {
				t.Errorf("bare mixed: password %q database %q", c.Password, c.Database)
			}
			// a URL: userinfo, path and a query value percent-encoded
			c := pg(`postgres://u:${DBC_T_PW}@h:5433/${DBC_T_PW}?sslmode=disable&application_name=${DBC_T_PW}`)
			if c.Password != v || c.Database != v || c.Port != 5433 || c.RuntimeParams["application_name"] != v {
				t.Errorf("url: password %q database %q port %d app %q", c.Password, c.Database, c.Port, c.RuntimeParams["application_name"])
			}

			// MySQL: the password raw, the database and a param encoded
			out, _ := ExpandDSN("c", "mysql", `u:${DBC_T_PW}@tcp(h:3306)/${DBC_T_PW}?parseTime=true&x=${DBC_T_PW}`)
			mc, err := mysql.ParseDSN(out)
			if err != nil {
				t.Fatalf("mysql %s: %v", out, err)
			}
			if mc.Passwd != v || mc.DBName != v || mc.Params["x"] != v || mc.Addr != "h:3306" || !mc.ParseTime {
				t.Errorf("mysql: passwd %q db %q x %q addr %q", mc.Passwd, mc.DBName, mc.Params["x"], mc.Addr)
			}
		})
	}
}

// TestExpandDSNLeavesTheUsersTextAlone: where a reference is the DSN's own
// text — a whole DSN, a keyword list, a host, a path — it is pasted as is,
// as plain expansion always did.
func TestExpandDSNLeavesTheUsersTextAlone(t *testing.T) {
	t.Setenv("DBC_T_WHOLE", "postgres://u:p@h/d?sslmode=disable")
	t.Setenv("DBC_T_OPTS", "host=h sslmode=disable")
	t.Setenv("DBC_T_HOST", "h1,h2")
	t.Setenv("DBC_T_DIR", "/tmp/my dir's")
	t.Setenv("DBC_T_SIMPLE", "secret")
	t.Setenv("DBC_T_MYDSN", "u:p@tcp(h)/d")
	for _, tc := range []struct{ driver, in, want string }{
		{"postgres", "${DBC_T_WHOLE}", "postgres://u:p@h/d?sslmode=disable"},
		{"postgres", "${DBC_T_OPTS} dbname=d", "host=h sslmode=disable dbname=d"},
		{"postgres", "postgres://u@${DBC_T_HOST}:5432/d", "postgres://u@h1,h2:5432/d"},
		{"postgres", "host=h password=${DBC_T_SIMPLE}", "host=h password=secret"},
		{"mysql", "${DBC_T_MYDSN}", "u:p@tcp(h)/d"},
		{"sqlite", "${DBC_T_DIR}/x.db", "/tmp/my dir's/x.db"},
		{"bytdb", "$DBC_T_DIR/x.bytdb", "/tmp/my dir's/x.bytdb"},
		{"", "u:${DBC_T_SIMPLE}@h", "u:secret@h"},
		{"postgres", "host=h dbname=d", "host=h dbname=d"},
	} {
		if got, _ := ExpandDSN("c", tc.driver, tc.in); got != tc.want {
			t.Errorf("%s %q → %q, want %q", tc.driver, tc.in, got, tc.want)
		}
	}
}

// TestExpandDSNUnsetBare: an unset variable as a bare value expands to
// empty; quoted, so the next pair is not taken for its value.
func TestExpandDSNUnsetBare(t *testing.T) {
	out, warns := ExpandDSN("c", "postgres", "password=${DBC_T_SURELY_UNSET} host=h")
	if len(warns) != 1 {
		t.Fatalf("warnings %q", warns)
	}
	cfg, err := pgconn.ParseConfig(out)
	if err != nil {
		t.Fatalf("%s: %v", out, err)
	}
	if cfg.Password != "" || cfg.Host != "h" {
		t.Fatalf("%s: password %q host %q", out, cfg.Password, cfg.Host)
	}
}
