package export

import (
	"io"
	"strconv"

	"github.com/rohanthewiz/serr"
)

// CSVOptions are the choices a CSV or TSV export leaves to its caller
// (N-334). Both change the bytes written, so both are off by default — the
// zero CSVOptions is the export as it always was — and a user turns them
// on in the config (csv_bom, csv_formula_guard; config.Config.CSVOptions),
// for every export dbc writes to a file or to stdout: the CLI's -t csv,
// the TUI's and dbc web's export dialogs, a script's s.Export. A copy to
// the clipboard never takes them: a BOM in a paste is a stray character,
// and a paste is going into the user's own sheet.
//
// The other formats ignore them.
type CSVOptions struct {
	// BOM starts the document with a UTF-8 byte order mark. Excel reads a
	// CSV without one in the system's code page, so "é" opens as "Ã©";
	// with one it reads UTF-8. Most other readers skip it, but not all
	// (a shell pipeline's cut or awk sees three bytes), hence an option.
	BOM bool
	// FormulaGuard puts a ' before a text cell a spreadsheet would read as
	// a formula (GuardFormula) — for an export of text people typed, sent
	// on to someone who will open it in Excel or Sheets.
	FormulaGuard bool
}

// BOM is the UTF-8 byte order mark CSVOptions.BOM writes.
const BOM = "\uFEFF"

// csvLike reports whether f is one of the formats CSVOptions apply to.
func csvLike(f Format) bool { return f == CSV || f == TSV }

// WriteBOM writes the byte order mark to w when o asks for one and f is
// CSV or TSV: once, at the very start of a document. A caller streaming a
// document block by block (the CLI) calls it before the first block and
// writes the blocks with BOM off, as WriteRunWith does.
func WriteBOM(w io.Writer, f Format, o CSVOptions) error {
	if !o.BOM || !csvLike(f) {
		return nil
	}
	if _, err := io.WriteString(w, BOM); err != nil {
		return serr.Wrap(err)
	}
	return nil
}

// GuardFormula is cell's text as FormulaGuard writes it: with a ' in front
// when a spreadsheet opening the file would take it for a formula, else as
// it is. raw is the cell's raw value (model.Result.Raw).
//
// A spreadsheet reads a cell starting with =, +, - or @ as a formula, and
// one starting with a tab or a carriage return can be turned into one; a
// cell holding =HYPERLINK(…) or =cmd|… in an export of user-entered text
// is the "CSV injection" OWASP describes. The ' is the spreadsheets' own
// "this is text" mark: Excel and Sheets show the cell without it.
//
// Numbers are left alone, so -5 stays -5: a raw Go number never starts a
// formula, and nor does text that is a plain number — which is how MySQL's
// text protocol hands over every number, as bytes, so a raw string can be
// an INT's value. Only text that is not a number is guarded: "-5+3",
// "+1 555 0100", "@home", "=SUM(A1:A9)".
func GuardFormula(cell string, raw any) string {
	if cell == "" {
		return cell
	}
	switch cell[0] {
	case '=', '+', '-', '@', '\t', '\r':
	default:
		return cell
	}
	switch raw.(type) {
	case string, []byte, nil:
		// text, or a number sent as text: a number is not a formula. nil
		// is a row built by hand, with no raw values, which has only its
		// text to go by (a NULL's field is empty, and returned above).
		if _, err := strconv.ParseFloat(cell, 64); err == nil {
			return cell
		}
		return "'" + cell
	}
	// a Go number, a bool: no formula
	return cell
}

// GuardRecord applies GuardFormula to a delimited record in place: what
// FormulaGuard does to each row, for a caller writing rows itself (the
// CLI's streamed CSV, after DelimitedRecord).
// rec[i]'s raw value is raw[i]; a NULL's field is already empty
// (DelimitedRecord), so its nil raw is never mistaken for a hand-built
// row's.
func GuardRecord(rec []string, raw []any) {
	for i, cell := range rec {
		var r any
		if i < len(raw) {
			r = raw[i]
		} else if len(raw) > 0 {
			continue // past the raw values of a row that has them: leave it
		}
		rec[i] = GuardFormula(cell, r)
	}
}
