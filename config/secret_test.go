package config

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSealOpenDSN(t *testing.T) {
	key := bytes.Repeat([]byte{7}, keyLen)
	const dsn = `host=db user=app password='p@ss w\'rd'`

	a, err := sealDSN(key, dsn)
	must(t, err)
	b, err := sealDSN(key, dsn)
	must(t, err)
	if !strings.HasPrefix(a, encPrefix) || strings.Contains(a, "p@ss") {
		t.Fatalf("envelope = %q", a)
	}
	// a fresh nonce each time: one DSN never encrypts to the same text twice
	if a == b {
		t.Fatal("two seals of one DSN are identical")
	}
	if got, err := openDSN(key, a); err != nil || got != dsn {
		t.Fatalf("open = %q, %v", got, err)
	}

	// a wrong key and a changed value both fail, rather than decrypt to garbage
	if _, err := openDSN(bytes.Repeat([]byte{8}, keyLen), a); err == nil {
		t.Fatal("opened with the wrong key")
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(a, encPrefix))
	must(t, err)
	raw[len(raw)/2] ^= 1 // one bit of the ciphertext
	if _, err := openDSN(key, encPrefix+base64.RawStdEncoding.EncodeToString(raw)); err == nil {
		t.Fatal("opened a changed envelope")
	}
	if _, err := openDSN(key, encPrefix+"!!"); err == nil {
		t.Fatal("opened an envelope that is not base64")
	}
}

// A file written by a dbc from before encryption — or by hand — is encrypted
// by the first LoadSaved, and the running config still gets the plain DSN.
func TestLoadSavedEncryptsPlainFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connections.toml")
	const pgDSN = "postgres://app:hunter2@db/app"
	must(t, os.WriteFile(path, []byte(`
[[connection]]
name = "pg"
driver = "postgres"
dsn = "`+pgDSN+`"

[[connection]]
name = "lite"
driver = "sqlite"
dsn = "file:lite.db"
`), 0o600))

	c := &Config{}
	c.LoadSaved(path, nil)
	if len(c.Warnings) != 0 {
		t.Fatalf("warnings: %q", c.Warnings)
	}
	if cn, ok := c.ConnByName("pg"); !ok || cn.DSN != pgDSN {
		t.Fatalf("merged pg = %+v", cn)
	}

	bs, _ := os.ReadFile(path)
	if text := string(bs); strings.Contains(text, "hunter2") || !strings.Contains(text, encPrefix) ||
		!strings.Contains(text, `dsn = "file:lite.db"`) {
		t.Fatalf("after LoadSaved the file reads:\n%s", text)
	}

	// read back plain, by every reader
	got, err := ReadSaved(path)
	must(t, err)
	if got[0].DSN != pgDSN || got[1].DSN != "file:lite.db" {
		t.Fatalf("read back %+v", got)
	}

	// the key is made once and kept: later writes encrypt with the same one
	key1, err := os.ReadFile(keyFile(path))
	must(t, err)
	must(t, OpenSaved(path).Add(SavedConn{Name: "my", Driver: "mysql", DSN: "u:pw@tcp(h)/d"}))
	key2, _ := os.ReadFile(keyFile(path))
	if !bytes.Equal(key1, key2) {
		t.Fatal("the key changed on a later write")
	}
	if got, _ = ReadSaved(path); len(got) != 3 || got[2].DSN != "u:pw@tcp(h)/d" {
		t.Fatalf("after an add: %+v", got)
	}

	// a second LoadSaved finds nothing plain, and writes nothing
	before, _ := os.Stat(path)
	(&Config{}).LoadSaved(path, nil)
	if after, _ := os.Stat(path); !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("an already encrypted file was rewritten")
	}
}

// A file of file-based connections holds no password, so it gets no key.
func TestNoKeyWithoutServerDSN(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connections.toml")
	must(t, OpenSaved(path).Add(SavedConn{Name: "a", Driver: "sqlite", DSN: "file:a.db"}))
	must(t, OpenSaved(path).Add(SavedConn{Name: "b", Driver: "bytdb", DSN: "b.bytdb"}))
	if _, err := os.Stat(keyFile(path)); !os.IsNotExist(err) {
		t.Fatalf("key file for a sqlite/bytdb-only file: %v", err)
	}
}

// Without its key the file cannot be read — and so is never rewritten: a
// write would lose every encrypted DSN in it.
func TestMissingKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connections.toml")
	must(t, OpenSaved(path).Add(SavedConn{Name: "pg", Driver: "postgres", DSN: "host=h password=x"}))
	must(t, os.Remove(keyFile(path)))
	before, _ := os.ReadFile(path)

	if _, err := ReadSaved(path); err == nil || !strings.Contains(err.Error(), keyFile(path)) {
		t.Fatalf("read without a key = %v, want an error naming it", err)
	}
	if err := OpenSaved(path).Add(SavedConn{Name: "b", Driver: "postgres", DSN: "host=b"}); err == nil {
		t.Fatal("an add without the key succeeded")
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatalf("the file was changed:\n%s", after)
	}
	// the add must not have made a new key either: one would not open the
	// DSNs already in the file
	if _, err := os.Stat(keyFile(path)); !os.IsNotExist(err) {
		t.Fatalf("a new key was made: %v", err)
	}

	c := &Config{}
	c.LoadSaved(path, nil)
	if len(c.Connections) != 0 || len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "missing") {
		t.Fatalf("LoadSaved without a key: %+v, %q", c.Connections, c.Warnings)
	}
}

func TestMalformedKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connections.toml")
	must(t, OpenSaved(path).Add(SavedConn{Name: "pg", Driver: "postgres", DSN: "host=h"}))
	must(t, os.WriteFile(keyFile(path), []byte("not hex\n"), 0o600))
	if _, err := ReadSaved(path); err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("read with a malformed key = %v", err)
	}
	// a writer must not replace a key it cannot read with a new one
	if err := OpenSaved(path).Add(SavedConn{Name: "b", Driver: "postgres", DSN: "host=b"}); err == nil {
		t.Fatal("an add with a malformed key succeeded")
	}
	if bs, _ := os.ReadFile(keyFile(path)); string(bs) != "not hex\n" {
		t.Fatalf("the key file was replaced: %q", bs)
	}
}

func TestKeyFile(t *testing.T) {
	if got := keyFile(filepath.Join("a", "connections.toml")); got != filepath.Join("a", "connections.key") {
		t.Fatalf("keyFile = %q", got)
	}
}
