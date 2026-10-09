package script

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/scanner"
	"go/token"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Check finds what is wrong with a script without running it, so an editor
// can mark errors on their lines as the user types, and `dbc script --check`
// can gate a scripts repo in CI. It never executes the script: not Run, not
// init(), not a package-level initializer.
//
//	src ─► 1. parse (go/parser) ── syntax errors? ──► those, and stop
//	        │                                         (the passes below need a tree)
//	        ├► 2. signature: package main, func Run(s *sdb.S) error, from the AST
//	        ├► 3. lint: m[k], _ = …  (yaegi drops the write)
//	        └► 4. compile (yaegi, newInterp + Compile, no Execute)
//
// Why each pass:
//
//  1. go/parser first, because its positions are exact and it reports up to
//     ten errors; yaegi stops at its first and words it less helpfully.
//  2. Run's signature from the AST, not the interpreter: asking yaegi for
//     main.Run needs Execute first, which runs init() and the package's var
//     initializers — exactly what a check must never do.
//  3. The lint is the one case that has cost real time: yaegi compiles and
//     runs a two-value assignment into a map element and stores nothing
//     (traefik/yaegi#1655). Without types it cannot tell a map from a
//     slice, so it flags both and says so; a slice there is rare.
//  4. yaegi's compile pass (type analysis and control flow, no code run)
//     catches what the parser cannot: an undefined name, a type mismatch,
//     an import a script cannot have. It is the same interpreter Run builds
//     (newInterp), so the two agree on what a script may import. It reports
//     only its first error, and does not catch everything the Go compiler
//     would (an unused variable passes).
//
// Diags come back sorted by position. A clean script is nil.
func Check(name, src string) []Diag {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, parser.ParseComments)
	if err != nil {
		return parseDiags(err)
	}
	var out []Diag
	out = append(out, checkSignature(fset, f)...)
	out = append(out, lintMapCommaOk(fset, f)...)
	out = append(out, lintStoreComputed(fset, f)...)
	out = append(out, compileDiags(src)...)
	slices.SortStableFunc(out, func(a, b Diag) int {
		if a.Line != b.Line {
			return a.Line - b.Line
		}
		return a.Col - b.Col
	})
	return out
}

// Diag is one problem Check found, at a 1-based line and column (column 0
// when only the line is known).
type Diag struct {
	Line     int    `json:"line"`
	Col      int    `json:"col"`
	Severity string `json:"severity"` // SevError or SevWarning
	Msg      string `json:"msg"`
}

// The two severities. An error means Run would fail (or never start); a
// warning is legal Go that probably does not do what was meant.
const (
	SevError   = "error"
	SevWarning = "warning"
)

// HasError reports whether any diag is an error rather than a warning.
func HasError(diags []Diag) bool {
	return slices.ContainsFunc(diags, func(d Diag) bool { return d.Severity == SevError })
}

// String is the diag as a compiler prints one, after the file name:
// "12:5: undefined: foo", with "warning: " before a warning's message.
func (d Diag) String() string {
	msg := d.Msg
	if d.Severity == SevWarning {
		msg = "warning: " + msg
	}
	return fmt.Sprintf("%d:%d: %s", d.Line, d.Col, msg)
}

// parseDiags turns go/parser's error (a scanner.ErrorList, normally) into
// diags.
func parseDiags(err error) []Diag {
	var list scanner.ErrorList
	if !errors.As(err, &list) {
		return []Diag{{Line: 1, Severity: SevError, Msg: err.Error()}}
	}
	out := make([]Diag, 0, len(list))
	for _, e := range list {
		out = append(out, Diag{Line: e.Pos.Line, Col: e.Pos.Column, Severity: SevError, Msg: e.Msg})
	}
	return out
}

// sdbPath is the sdb package's import path, as scripts write it.
const sdbPath = "github.com/rohanthewiz/dbc/sdb"

// wantRun is the signature dbc calls, as the messages spell it.
const wantRun = "func Run(s *sdb.S) error"

