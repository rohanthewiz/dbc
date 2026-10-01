package db

import (
	"io"
	"log"
	"os"

	"github.com/go-sql-driver/mysql"
)

// SetDriverLog sends the database drivers' own log lines to w. Only
// go-sql-driver/mysql writes any: it logs a connection that died under it
// ("[mysql] packets.go:58 unexpected EOF", "closing bad idle connection")
// straight to stderr, on top of the error it returns to the caller.
//
// On stderr those lines are right for the headless commands and dbc web's
// server log, but the terminal UI owns the screen: a stray write lands in
// the middle of its frame and stays there until the next full redraw. The
// TUI passes a writer of its own, which shows each line in its log pane.
// nil restores stderr.
func SetDriverLog(w io.Writer) {
	if w == nil {
		_ = mysql.SetLogger(log.New(os.Stderr, "[mysql] ", log.Ldate|log.Ltime))
		return
	}
	// No date or time: the TUI's log pane stamps nothing, and the lines
	// read as what they are, notes from the driver, next to dbc's own.
	_ = mysql.SetLogger(log.New(w, "mysql driver: ", 0))
}
