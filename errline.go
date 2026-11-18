package main

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/rohanthewiz/dbc/config"

	"github.com/rohanthewiz/logger"
	"github.com/rohanthewiz/serr"
	"github.com/urfave/cli/v3"
)

// A headless failure is reported on stderr as one plain line:
//
//	dbc: query failed: SQL logic error: near "selec": syntax error (1) [conn=lite]
//	└┬─┘ └────┬─────┘  └──────────────────┬──────────────────────────┘ └───┬────┘
//	 │      what failed     the error (wrap messages, outer first, then  the error's
//	 │      (fail's msg)    the root error's text)                       own fields
//	 └ who is talking, as a Unix tool says it
//
// It used to be logrus's structured line (time=… level=error msg=… plus the
// serr frame stack), with the newlines in a message escaped and the cause
// at the far end of a wall of location fields. That suits a log file, not a
// person at a shell or a script reading stderr, so it is now opt-in:
// DBC_LOG_ERRORS=1 brings the old line back (logger.LogErr, as before) for
// anyone who parses it or wants the call locations.
//
// The fields come from serr's UserFields — what the code that failed chose to
// say (the connection, the statement's position, a path) — never the
// location/function frames serr adds on each wrap. They go last, in square
// brackets: an error's own text often ends in a parenthesis ("(1)", "(SQLSTATE
// 23505)"), so a bracket is the one ending a reader (or tutorial.lastLine)
// can cut off without guessing.

// logErrorsEnv names the environment variable that restores the structured
// log line for headless failures.
const logErrorsEnv = "DBC_LOG_ERRORS"

// structuredErrors reports whether DBC_LOG_ERRORS asks for the old line.
// "", "0" and "false" leave it off, so `DBC_LOG_ERRORS=0` in a script reads
// as meant.
func structuredErrors() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(logErrorsEnv))) {
	case "", "0", "false", "no", "off":
		return false
	}
	return true
}

// reportErr writes a failure to stderr without ending the run: the plain
// line, or the structured one under DBC_LOG_ERRORS. fail and the
// --keep-going report go through it, so every failure a run prints has the
// one shape.
func reportErr(err error, msg string) {
	if structuredErrors() {
		logger.LogErr(err, msg)
		return
	}
	fmt.Fprintln(os.Stderr, errLine(err, msg))
}

// errLine is the plain report of err: "dbc: msg: wraps: error [k=v …]".
// msg is the caller's account of what failed ("query failed"); it may be
// empty. See the diagram at the top of this file.
func errLine(err error, msg string) string {
	var b strings.Builder
	b.WriteString("dbc: ")
	if msg != "" {
		b.WriteString(msg)
		b.WriteString(": ")
	}
	if err == nil {
		// fail is never handed a nil error on purpose; say something rather
		// than print "dbc: msg: <nil>"
		b.WriteString("unknown error")
		return b.String()
	}
	var se *serr.SErr
	if !errors.As(err, &se) {
		b.WriteString(err.Error())
		return b.String()
	}
	// Wrap messages (serr.Wrap(err, "reading x")) are stored under "msg",
	// innermost first, since each wrap appends its fields after the ones it
	// was handed. Printed outermost first they read as Go's own
	// "outer: inner: cause" chain does. One equal to msg (or to the error's
	// text) is said once.
	text := err.Error()
	fields := se.Fields()
	var wraps []string
	for i := 0; i+1 < len(fields); i += 2 {
		if fields[i] == serr.MsgKey {
			wraps = append(wraps, fields[i+1])
		}
	}
	slices.Reverse(wraps)
	for _, w := range wraps {
		if w == "" || w == msg || w == text {
			continue
		}
		b.WriteString(w)
		b.WriteString(": ")
	}
	b.WriteString(text)

	// The user's fields, each pair once: a value wrapped twice with the same
	// key (the connection, by two layers that both know it) is noise.
	user := se.UserFields()
	seen := map[string]bool{}
	var pairs []string
	for i := 0; i+1 < len(user); i += 2 {
		kv := user[i] + "=" + fieldValue(user[i+1])
		if seen[kv] {
			continue
		}
		seen[kv] = true
		pairs = append(pairs, kv)
	}
	if len(pairs) > 0 {
		b.WriteString(" [")
		b.WriteString(strings.Join(pairs, " "))
		b.WriteString("]")
	}
	return b.String()
}