// checkSignature checks what Run (engine.go) will ask of the script: that
// it is package main and has a top-level func Run(s *sdb.S) error. The sdb
// import may be renamed (import db "…/sdb" makes it *db.S) or dot-imported
// (*S); the parameter's own name does not matter.
func checkSignature(fset *token.FileSet, f *ast.File) []Diag {
	if f.Name.Name != "main" {
		p := fset.Position(f.Name.Pos())
		return []Diag{{Line: p.Line, Col: p.Column, Severity: SevError,
			Msg: fmt.Sprintf("package %s: a script is package main", f.Name.Name)}}
	}
	sdbName := "" // how this file refers to the sdb package; "" if not imported
	for _, im := range f.Imports {
		if path, _ := strconv.Unquote(im.Path.Value); path == sdbPath {
			sdbName = "sdb"
			if im.Name != nil {
				sdbName = im.Name.Name
			}
		}
	}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Name.Name != "Run" {
			continue
		}
		if isRunSignature(fn.Type, sdbName) {
			return nil
		}
		p := fset.Position(fn.Name.Pos())
		var got bytes.Buffer
		_ = printer.Fprint(&got, fset, fn.Type)
		return []Diag{{Line: p.Line, Col: p.Column, Severity: SevError,
			Msg: fmt.Sprintf("Run must be %s, not %s", wantRun,
				strings.Replace(got.String(), "func", "func Run", 1))}}
	}
	p := fset.Position(f.Package)
	return []Diag{{Line: p.Line, Col: p.Column, Severity: SevError,
		Msg: "no " + wantRun + ": it is what dbc calls to run the script"}}
}

// isRunSignature reports whether t is func(*<sdbName>.S) error, with any
// parameter name (or none).
func isRunSignature(t *ast.FuncType, sdbName string) bool {
	if t.TypeParams != nil || t.Params == nil || len(t.Params.List) != 1 ||
		len(t.Params.List[0].Names) > 1 || t.Results == nil || len(t.Results.List) != 1 ||
		len(t.Results.List[0].Names) > 1 {
		return false
	}
	if res, ok := t.Results.List[0].Type.(*ast.Ident); !ok || res.Name != "error" {
		return false
	}
	star, ok := t.Params.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	switch x := star.X.(type) {
	case *ast.SelectorExpr: // *sdb.S, or *alias.S
		pkg, ok := x.X.(*ast.Ident)
		return ok && sdbName != "" && sdbName != "." && pkg.Name == sdbName && x.Sel.Name == "S"
	case *ast.Ident: // *S, with sdb dot-imported
		return sdbName == "." && x.Name == "S"
	}
	return false
}

// lintMapCommaOk flags a two-value assignment into an index expression:
//
//	m[k], _ = v.(string)     m[k], ok = other[k]     m[k], err = f()
//
// yaegi v0.16.1 compiles each of these and stores nothing in m (and a
// channel receive there panics); see traefik/yaegi#1655. `:=` cannot have
// an index on its left, so only `=` is looked at. A slice element would be
// fine, but without type information it looks the same, so the message
// says so rather than guess.
func lintMapCommaOk(fset *token.FileSet, f *ast.File) []Diag {
	var out []Diag
	ast.Inspect(f, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || as.Tok != token.ASSIGN || len(as.Lhs) != 2 || len(as.Rhs) != 1 {
			return true
		}
		for _, l := range as.Lhs {
			if _, isIndex := l.(*ast.IndexExpr); isIndex {
				p := fset.Position(l.Pos())
				out = append(out, Diag{Line: p.Line, Col: p.Column, Severity: SevWarning,
					Msg: "the script interpreter drops a two-value assignment into a map element " +
						"(yaegi#1655): assign to a variable, then m[k] = v. (A slice element is fine.)"})
			}
		}
		return true
	})
	return out
}

