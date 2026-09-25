package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/rohanthewiz/logger"
	"github.com/rohanthewiz/rweb"
	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/db"
	"github.com/rohanthewiz/dbc/workspace"
)

// Every JSON response is this envelope, gonotes' shape. Error is words for
// the user; Stopped marks a request that ended because the user stopped it —
// shown as stopped, not failed.
type envelope struct {
	Success bool   `json:"success"`
	Data    any    `json:"data,omitempty"`
	Error   string `json:"error,omitempty"`
	Stopped bool   `json:"stopped,omitempty"`
}

// reqError is a request the web layer itself turns away — a malformed body,
// an unknown workspace — as opposed to one the workspace refused.
type reqError struct {
	status int
	msg    string
}

func (e *reqError) Error() string { return e.msg }

func badRequest(format string, args ...any) error {
	return &reqError{status: http.StatusBadRequest, msg: fmt.Sprintf(format, args...)}
}

func notFound(format string, args ...any) error {
	return &reqError{status: http.StatusNotFound, msg: fmt.Sprintf(format, args...)}
}

// conflict is a request that raced something that moved under it — the
// result it was about was replaced, the assistant is still answering — and
// can be made again once the page has caught up.
func conflict(format string, args ...any) error {
	return &reqError{status: http.StatusConflict, msg: fmt.Sprintf(format, args...)}
}

// ok answers 200 with data.
func ok(ctx rweb.Context, data any) error {
	return writeJSON(ctx, http.StatusOK, envelope{Success: true, Data: data})
}

// fail is the one place an error becomes a response. The status comes from
// the error, not from the handler that saw it:
//
//	error                                  status  envelope
//	─────────────────────────────────────  ──────  ─────────────────────────
//	*workspace.Refusal, Busy               409     error: the refusal's words
//	*workspace.Refusal, any other reason   400     error: the refusal's words
//	*reqError                              its own error: its message
//	db.ErrCanceled (anywhere in the chain) 200     stopped: true
//	anything else                          500     error: the user message
//
// Only a 500 is logged: it is the one that means something went wrong in
// dbc. The rest are the user's input or the user's choice, and each is
// already said in words to the user. The log gets every field serr
// accumulated on the way up; the browser gets serr's user message where a
// layer set one, else the core error text — never the fields, where a
// connection string could hide.
func fail(ctx rweb.Context, err error) error {
	status, env := classify(err)
	if status >= http.StatusInternalServerError {
		logger.LogErr(serr.Wrap(err, "route", ctx.Request().Path()), "request failed")
	}
	return writeJSON(ctx, status, env)
}

// classify maps an error to a status and envelope; see fail.
func classify(err error) (int, envelope) {
	var ref *workspace.Refusal
	var re *reqError
	switch {
	case errors.As(err, &ref):
		status := http.StatusBadRequest
		if ref.Reason == workspace.Busy {
			status = http.StatusConflict
		}
		return status, envelope{Error: ref.Note.Text}
	case errors.As(err, &re):
		return re.status, envelope{Error: re.msg}
	case errors.Is(err, db.ErrCanceled):
		return http.StatusOK, envelope{Stopped: true, Error: "stopped"}
	}
	return http.StatusInternalServerError, envelope{Error: userMsg(err)}
}

// userMsg is what the browser is told about an error: the user message a
// layer set with SetUserMsg, else the core error's own text.
func userMsg(err error) string {
	return serr.UserMsgFromErr(err, err.Error())
}

func writeJSON(ctx rweb.Context, status int, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return serr.Wrap(err, "op", "encode response")
	}
	ctx.SetStatus(status)
	ctx.Response().SetHeader("Content-Type", "application/json; charset=utf-8")
	ctx.Response().SetHeader("Cache-Control", "no-store")
	return ctx.Bytes(b)
}

// decode reads a JSON request body into v; a body that does not parse is a
// 400, not a 500.
func decode(ctx rweb.Context, v any) error {
	body := ctx.Request().Body()
	if len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, v); err != nil {
		return badRequest("the request body is not valid JSON: %v", err)
	}
	return nil
}
