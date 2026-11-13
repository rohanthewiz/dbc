package config

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
)

// TestExpandDSNDollarEscape: "$$" is a literal "$" in every DSN shape, as
// the driver reads it, beside a real ${VAR} and with no warning; a lone
// "$" before a name is still a reference, and its warning names the escape.
// S3cr$t9 is the password from the report: before, it lost "$t9" to a
// lookup, and S3cr$$t9 lost "$$" too.
func TestExpandDSNDollarEscape(t *testing.T) {
	t.Setenv("DBC_T_USER", "app")

	pgKV, warns := ExpandDSN("c", "postgres", `host=h user=${DBC_T_USER} password='S3cr$$t9'`)
	if len(warns) != 0 {
		t.Fatalf("pg keyword warnings %q", warns)
	}
	pc, err := pgconn.ParseConfig(pgKV)
	if err != nil || pc.Password != "S3cr$t9" || pc.User != "app" {
		t.Fatalf("pg keyword %s: password %q user %q err %v", pgKV, pc.Password, pc.User, err)
	}

	pgURL, warns := ExpandDSN("c", "postgres", `postgres://${DBC_T_USER}:S3cr$$t9@h/d`)
	if len(warns) != 0 {
		t.Fatalf("pg url warnings %q", warns)
	}
	pc, err = pgconn.ParseConfig(pgURL)
	if err != nil || pc.Password != "S3cr$t9" || pc.User != "app" {
		t.Fatalf("pg url %s: password %q user %q err %v", pgURL, pc.Password, pc.User, err)
	}

	my, warns := ExpandDSN("c", "mysql", `${DBC_T_USER}:S3cr$$t9@tcp(h:3306)/d`)
	if len(warns) != 0 {
		t.Fatalf("mysql warnings %q", warns)
	}
	mc, err := mysql.ParseDSN(my)
	if err != nil || mc.Passwd != "S3cr$t9" || mc.User != "app" {
		t.Fatalf("mysql %s: password %q user %q err %v", my, mc.Passwd, mc.User, err)
	}

	// "$$" right before a reference: the literal, then the value
	if out, _ := ExpandDSN("c", "sqlite", `a$$${DBC_T_USER}b$$$$`); out != `a$appb$$` {
		t.Fatalf("$$ before a reference: %q", out)
	}

	// the unescaped password: still a reference, and the warning says how
	// to write the "$" that was meant
	_, warns = ExpandDSN("pg", "postgres", `password='S3cr$t9'`)
	if len(warns) != 1 || !strings.Contains(warns[0], "$t9") || !strings.Contains(warns[0], "write $$ for a literal $") {
		t.Fatalf("unescaped warnings %q", warns)
	}
}

// "$$" works in the path keys too — tls_* and the *_dir / pg_bin ones — and
// their unset-variable warnings name it as the DSN's does.
func TestExpandPathsDollarEscape(t *testing.T) {
	base := t.TempDir()
	tl, warns := ExpandTLS("c", TLSOpts{TLS: "verify-full", TLSCA: "ca$$1.pem"}, base)
	if len(warns) != 0 || tl.TLSCA != filepath.Join(base, "ca$1.pem") {
		t.Fatalf("tls_ca = %q, warnings %q", tl.TLSCA, warns)
	}
	_, warns = ExpandTLS("c", TLSOpts{TLS: "verify-full", TLSCA: "ca$x.pem"}, base)
	if len(warns) != 1 || !strings.Contains(warns[0], "write $$ for a literal $") {
		t.Fatalf("tls_ca unset warnings %q", warns)
	}

	p, warns := resolvePath("pg_bin", "bin$$old", base)
	if len(warns) != 0 || p != filepath.Join(base, "bin$old") {
		t.Fatalf("pg_bin = %q, warnings %q", p, warns)
	}
	_, warns = resolvePath("pg_bin", "bin$old", base)
	if len(warns) != 1 || !strings.Contains(warns[0], "pg_bin references unset env var $old") ||
		!strings.Contains(warns[0], "write $$ for a literal $") {
		t.Fatalf("pg_bin unset warnings %q", warns)
	}
}

// TestLoadDollarEscape: the whole path, a config file through Load.
func TestLoadDollarEscape(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
[[connection]]
name = "pg"
driver = "postgres"
dsn = "host=h user=app password='S3cr$$t9'"
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Warnings) != 0 || !strings.Contains(cfg.Connections[0].DSN, "password='S3cr$t9'") {
		t.Fatalf("dsn %q, warnings %q", cfg.Connections[0].DSN, cfg.Warnings)
	}
}