// lintStoreComputed flags an operator's result stored straight into an
// element of a two-level index or a selector's index — the shape of a
// batch's or a result's rows:
//
//	b.Rows[i][c] = s + "!"      rows[i][j] = n * 2      b.Rows[i][c] = -n
//
// yaegi v0.16.1 writes the result of a binary (+ - * / …, not a
// comparison) or unary operator stored into an element of a []any it was
// handed by compiled code into the wrong frame slot: the element keeps
// its old value, and a local — often the batch itself — is overwritten,
// so a transform's `return b, nil` returns nil and its rows are silently
// dropped. Through a call's result, a variable, or a conversion the
// value lands where it should. Without types the lint cannot tell such a
// slice from one the code made itself (which works), so it looks only at
// the two shapes rows come in and says what to do either way. A row
// taken into a variable first (row := b.Rows[i]; row[c] = x + 1) is
// affected too, and not caught.
func lintStoreComputed(fset *token.FileSet, f *ast.File) []Diag {
	var out []Diag
	ast.Inspect(f, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || as.Tok != token.ASSIGN || len(as.Lhs) != len(as.Rhs) {
			return true
		}
		for i, l := range as.Lhs {
			ix, ok := l.(*ast.IndexExpr)
			if !ok || !rowShaped(ix.X) || !computed(as.Rhs[i]) {
				continue
			}
			p := fset.Position(l.Pos())
			out = append(out, Diag{Line: p.Line, Col: p.Column, Severity: SevWarning,
				Msg: "the script interpreter mis-stores an operator's result put straight into a row's element: " +
					"the row keeps its old value and a local (often the batch) is overwritten. " +
					"Put it in a variable first (v := …; then assign v) or wrap it in any(…)."})
		}
		return true
	})
	return out
}

// rowShaped reports whether x, the indexed part of an element store, is
// rows[i] or a.B (b.Rows, r.Raw): what an element of a row is reached
// through.
func rowShaped(x ast.Expr) bool {
	switch ast.Unparen(x).(type) {
	case *ast.IndexExpr, *ast.SelectorExpr:
		return true
	}
	return false
}

// computed reports whether e is an operator's result: a binary
// expression other than a comparison, or a unary one other than & and <-.
func computed(e ast.Expr) bool {
	switch e := ast.Unparen(e).(type) {
	case *ast.BinaryExpr:
		switch e.Op {
		case token.EQL, token.NEQ, token.LSS, token.LEQ, token.GTR, token.GEQ, token.LAND, token.LOR:
			return false
		}
		return true
	case *ast.UnaryExpr:
		return e.Op != token.AND && e.Op != token.ARROW && e.Op != token.NOT
	}
	return false
}

// yaegiPosRe finds the position yaegi puts at the front of an error:
// "6:2: undefined selector: Nope", sometimes after a file name.
var yaegiPosRe = regexp.MustCompile(`(?s)^(?:[^\n]*?:)?(\d+):(\d+): (.*)$`)

// yaegiImportRe is yaegi's error for an import it has no symbols for, which
// goes on to give GOPATH advice that does not apply inside dbc.
var yaegiImportRe = regexp.MustCompile(`^import "([^"]*)" error: unable to find source`)

// compileDiags runs yaegi's compile pass over src. Compile parses, type
// checks and builds the control flow; it does not Execute, so nothing in
// the script runs. A panic inside yaegi (it has a few on odd input) is a
// diag, not a crash of whoever asked.
func compileDiags(src string) (out []Diag) {
	defer func() {
		if r := recover(); r != nil {
			out = []Diag{{Line: 1, Severity: SevError, Msg: fmt.Sprintf("the script interpreter failed on this script: %v", r)}}
		}
	}()
	i, err := newInterp()
	if err != nil {
		return []Diag{{Line: 1, Severity: SevError, Msg: err.Error()}}
	}
	prog, err := i.Compile(src)
	if err == nil {
		if prog == nil {
			// yaegi skips a file a legacy "// +build" line rules out,
			// silently; Run would then find no main.Run
			return []Diag{{Line: 1, Severity: SevError,
				Msg: "the script interpreter skipped this file: a // +build line excludes it (use //go:build ignore)"}}
		}
		return nil
	}
	return []Diag{yaegiDiag(err.Error())}
}

// yaegiDiag turns one yaegi error message into a diag: its position, when it
// has one (else line 1, the message intact), and a plainer message for an
// import a script cannot have.
func yaegiDiag(msg string) Diag {
	d := Diag{Line: 1, Severity: SevError, Msg: msg}
	if m := yaegiPosRe.FindStringSubmatch(msg); m != nil {
		d.Line, _ = strconv.Atoi(m[1])
		d.Col, _ = strconv.Atoi(m[2])
		d.Msg = m[3]
	}
	if m := yaegiImportRe.FindStringSubmatch(d.Msg); m != nil {
		d.Msg = fmt.Sprintf("cannot import %q: a script can import the standard library and %s", m[1], sdbPath)
	}
	return d
}
