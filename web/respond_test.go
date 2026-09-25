package web

import (
	"errors"
	"testing"

	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/workspace"
)

// The status comes from the error, not from the handler that saw it; this
// pins the mapping fail documents.
func TestClassifyStatus(t *testing.T) {
	busy := &workspace.Refusal{Reason: workspace.Busy, Note: workspace.Note{Text: "busy — query is still running (Ctrl+K stops it)"}}
	nothing := &workspace.Refusal{Reason: workspace.Nothing, Note: workspace.Note{Text: "nothing to run — type a query first"}}
	withUser := serr.WrapAsSErr(errors.New("dial tcp: refused"), "dsn", "postgres://u:secret@h/db")
	withUser.SetUserMsg("could not reach the database", serr.Severity.Error)

	for _, c := range []struct {
		name    string
		err     error
		status  int
		msg     string
		stopped bool
	}{
		{"busy", busy, 409, busy.Note.Text, false},
		{"busy, wrapped", serr.Wrap(busy, "route", "/run"), 409, busy.Note.Text, false},
		{"nothing to run", nothing, 400, nothing.Note.Text, false},
		{"bad request", badRequest("unknown connection %q", "x"), 400, `unknown connection "x"`, false},
		{"unknown workspace", notFound("no workspace"), 404, "no workspace", false},
		{"stopped", serr.Wrap(db.ErrCanceled, "conn", "pg"), 200, "stopped", true},
		// a failure: the user message where one was set, never the fields
		{"user message", withUser, 500, "could not reach the database", false},
		{"plain", serr.Wrap(errors.New("disk full"), "dsn", "postgres://u:secret@h/db"), 500, "disk full", false},
	} {
		status, env := classify(c.err)
		if status != c.status || env.Error != c.msg || env.Stopped != c.stopped {
			t.Errorf("%s: %d %q stopped=%v, want %d %q stopped=%v",
				c.name, status, env.Error, env.Stopped, c.status, c.msg, c.stopped)
		}
	}
}
