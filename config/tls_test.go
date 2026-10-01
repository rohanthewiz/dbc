package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTLSOptsCheck(t *testing.T) {
	for _, tc := range []struct {
		in   TLSOpts
		want string // "" = accepted
	}{
		{TLSOpts{}, ""},
		{TLSOpts{TLS: " Verify-Full "}, ""},
		{TLSOpts{TLS: "require"}, ""},
		{TLSOpts{TLS: "verify-ca", TLSCA: "ca.pem"}, ""},
		{TLSOpts{TLS: "verify-full", TLSCert: "c.pem", TLSKey: "c.key"}, ""},
		{TLSOpts{TLS: "verify_full"}, `unknown tls mode "verify_full"`},
		{TLSOpts{TLS: "true"}, "unknown tls mode"},
		{TLSOpts{TLSCA: "ca.pem"}, "need a tls mode"},
		{TLSOpts{TLS: "disable", TLSCA: "ca.pem"}, "never reads"},
		{TLSOpts{TLS: "require", TLSCert: "c.pem"}, "go together"},
		{TLSOpts{TLS: "require", TLSKey: "c.key"}, "go together"},
	} {
		in := tc.in
		err := in.Check()
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%+v: %v", tc.in, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%+v: got %v, want an error mentioning %q", tc.in, err, tc.want)
		}
	}
	// the mode is normalized in place, as Load and the web form rely on
	o := TLSOpts{TLS: " Verify-Full "}
	if err := o.Check(); err != nil || o.TLS != "verify-full" {
		t.Errorf("normalized to %q, %v", o.TLS, err)
	}
}

func TestExpandTLS(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	t.Setenv("DBC_TEST_CERTS", "/etc/certs")
	got, warns := ExpandTLS("c", TLSOpts{TLS: "verify-full", TLSCA: "${DBC_TEST_CERTS}/ca.pem",
		TLSCert: "~/me.pem", TLSKey: "keys/me.key"}, "/cfg")
	want := TLSOpts{TLS: "verify-full", TLSCA: "/etc/certs/ca.pem",
		TLSCert: filepath.Join(home, "me.pem"), TLSKey: filepath.Join("/cfg", "keys/me.key")}
	if got != want || len(warns) != 0 {
		t.Fatalf("got %+v, %q\nwant %+v", got, warns, want)
	}

	// no base: a relative path stays relative (the --tls-* flags)
	if got, _ = ExpandTLS("c", TLSOpts{TLSCA: "ca.pem"}, ""); got.TLSCA != "ca.pem" {
		t.Errorf("no base: %q", got.TLSCA)
	}
	// an unset variable is named, with the key it was in
	_, warns = ExpandTLS("c", TLSOpts{TLSCA: "${DBC_TEST_SURELY_UNSET_TLS}/ca.pem"}, "/cfg")
	if len(warns) != 1 || !strings.Contains(warns[0], "tls_ca") || !strings.Contains(warns[0], "DBC_TEST_SURELY_UNSET_TLS") {
		t.Errorf("warnings %q", warns)
	}
}

// TestLoadTLS: the keys sit flat beside dsn, paths come out relative to the
// config file, and a bad setting fails the load instead of connecting with
// less than was asked for.
func TestLoadTLS(t *testing.T) {
	path := writeConfig(t, `
[[connection]]
name = "prod"
driver = "postgres"
dsn = "postgres://app@db/app"
tls = "VERIFY-FULL"
tls_ca = "certs/ca.pem"
tls_cert = "/abs/me.pem"
tls_key = "/abs/me.key"

[[connection]]
name = "plain"
driver = "mysql"
dsn = "u@tcp(h)/d"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	prod, _ := cfg.ConnByName("prod")
	want := TLSOpts{TLS: "verify-full", TLSCA: filepath.Join(filepath.Dir(path), "certs", "ca.pem"),
		TLSCert: "/abs/me.pem", TLSKey: "/abs/me.key"}
	if prod.TLSOpts != want {
		t.Fatalf("prod TLS = %+v\nwant %+v", prod.TLSOpts, want)
	}
	if plain, _ := cfg.ConnByName("plain"); plain.TLSOpts.Set() {
		t.Fatalf("plain has TLS settings: %+v", plain.TLSOpts)
	}

	bad := writeConfig(t, `
[[connection]]
name = "typo"
driver = "postgres"
dsn = "postgres://h/app"
tls = "verify_full"
`)
	if _, err = Load(bad); err == nil || !strings.Contains(err.Error(), "unknown tls mode") {
		t.Fatalf("a bad tls mode loaded: %v", err)
	}
}

// TestSavedTLS: a saved entry keeps its TLS keys as typed, writes none when
// it has none (so older entries' text does not change), and one with a bad
// setting is skipped at merge rather than merged without TLS.
func TestSavedTLS(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connections.toml")
	st := OpenSaved(path)
	must(t, st.Add(SavedConn{Name: "secure", Driver: "postgres", DSN: "host=h",
		TLSOpts: TLSOpts{TLS: "verify-full", TLSCA: "${DBC_TEST_CA_DIR}/ca.pem"}}))
	must(t, st.Add(SavedConn{Name: "plain", Driver: "sqlite", DSN: "file:x"}))
	must(t, st.Add(SavedConn{Name: "broken", Driver: "postgres", DSN: "host=h",
		TLSOpts: TLSOpts{TLS: "verify_full"}}))

	bs, err := os.ReadFile(path)
	must(t, err)
	text := string(bs)
	if !strings.Contains(text, `tls_ca = "${DBC_TEST_CA_DIR}/ca.pem"`) {
		t.Errorf("tls_ca not kept as typed:\n%s", text)
	}
	if strings.Count(text, "tls = ") != 2 || strings.Contains(text, `tls_cert =`) {
		t.Errorf("empty TLS keys written:\n%s", text)
	}

	t.Setenv("DBC_TEST_CA_DIR", "/etc/dbc")
	c := &Config{}
	c.LoadSaved(path, nil)
	if got := strings.Join(names(c.Connections), ","); got != "secure,plain" {
		t.Fatalf("merged %s", got)
	}
	if cn, _ := c.ConnByName("secure"); cn.TLS != "verify-full" || cn.TLSCA != "/etc/dbc/ca.pem" {
		t.Fatalf("secure = %+v", cn.TLSOpts)
	}
	if w := strings.Join(c.Warnings, "\n"); !strings.Contains(w, `"broken" skipped: unknown tls mode`) {
		t.Fatalf("warnings:\n%s", w)
	}
}
