package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/rohanthewiz/serr"
)

// TLS for a connection, set the same way for every server engine.
//
// WHY KEYS OF dbc'S OWN, WHEN A DSN CAN ALREADY ASK FOR TLS. A Postgres DSN
// can (sslmode, sslrootcert, sslcert, sslkey), but a MySQL one cannot name a
// CA file or a client certificate at all: go-sql-driver/mysql takes those
// only as a *tls.Config registered in code. And the two spell the same
// choice differently (sslmode=verify-full against tls=true). The keys below
// are one spelling for both, and the db package turns them into each
// driver's own settings when it opens the pool (db/tls.go):
//
//	[[connection]]                       postgres: DSN params sslmode,
//	tls      = "verify-full"     ──►               sslrootcert, sslcert, sslkey
//	tls_ca   = "~/certs/ca.pem"          mysql:    a *tls.Config on the
//	tls_cert = "~/certs/me.pem"                    connector (CA pool, client
//	tls_key  = "~/certs/me.key"                    cert, ServerName)
//	tls_key_password = "${ME_KEY_PASS}"
//
// Unset (tls = "") leaves TLS to the DSN, exactly as before these keys
// existed. Set, they win over whatever the DSN says: a config line that asks
// for verify-full must not be quietly undone by a sslmode=disable left in a
// copied URL.
//
// THE MODES are libpq's sslmode names, the most widely known spelling, with
// libpq's meaning on both engines:
//
//	mode         encrypted   server cert checked     falls back to plaintext
//	disable      no          —                       —
//	prefer       if offered  no                      yes, if the server has no TLS
//	require      yes         no (chain only if CA)   no
//	verify-ca    yes         chain                   no
//	verify-full  yes         chain + host name       no
//
// "require" with a tls_ca verifies the chain, as libpq does: naming a CA is
// asking for it to be checked. With no tls_ca, verify-ca and verify-full
// check against the system's trust store — right for a managed database
// whose certificate a public CA signed.
//
// Only verify-full stops a man in the middle: the others either check
// nothing or accept any certificate the CA ever signed, for any host.
//
// AN ENCRYPTED tls_key. tls_key_password names the environment variable
// holding the key's passphrase — "${VAR}" or "$VAR", and nothing else. A
// literal passphrase is refused: config.toml and the saved-connections file
// are plain text, and dbc web sends TLSOpts to the browser (they are paths,
// not secrets — so the reference may go there, the passphrase must not).
// The reference is kept as written through ExpandTLS, and the db package
// reads the variable only when it opens the pool (db/tlskey.go), so the
// passphrase never sits in a Connection, a JSON response or a log line.

// TLS modes, as written in the tls key.
const (
	TLSDisable    = "disable"
	TLSPrefer     = "prefer"
	TLSRequire    = "require"
	TLSVerifyCA   = "verify-ca"
	TLSVerifyFull = "verify-full"
)

// TLSModes lists the modes in order of strictness, for messages and the
// web form's select.
var TLSModes = []string{TLSDisable, TLSPrefer, TLSRequire, TLSVerifyCA, TLSVerifyFull}

// TLSOpts is a connection's TLS settings. It is embedded (untagged) in
// Connection and SavedConn, so the TOML keys sit flat beside dsn in both
// files — a saved entry can still be copied into config.toml as it is. The
// JSON tags are dbc web's: its connection form and list embed it the same
// way. None of it is secret (paths, not the keys themselves), so unlike the
// DSN it does go back to the browser.
type TLSOpts struct {
	TLS     string `toml:"tls,omitempty" json:"tls,omitempty"`           // a mode above; "" leaves TLS to the DSN
	TLSCA   string `toml:"tls_ca,omitempty" json:"tls_ca,omitempty"`     // PEM file of CA certificates to trust
	TLSCert string `toml:"tls_cert,omitempty" json:"tls_cert,omitempty"` // PEM client certificate, for servers that ask for one
	TLSKey  string `toml:"tls_key,omitempty" json:"tls_key,omitempty"`   // its PEM private key
	// TLSKeyPassword is "${VAR}" (or "$VAR"): the environment variable
	// holding tls_key's passphrase, when the key is encrypted. The
	// reference, never the passphrase — see "AN ENCRYPTED tls_key" above.
	TLSKeyPassword string `toml:"tls_key_password,omitempty" json:"tls_key_password,omitempty"`
}

// Set reports whether any TLS key is given.
func (t TLSOpts) Set() bool {
	return t.TLS != "" || t.TLSCA != "" || t.TLSCert != "" || t.TLSKey != "" || t.TLSKeyPassword != ""
}

// envRef matches a whole tls_key_password: one environment variable, as
// ${NAME} or $NAME, with the kind of name a shell would export.
var envRef = regexp.MustCompile(`^\$(?:\{([A-Za-z_][A-Za-z0-9_]*)\}|([A-Za-z_][A-Za-z0-9_]*))$`)

