package config

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/rohanthewiz/serr"
)

// Encryption of the DSNs in the saved-connections file (saved.go).
//
// WHAT IT IS FOR. A DSN typed into dbc web's or the TUI's connection form
// usually carries its password inline, and connections.toml used to keep it
// as typed. Encrypted, the file can be backed up, synced with dotfiles,
// grepped, pasted into an issue or shown on a shared screen without giving
// a password away: the key is in a second file, connections.key, beside it.
//
// WHAT IT IS NOT. Both files sit in the same 0700 directory, readable by the
// same user, so anything running as that user — or anyone with a copy of
// the whole directory — can still decrypt. It protects the file leaking on
// its own, not the account. Keeping the key in the OS keychain instead
// would be the next step, and would need no change to the file's format.
//
// FORMAT.
//
//	connections.key   64 hex characters: a 32-byte AES-256 key from
//	                  crypto/rand, made by the first write that needs it, 0600
//	dsn = "enc:v1:<base64( nonce(12) ‖ AES-256-GCM(dsn) ‖ tag(16) )>"
//
// GCM rather than a bare stream cipher because it is authenticated: a wrong
// key or a hand-mangled value fails to open, instead of decrypting to
// garbage that would then be sent to a server as a DSN. The nonce is fresh
// from crypto/rand on every write (each write re-encrypts the whole file);
// at 96 random bits a repeat under one key is out of reach for the few
// thousand writes a file like this ever sees. "v1" leaves room to change
// the scheme later and still read this one.
//
// WHICH DSNs. Those of database servers — every driver but the file-based
// sqlite and bytdb — since only they take a password; a sqlite or bytdb DSN
// is a path, and stays readable. A DSN holding its password as a ${VAR} is
// encrypted too: telling a ${VAR} password from an inline one would mean
// parsing each driver's DSN syntax, and a wrong call there would leave a
// password in plain text — the one outcome this is here to prevent.
//
// IN MEMORY everything is plain: ReadSaved decrypts, writeSaved encrypts,
// so SavedConn.DSN, every caller and the running config never see an
// envelope.
//
//	connections.toml ──ReadSaved──► openSaved ──► []SavedConn (plain) ──► callers
//	                 ◄─writeSaved── sealSaved ◄── []SavedConn (plain) ◄── edit
//	                                    │
//	                                    └─ loadOrCreateKey(connections.key)
//
// A plain dsn found in the file — written by an older dbc, or by hand — is
// read as is and encrypted at the next write, which LoadSaved makes at once.

// encPrefix starts an encrypted DSN. No real DSN does: a postgres one starts
// with its scheme or a key=, a mysql one with its user, and a mysql user
// named enc would also need a password starting with "v1:".
const encPrefix = "enc:v1:"

// keyLen is AES-256's key size.
const keyLen = 32

// keyFile is the key for the saved-connections file at savedPath: beside
// it, with .key for its extension (connections.toml ─► connections.key).
// Derived from the file rather than fixed under ~/.config/dbc, so a test's
// file in a temp directory gets a key of its own, and a copy of the
// directory carries both.
func keyFile(savedPath string) string {
	return strings.TrimSuffix(savedPath, filepath.Ext(savedPath)) + ".key"
}

// sealsDSN reports whether a driver's DSNs are encrypted: a database
// server's, which may hold a password; not sqlite's or bytdb's, a file
// path. An unknown driver counts as a server, so a typo in it cannot leave
// a password in plain text.
func sealsDSN(driver string) bool {
	switch strings.ToLower(strings.TrimSpace(driver)) {
	case "sqlite", "sqlite3", "bytdb":
		return false
	}
	return true
}

// readKey reads the key at path. A missing file is an error errors.Is
// os.ErrNotExist, which callers branch on: for a reader it means encrypted
// DSNs it cannot open, for a writer a key still to be made.
func readKey(path string) ([]byte, error) {
	bs, err := os.ReadFile(path)
	if err != nil {
		return nil, serr.Wrap(err, "path", path)
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(bs)))
	if err != nil || len(key) != keyLen {
		return nil, serr.New(fmt.Sprintf(
			"key file %s is malformed: it should hold %d hex characters", path, 2*keyLen))
	}
	return key, nil
}