// fieldValue is a field's value as errLine writes it: bare when it is one
// plain token, quoted (Go syntax) when a space, quote, bracket or control
// character would make the pair ambiguous to read or to split.
func fieldValue(v string) string {
	if v == "" {
		return `""`
	}
	if strings.ContainsFunc(v, func(r rune) bool {
		return r <= ' ' || r == '"' || r == '[' || r == ']' || r == '=' || r == 0x7f
	}) {
		return strconv.Quote(v)
	}
	return v
}

// unknownConn is the usage message for a connection name the config does
// not have, naming the ones it does: a typo is then fixed by reading the
// line rather than opening the config file. An empty config (only --dsn
// would work) says that instead of listing nothing.
func unknownConn(cfg *config.Config, name string) string {
	var names []string
	for _, cn := range cfg.Conns() {
		names = append(names, cn.Name)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "no connection named %q", name)
	if len(names) == 0 {
		b.WriteString(" — none are configured")
	} else {
		b.WriteString(" — configured: ")
		b.WriteString(strings.Join(names, ", "))
	}
	fmt.Fprintf(&b, " (or --driver/--dsn for an ad-hoc one, named %q)", adHocConnName)
	return b.String()
}

// standaloneSQL is every statement that is a single word with nothing after
// it, in any of the dialects dbc speaks: the words a one-word argument may
// be and still be SQL. Anything else alone on the command line is a
// mistyped subcommand far more often than a statement.
var standaloneSQL = map[string]bool{
	"begin": true, "commit": true, "rollback": true, "end": true, "abort": true,
	"vacuum": true, "analyze": true, "analyse": true, "checkpoint": true,
	"reindex": true,
}

// checkBareWord refuses a lone argument that is one bare word and no SQL
// statement (`dbc wbe`): the cli package hands an unknown subcommand to the
// root action, which used to send it to the default connection as SQL and
// report the server's syntax error. Now it is a usage error that names the
// closest command. A trailing ";" (`dbc 'vacuum;'`, `dbc 'foo;'`) says it is
// SQL and gets past the check, as does anything with a space, quote or
// parenthesis in it.
func checkBareWord(arg string, cmds []*cli.Command) error {
	word := strings.TrimSpace(arg)
	if word == "" || !bareWord(word) || standaloneSQL[strings.ToLower(word)] {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%q is not a dbc command", word)
	if near := nearestCommand(word, cmds); near != "" {
		fmt.Fprintf(&b, " — did you mean \"dbc %s\"?", near)
	} else {
		b.WriteString(" (dbc --help lists them).")
	}
	fmt.Fprintf(&b, " To run it as SQL, end it with a semicolon: dbc '%s;'", word)
	return errors.New(b.String())
}

// bareWord reports whether s is one identifier-like word: ASCII letters,
// digits, '_' and '-' only. That is what a subcommand looks like; a quoted
// name, a statement or an expression has something else in it.
func bareWord(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// nearestCommand is the subcommand (or alias) closest to word by
// config's editDistance — a swap of two letters is one edit, so "wbe"
// finds "web" — within 2 edits (1 for a word of four letters or fewer,
// where 2 would match nearly anything), or "" when none is that close.
// Hidden commands are skipped: they are not offered in help either.
func nearestCommand(word string, cmds []*cli.Command) string {
	word = strings.ToLower(word)
	limit := 2
	if len(word) <= 4 {
		limit = 1
	}
	best, bestD := "", limit+1
	for _, c := range cmds {
		if c.Hidden {
			continue
		}
		for _, n := range append([]string{c.Name}, c.Aliases...) {
			if d := config.EditDistance(word, n); d < bestD {
				best, bestD = c.Name, d
			}
		}
	}
	return best
}
