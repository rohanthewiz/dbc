// Package pgerr reads what a Postgres error carries besides its message:
// the DETAIL, HINT and CONTEXT a server sends with an error and that
// pgconn.PgError.Error() leaves out. It is its own small package so both
// the query path (db) and the loaders (etl, which keeps its dependencies
// few) report a server's error the same way.
package pgerr

import (
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// WithDetail puts a Postgres error's DETAIL, HINT and CONTEXT into its
// text. pgconn.PgError.Error() is "SEVERITY: message (SQLSTATE x)" alone,
// so the DETAIL and HINT a server error carries — and that RAISE EXCEPTION
// … USING DETAIL = …, HINT = … sets — never reached the log or the
// assistant, nor did CONTEXT, which says which function raised and from
// where. They are appended to the message, as the log's notice lines write
// them (Notice.String), rather than added as serr fields: fields print
// after the location noise, in no fixed order, where a hint is easy to miss.
//
// The error is wrapped with %w, so errors.As still finds the *PgError
// (BadConn, the fault and cancel checks are unaffected).
func WithDetail(err error) error {
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		return err
	}
	var b strings.Builder
	if pe.Detail != "" {
		b.WriteString(" — DETAIL: ")
		b.WriteString(pe.Detail)
	}
	if pe.Hint != "" {
		b.WriteString(" — HINT: ")
		b.WriteString(pe.Hint)
	}
	if c := Context(pe.Where); c != "" {
		b.WriteString(" — CONTEXT: ")
		b.WriteString(c)
	}
	if b.Len() == 0 {
		return err
	}
	return fmt.Errorf("%w%s", err, b.String())
}

// Limits on the CONTEXT Context keeps: frames of the call chain, and
// runes per frame (a "SQL statement" frame quotes the whole statement).
const (
	ContextFrames = 4
	ContextRunes  = 160
)

// Context condenses a Postgres error's CONTEXT (PgError.Where) to one
// line. The server sends one frame per line, innermost first:
//
//	PL/pgSQL function check_job(text,integer) line 7 at RAISE
//	SQL statement "SELECT check_job('nightly', 5)"
//	PL/pgSQL function inline_code_block line 1 at PERFORM
//
// becomes
//
//	PL/pgSQL function check_job(text,integer) line 7 at RAISE ← SQL statement "SELECT check_job('nightly', 5)"
//
// A DO block's own frame ("inline_code_block") is dropped: the block is
// what the user just ran, so it adds nothing — and a RAISE straight in a DO
// block, whose CONTEXT is only that frame, gets no CONTEXT at all. A deep
// chain keeps its innermost frames, the ones nearest the error, and ends in
// "← …"; a long frame is cut with "…". Whitespace runs collapse to one
// space, so the log line stays one line.
//
// A frame is not always one line: a "SQL statement" frame quotes the
// statement as written, newlines and all, and the server does not escape
// it. So lines are joined while a frame's double quotes are unbalanced —
// the quotes around the statement open it, and an identifier quoted inside
// it ("Order") comes in pairs, so the parity holds for any statement whose
// own quotes balance, which is every one that parsed.
func Context(where string) string {
	var raw []string
	for _, line := range strings.Split(where, "\n") {
		if n := len(raw); n > 0 && strings.Count(raw[n-1], `"`)%2 == 1 {
			raw[n-1] += "\n" + line // still inside a quoted statement
			continue
		}
		raw = append(raw, line)
	}
	var frames []string
	for _, f := range raw {
		f = strings.Join(strings.Fields(f), " ")
		if f == "" || strings.Contains(f, "inline_code_block") {
			continue
		}
		if r := []rune(f); len(r) > ContextRunes {
			f = string(r[:ContextRunes]) + "…"
		}
		frames = append(frames, f)
	}
	if len(frames) > ContextFrames {
		frames = append(frames[:ContextFrames], "…")
	}
	return strings.Join(frames, " ← ")
}
