package main

import (
	"os"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/config"
)

// What `dbc encrypt` accepts as the DSN: one line, trimmed, since a pipe's
// trailing newline would otherwise be encrypted into the last value.
func TestReadDSN(t *testing.T) {
	for _, tc := range []struct {
		in, want, err string
	}{
		{in: "postgres://u:p@h/db\n", want: "postgres://u:p@h/db"},
		{in: "  host=h password='a b'\r\n", want: "host=h password='a b'"},
		{in: "", err: "no DSN"},
		{in: " \n\n", err: "no DSN"},
		{in: "host=a\nhost=b\n", err: "more than one line"},
	} {
		got, err := readDSN(strings.NewReader(tc.in))
		switch {
		case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
			t.Errorf("readDSN(%q) error = %v, want %q", tc.in, err, tc.err)
		case tc.err == "" && (err != nil || got != tc.want):
			t.Errorf("readDSN(%q) = %q, %v, want %q", tc.in, got, err, tc.want)
		}
	}
}

// The ad-hoc --dsn takes an envelope from `dbc encrypt`, opened with the
// same key as a config file's; a plain --dsn is kept as typed, and a missing
// key fails the flags rather than dialing the envelope.
func TestAdHocDSNEnvelope(t *testing.T) {
	resetFlags(t)
	t.Setenv("HOME", t.TempDir())
	saved := config.SavedFile()
	const plain = "file:adhoc.db"
	env, err := config.SealDSN(saved, plain)
	if err != nil {
		t.Fatal(err)
	}

	flagDriver = "sqlite"
	for _, dsn := range []string{env, plain} {
		flagDSN = dsn
		cfg := &config.Config{}
		if err := addAdHocConn(cfg); err != nil {
			t.Fatalf("addAdHocConn(%q): %v", dsn, err)
		}
		if got := cfg.Connections[0].DSN; got != plain {
			t.Fatalf("--dsn %q became %q, want %q", dsn, got, plain)
		}
	}

	if err := os.Remove(config.KeyFile(saved)); err != nil {
		t.Fatal(err)
	}
	flagDSN = env
	if err := addAdHocConn(&config.Config{}); err == nil ||
		!strings.Contains(err.Error(), "--dsn") || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("envelope without its key = %v", err)
	}
}