// KeyPasswordVar is the name of the environment variable tls_key_password
// refers to, or "" when there is none — or when it is not a reference,
// which Check refuses.
func (t TLSOpts) KeyPasswordVar() string {
	m := envRef.FindStringSubmatch(strings.TrimSpace(t.TLSKeyPassword))
	if m == nil {
		return ""
	}
	return m[1] + m[2]
}

// Check normalizes the mode's case and turns away settings that cannot be
// what was meant. Every rule here fails closed: a typo in a security setting
// must not become "no TLS".
//
//   - an unknown mode ("verify_full", "true") is refused rather than read as
//     unset, which would fall back to the DSN — often plaintext;
//   - a CA or certificate with no mode is refused: whether it was meant to
//     verify or merely to be offered cannot be guessed;
//   - so are files beside tls = "disable", which would never be read;
//   - a client certificate needs its key and the other way round;
//   - tls_key_password must be an environment reference, and needs a
//     tls_key to unlock. A literal is refused rather than used: accepting
//     it would leave a passphrase in a plain-text file, and in every
//     browser dbc web shows the connection to.
func (t *TLSOpts) Check() error {
	t.TLS = strings.ToLower(strings.TrimSpace(t.TLS))
	t.TLSCA, t.TLSCert, t.TLSKey = strings.TrimSpace(t.TLSCA), strings.TrimSpace(t.TLSCert), strings.TrimSpace(t.TLSKey)
	t.TLSKeyPassword = strings.TrimSpace(t.TLSKeyPassword)
	files := t.TLSCA != "" || t.TLSCert != "" || t.TLSKey != ""
	switch {
	case t.TLS != "" && !validTLSMode(t.TLS):
		// the value goes in the message, which is shown as is (serr's
		// fields are not): the typo is what the user needs to see
		return serr.New(fmt.Sprintf("unknown tls mode %q (use %s)", t.TLS, strings.Join(TLSModes, ", ")))
	case t.TLS == "" && files:
		return serr.New("tls_ca, tls_cert and tls_key need a tls mode — " +
			`verify-full to check the server's certificate, or require to only encrypt`)
	case t.TLS == TLSDisable && files:
		return serr.New(`tls = "disable" never reads tls_ca, tls_cert or tls_key — remove them or pick another mode`)
	case (t.TLSCert == "") != (t.TLSKey == ""):
		return serr.New("tls_cert and tls_key go together: a client certificate is no use without its key")
	case t.TLSKeyPassword != "" && t.KeyPasswordVar() == "":
		// the value is NOT echoed: it may be the passphrase itself
		return serr.New(`tls_key_password must name an environment variable — "${MY_KEY_PASS}" — ` +
			"never the passphrase itself, which would sit in a plain-text file")
	case t.TLSKeyPassword != "" && t.TLSKey == "":
		return serr.New("tls_key_password unlocks tls_key — set tls_key (and tls_cert) too, or remove it")
	}
	return nil
}

func validTLSMode(m string) bool { return slices.Contains(TLSModes, m) }

// ExpandTLS returns t with its file paths made absolute: ${VAR} and $VAR
// expanded from the environment (as ExpandDSN does, with a warning for an
// unset one), a leading ~/ taken as the home directory, and a relative path
// joined to base — the directory of the file that defines the connection.
//
// Relative to that file, not to the working directory: dbc is started from
// wherever the user happens to be (a project for its scripts, a migrations
// folder), and "certs/ca.pem" written next to config.toml should mean the
// same file every time. base "" leaves a relative path relative to the
// working directory — the ad-hoc --tls-* flags, typed in the shell, mean
// that. name is the connection's, for the warnings.
func ExpandTLS(name string, t TLSOpts, base string) (TLSOpts, []string) {
	var warns []string
	one := func(key, p string) string {
		if p == "" {
			return ""
		}
		p = os.Expand(p, func(k string) string {
			v, ok := os.LookupEnv(k)
			if !ok {
				warns = append(warns, fmt.Sprintf(
					"connection %q: %s references unset env var $%s (expanded to empty)", name, key, k))
			}
			return v
		})
		if p == "~" || strings.HasPrefix(p, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				p = filepath.Join(home, p[1:])
			}
		}
		if base != "" && !filepath.IsAbs(p) {
			p = filepath.Join(base, p)
		}
		return p
	}
	t.TLSCA = one("tls_ca", t.TLSCA)
	t.TLSCert = one("tls_cert", t.TLSCert)
	t.TLSKey = one("tls_key", t.TLSKey)
	// TLSKeyPassword is left as written: it is a reference, resolved only
	// when the pool opens (db/tlskey.go), so the passphrase never lands in
	// the Connection, which is shown, logged and sent to the browser.
	return t, warns
}

// SavedDir is the directory relative tls_* paths of a saved connection (one
// added in dbc web) resolve against: the saved-connections file's own, as a
// config file's resolve against its directory. "" when there is no home
// directory, which leaves them relative to the working directory.
func SavedDir() string {
	if f := SavedFile(); f != "" {
		return filepath.Dir(f)
	}
	return ""
}
