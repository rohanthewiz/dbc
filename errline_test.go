package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/serr"
)

// errLine is the plain failure line: msg, wrap messages outermost first,
// the root error, then the error's own fields — never serr's frames.
func TestErrLine(t *testing.T) {
	inner := serr.Wrap(errors.New(`near "selec": syntax error`), "conn", "lite")
	cases := []struct {
		name string
		err  error
		msg  string
		want string
	}{
		{"plain error", errors.New("boom"), "query failed", "dbc: query failed: boom"},
		{"no msg", errors.New("boom"), "", "dbc: boom"},
		{"nil", nil, "query failed", "dbc: query failed: unknown error"},
		{"fields", inner, "query failed", `dbc: query failed: near "selec": syntax error [conn=lite]`},
		{"wraps outer first",
			serr.Wrap(serr.Wrap(errors.New("no such file"), "reading the spec"), "loading pipeline nightly"),
			"run failed", "dbc: run failed: loading pipeline nightly: reading the spec: no such file"},
		{"a wrap equal to msg is said once",
			serr.Wrap(errors.New("x"), "copy failed"), "copy failed", "dbc: copy failed: x"},
		{"duplicate pair once, differing values kept",
			serr.Wrap(serr.Wrap(errors.New("dup"), "conn", "w", "op", "insert"), "conn", "w", "op", "commit"),
			"copy failed", "dbc: copy failed: dup [conn=w op=insert op=commit]"},
		{"quoted values",
			serr.New("bad", "path", "/tmp/a b", "stmt", `SELECT "x"`, "empty", ""),
			"", `dbc: bad [path="/tmp/a b" stmt="SELECT \"x\"" empty=""]`},
		{"serr inside a %w chain keeps its fields",
			fmt.Errorf("outer: %w", serr.New("inner", "k", "v")), "m", "dbc: m: outer: inner [k=v]"},
	}
	for _, c := range cases {
		if got := errLine(c.err, c.msg); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
		if strings.Contains(errLine(c.err, c.msg), "location=") {
			t.Errorf("%s: frame fields leaked", c.name)
		}
	}
}

func TestStructuredErrorsEnv(t *testing.T) {
	for v, want := range map[string]bool{"": false, "0": false, "false": false, "off": false,
		"1": true, "true": true, "yes": true} {
		t.Setenv(logErrorsEnv, v)
		if got := structuredErrors(); got != want {
			t.Errorf("%s=%q: structured %v, want %v", logErrorsEnv, v, got, want)
		}
	}
}

// A lone bare word that is neither a command nor a one-word statement is
// refused, with the nearest command when one is close; SQL gets through.
func TestCheckBareWord(t *testing.T) {
	cmds := newCLI().Commands
	refused := map[string]string{
		"wbe":      `did you mean "dbc web"?`,
		"scirpt":   `did you mean "dbc script"?`,
		"migrtae":  `did you mean "dbc migrate"?`,
		"pipelien": `did you mean "dbc pipeline"?`,
		"foo":      "dbc --help lists them",
		"Users":    "dbc --help lists them",
	}
	for arg, want := range refused {
		err := checkBareWord(arg, cmds)
		if err == nil {
			t.Errorf("%q: not refused", arg)
			continue
		}
		if !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "dbc '"+arg+";'") {
			t.Errorf("%q: %v (want %q and the ; hint)", arg, err, want)
		}
	}
	for _, arg := range []string{"select 1", "vacuum", "COMMIT", " begin ", "wbe;", `"users"`,
		"now()", "", "table users", "-- c\nvacuum"} {
		if err := checkBareWord(arg, cmds); err != nil {
			t.Errorf("%q refused: %v", arg, err)
		}
	}
}

func TestUnknownConn(t *testing.T) {
	cfg := &config.Config{Connections: []config.Connection{{Name: "lite"}, {Name: "pg"}}}
	got := unknownConn(cfg, "nope")
	if !strings.Contains(got, `no connection named "nope"`) || !strings.Contains(got, "configured: lite, pg") {
		t.Errorf("listed: %s", got)
	}
	if got := unknownConn(&config.Config{}, "x"); !strings.Contains(got, "none are configured") {
		t.Errorf("empty: %s", got)
	}
}