// loadOrCreateKey reads the key at path, first making one if there is none.
//
// Only writeSaved calls it, and only under the writers' lock (see edit), so
// two dbc processes cannot both find no key and each make one: the second
// would replace the first after DSNs had been encrypted with it, losing
// them for good.
//
// A new key is written as the file it protects is (writeAtomic: temp file,
// fsync, rename), so no reader ever sees half of one. The directory is then
// synced too, best effort (not every OS can sync a directory), so the key's
// name is on disk before any DSN encrypted with it is.
func loadOrCreateKey(path string) ([]byte, error) {
	key, err := readKey(path)
	if !errors.Is(err, os.ErrNotExist) {
		return key, err // the key, or a key file that is there but unusable
	}
	key = make([]byte, keyLen)
	if _, err = rand.Read(key); err != nil {
		return nil, serr.Wrap(err, "op", "generate key")
	}
	if err = writeAtomic(path, []byte(hex.EncodeToString(key)+"\n")); err != nil {
		return nil, err
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return key, nil
}

// newAEAD is AES-256-GCM over key, in the form that draws a random nonce
// on each Seal and carries it at the front of the result, so the envelope
// is nonce ‖ ciphertext ‖ tag without this file slicing it by hand.
func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, serr.Wrap(err, "op", "aes cipher")
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, serr.Wrap(err, "op", "gcm")
	}
	return aead, nil
}

// sealDSN encrypts dsn into its envelope: encPrefix, then the base64 of
// nonce ‖ ciphertext ‖ tag.
func sealDSN(key []byte, dsn string) (string, error) {
	aead, err := newAEAD(key)
	if err != nil {
		return "", err
	}
	return encPrefix + base64.RawStdEncoding.EncodeToString(aead.Seal(nil, nil, []byte(dsn), nil)), nil
}

// openDSN decrypts an envelope sealDSN made. GCM cannot tell a wrong key
// from a changed value — both fail the tag check — so the error names both.
func openDSN(key []byte, envelope string) (string, error) {
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(envelope, encPrefix))
	if err != nil {
		return "", serr.New("the encrypted DSN is not valid base64 (edited by hand?)")
	}
	aead, err := newAEAD(key)
	if err != nil {
		return "", err
	}
	plain, err := aead.Open(nil, nil, raw, nil)
	if err != nil {
		return "", serr.New("the encrypted DSN does not decrypt: the key is not the one it was " +
			"written with, or the value was changed")
	}
	return string(plain), nil
}

// openSaved decrypts, in place, the DSNs in list that are envelopes,
// reading the key beside path only once one is found. It reports whether
// any DSN that sealsDSN would encrypt was plain in the file, for LoadSaved
// to encrypt it now.
//
// One DSN that does not open fails the whole read, the way a file that
// does not parse does: an edit must not rewrite the file without it, and a
// connection must not be merged with an envelope for its DSN. In practice
// it is all or none anyway — a missing or wrong key fails every envelope.
func openSaved(path string, list []SavedConn) (plain bool, err error) {
	var key []byte
	for i := range list {
		sc := &list[i]
		if !strings.HasPrefix(sc.DSN, encPrefix) {
			plain = plain || (sc.DSN != "" && sealsDSN(sc.Driver))
			continue
		}
		if key == nil {
			if key, err = readKey(keyFile(path)); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return false, serr.New(fmt.Sprintf(
						"saved connection %q has an encrypted DSN, but its key %s is missing: "+
							"restore the key, or replace the encrypted dsn in %s with the plain one "+
							"(dbc encrypts it again)", sc.Name, keyFile(path), path))
				}
				return false, err
			}
		}
		if sc.DSN, err = openDSN(key, sc.DSN); err != nil {
			return false, serr.New(fmt.Sprintf("saved connection %q: %v (key %s)",
				sc.Name, err, keyFile(path)))
		}
	}
	return plain, nil
}

// sealSaved returns a copy of list with the DSNs sealsDSN picks encrypted,
// leaving list itself plain: it is the caller's (edit's next list, which a
// memory-only store keeps). The key is read, or made, only when some DSN
// needs it, so a file of sqlite connections never gets one.
func sealSaved(path string, list []SavedConn) ([]SavedConn, error) {
	out := slices.Clone(list)
	var key []byte
	for i := range out {
		sc := &out[i]
		if sc.DSN == "" || !sealsDSN(sc.Driver) {
			continue
		}
		var err error
		if key == nil {
			if key, err = loadOrCreateKey(keyFile(path)); err != nil {
				return nil, err
			}
		}
		if sc.DSN, err = sealDSN(key, sc.DSN); err != nil {
			return nil, serr.Wrap(err, "connection", sc.Name)
		}
	}
	return out, nil
}
