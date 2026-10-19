package sqlsplit

import "strings"

// Fold is a span of a buffer an editor can fold away, as byte offsets.
// Start is the first byte of the line that stays visible as the fold's
// header; End is the last byte the fold covers, so the line holding End is
// the last one hidden. An editor needs only the lines of the two, which
// keeps this package free of any one editor's line model (Monaco counts
// "\r" alone as a line break; the offsets mean the same either way).
type Fold struct {
	Start, End int
}

// Folds returns the foldable spans of sql, outermost first within each
// statement:
//
//   - every statement whose code spans more than one line, from its code
//     (CodeStart: heading comments stay visible above the fold, as a label,
//     and the folded line reads as the statement's verb) to its last byte;
//   - inside it, every parenthesised block that spans lines — a CTE's
//     body, a subquery, a column list — from the line of its "(" to the
//     line before its ")" when that ")" opens its own line, so the fold
//     reads "AS (⋯" over a ")" still in place, as a code editor's bracket
//     fold does. A ")" after code on its line is folded with that line.
//
//	WITH recent AS (       ← stmt fold header, and the paren fold's
//	    SELECT *           ┐ paren fold
//	    FROM runs          ┘ (End: its last byte, the "\n" before ")")
//	)                      ┐ stmt fold
//	SELECT * FROM recent;  ┘
//
// It is computed from the splitter and the lexer, not a grammar, for the
// reason everything here is: the statement a fold hides is exactly the one
// Ctrl+Enter runs, and a "(" inside a string, a quoted name, a comment or a
// dollar-quoted body is not a bracket. Parens are matched per statement, so
// one left unclosed (a statement being typed) cannot swallow the next ones;
// an unmatched "(" or ")" is skipped.
//
// Single-line spans are dropped — there is nothing to fold. Two spans can
// share a header line, as the statement and its CTE do above; an editor
// draws one fold arrow per line and Monaco keeps the first range it was
// given for a line, so each statement's fold comes before its blocks':
// folding the query wins over folding its first block. A block that opens
// on a later line ("FROM (" under "SELECT *") keeps a fold of its own.
func Folds(sql string) []Fold {
	stmts := Split(sql)
	if len(stmts) == 0 {
		return nil
	}
	// The spans no bracket can live in, in buffer order, for a cursor that
	// only moves forward across the statements below.
	var opaque []Token
	for _, t := range Lex(sql) {
		switch t.Kind {
		case TokString, TokIdent, TokComment:
			opaque = append(opaque, t)
		}
	}
	var out []Fold
	k := 0 // next opaque span not yet behind the scan
	for _, st := range stmts {
		// the statement's code: its heading comments are not folded with it
		if end := st.End - 1; multiLine(sql, st.CodeStart, end) {
			out = append(out, Fold{Start: lineStart(sql, st.CodeStart), End: end})
		}
		var open []int // offsets of the "(" not yet closed, innermost last
		for i := st.CodeStart; i < st.End; i++ {
			for k < len(opaque) && opaque[k].End <= i {
				k++
			}
			if k < len(opaque) && opaque[k].Start <= i {
				i = opaque[k].End - 1 // the loop's i++ steps past the span
				continue
			}
			switch sql[i] {
			case '(':
				open = append(open, i)
			case ')':
				if len(open) == 0 {
					continue // unmatched: not a block
				}
				o := open[len(open)-1]
				open = open[:len(open)-1]
				end := i
				// a ")" opening its own line stays visible: the fold ends
				// on the line above, at that line's break
				if ls := lineStart(sql, i); strings.TrimSpace(sql[ls:i]) == "" && ls > 0 {
					end = ls - 1
				}
				if multiLine(sql, o, end) {
					out = append(out, Fold{Start: lineStart(sql, o), End: end})
				}
			}
		}
	}
	return out
}

// multiLine reports whether sql[a:b] — the fold's header byte up to, not
// including, its last byte — crosses a line break, i.e. whether a fold from
// a to b would hide at least one line. A last byte that IS the break (a
// paren fold ending above its ")") ends that line, so it does not count.
func multiLine(sql string, a, b int) bool {
	return a < b && strings.ContainsAny(sql[a:b], "\n\r")
}

// lineStart is the offset of the first byte of the line holding sql[i].
func lineStart(sql string, i int) int {
	return strings.LastIndexAny(sql[:i], "\n\r") + 1
}
