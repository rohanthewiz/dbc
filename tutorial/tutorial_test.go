package tutorial

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// docText reads the tutorial's Markdown from the repo root.
func docText(t *testing.T) []string {
	t.Helper()
	bs, err := os.ReadFile(filepath.Join("..", filepath.FromSlash(DocPath)))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(string(bs), "\n")
}

// docAnchors names, for each embedded file the doc shows, the text of the
// line that introduces its code block. The block is the first fenced one
// after that line. bad.go is the one file the doc doesn't show.
var docAnchors = map[string]string{
	"dbc.toml":                       "`dbc.toml`:",
	"seed.sql":                       "Save this as `seed.sql`:",
	"scripts/order_summary.go":       "`scripts/order_summary.go`:",
	"scripts/region_report.go":       "`scripts/region_report.go` writes",
	"scripts/load_warehouse.go":      "`scripts/load_warehouse.go`:",
	"pipelines/shop-sync.json":       "`pipelines/shop-sync.json` has",
	"plugins/order_tier.go":          "`plugins/order_tier.go`:",
	"pipelines/order-tiers.json":     "`pipelines/order-tiers.json`:",
	"pipelines/warehouse-facts.json": "`pipelines/warehouse-facts.json`:",
	"pipelines/revenue-report.json":  "`pipelines/revenue-report.json`:",
	"jobs/nightly-shop.json":         "`jobs/nightly-shop.json`:",
	"scripts/orchestrate.go":         "`scripts/orchestrate.go`:",
}

// notInDoc are embedded files the doc mentions without showing.
var notInDoc = []string{"bad.go"}

// The embedded workspace is the doc's code, byte for byte: a change to
// either side alone fails here, so `--run` never writes something other
// than what the learner reads.
func TestWorkspaceMatchesDoc(t *testing.T) {
	doc := docText(t)
	for _, name := range FileNames() {
		if slices.Contains(notInDoc, name) {
			continue
		}
		anchor, ok := docAnchors[name]
		if !ok {
			t.Errorf("%s is embedded but has no doc anchor: add it to docAnchors (or notInDoc)", name)
			continue
		}
		block, err := blockAfter(doc, anchor)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		bs, _ := File(name)
		if string(bs) != block {
			t.Errorf("%s differs from its code block in %s (after %q): update one to match the other", name, DocPath, anchor)
		}
	}
	for name := range docAnchors {
		if _, err := File(name); err != nil {
			t.Errorf("%s has a doc anchor but isn't embedded", name)
		}
	}
}

// blockAfter is the body of the first fenced block after the first line
// containing anchor, with a trailing newline as a file has.
func blockAfter(doc []string, anchor string) (string, error) {
	at := slices.IndexFunc(doc, func(l string) bool { return strings.Contains(l, anchor) })
	if at < 0 {
		return "", fmt.Errorf("no line containing %q", anchor)
	}
	open := -1
	for i := at + 1; i < len(doc); i++ {
		if strings.HasPrefix(doc[i], "```") {
			open = i
			break
		}
	}
	if open < 0 {
		return "", fmt.Errorf("no code block after %q", anchor)
	}
	for i := open + 1; i < len(doc); i++ {
		if doc[i] == "```" {
			return strings.Join(doc[open+1:i], "\n") + "\n", nil
		}
	}
	return "", fmt.Errorf("unclosed code block after %q", anchor)
}

// Each lesson is a doc section with the same number and heading, and each
// numbered section has a lesson.
func TestLessonsMatchDocHeadings(t *testing.T) {
	heading := regexp.MustCompile(`^## (\d+)\. (.+)$`)
	var docHeads []string
	for _, l := range docText(t) {
		if m := heading.FindStringSubmatch(l); m != nil {
			docHeads = append(docHeads, m[1]+". "+m[2])
		}
	}
	var lessonHeads []string
	for _, l := range Lessons() {
		lessonHeads = append(lessonHeads, fmt.Sprintf("%d. %s", l.N, l.Title))
	}
	if !slices.Equal(docHeads, lessonHeads) {
		t.Errorf("doc headings and lessons differ:\n doc:     %q\n lessons: %q", docHeads, lessonHeads)
	}
}

// Every embedded file belongs to exactly one lesson, and every lesson's
// file is embedded.
func TestLessonFilesEmbedded(t *testing.T) {
	owner := map[string]int{}
	for _, l := range Lessons() {
		for _, f := range l.Files {
			if prev, ok := owner[f]; ok {
				t.Errorf("%s is in lessons %d and %d", f, prev, l.N)
			}
			owner[f] = l.N
			if _, err := File(f); err != nil {
				t.Errorf("lesson %d: %v", l.N, err)
			}
		}
	}
	for _, f := range FileNames() {
		if _, ok := owner[f]; !ok {
			t.Errorf("%s is embedded but no lesson writes it", f)
		}
	}
}

func TestAnchor(t *testing.T) {
	for n, want := range map[int]string{
		0: "0-set-up-a-workspace",
		4: "4-one-command-etl-dbc-copy",
		5: "5-etl-in-a-script-copy-reader-writer",
		6: "6-pipelines-etl-as-data",
	} {
		l, _ := Find(n)
		if got := l.Anchor(); got != want {
			t.Errorf("lesson %d anchor = %q, want %q", n, got, want)
		}
	}
}

func TestCommandLine(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"script", "order_summary"}, "dbc script order_summary"},
		{[]string{"-t", "markdown", "SELECT * FROM orders"}, `dbc -t markdown "SELECT * FROM orders"`},
		{[]string{"--where", "status = 'paid'"}, `dbc --where "status = 'paid'"`},
		{[]string{"-p", "min_total=30"}, "dbc -p min_total=30"},
		{[]string{"echo $HOME's"}, `dbc 'echo $HOME'\''s'`},
		{[]string{""}, `dbc ""`},
	} {
		if got := CommandLine(c.args); got != c.want {
			t.Errorf("CommandLine(%q) = %s, want %s", c.args, got, c.want)
		}
	}
}

func TestLastLine(t *testing.T) {
	log := `time="2026-10-12T20:39:12Z" level=error msg="duplicate primary key" conn=warehouse ` +
		`error="duplicate primary key" fields.msg="copy failed" stmt="INSERT INTO \"orders\""`
	if got := lastLine("noise\n"+log+"\n\n", "x"); got != "duplicate primary key" {
		t.Errorf("structured line = %q", got)
	}
	twoPart := `level=error msg="script failed" error="no such table: customers"`
	if got := lastLine(twoPart, "x"); got != "script failed: no such table: customers" {
		t.Errorf("msg+error = %q", got)
	}
	if got := lastLine("bad.go:15:3: warning: map\n", "x"); got != "bad.go:15:3: warning: map" {
		t.Errorf("plain line = %q", got)
	}
	if got := lastLine(" \n", "none"); got != "none" {
		t.Errorf("empty = %q", got)
	}
}

// Init writes lesson 0's files and the directories, and keeps a file the
// learner changed unless forced.
func TestInitAndWriteFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ws")
	if err := Init(dir, false, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, d := range Dirs {
		if fi, err := os.Stat(filepath.Join(dir, d)); err != nil || !fi.IsDir() {
			t.Errorf("no %s/ after Init", d)
		}
	}
	if err := CheckWorkspace(dir); err != nil {
		t.Errorf("Init's workspace isn't one: %v", err)
	}

	seed := filepath.Join(dir, "seed.sql")
	if err := os.WriteFile(seed, []byte("-- mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if was, err := WriteFile(dir, "seed.sql", false); err != nil || was != Kept {
		t.Errorf("WriteFile over an edit = %v %v, want Kept", was, err)
	}
	if bs, _ := os.ReadFile(seed); string(bs) != "-- mine\n" {
		t.Errorf("the edit was overwritten: %q", bs)
	}
	if was, err := WriteFile(dir, "seed.sql", true); err != nil || was != Replaced {
		t.Errorf("forced WriteFile = %v %v, want Replaced", was, err)
	}
	want, _ := File("seed.sql")
	if bs, _ := os.ReadFile(seed); !bytes.Equal(bs, want) {
		t.Errorf("forced WriteFile left %q", bs)
	}
	if was, err := WriteFile(dir, "scripts/order_summary.go", false); err != nil || was != Created {
		t.Errorf("new file = %v %v, want Created", was, err)
	}
}

func TestCheckWorkspace(t *testing.T) {
	dir := t.TempDir()
	if err := CheckWorkspace(dir); err == nil || !strings.Contains(err.Error(), "no dbc.toml") {
		t.Errorf("empty dir: %v", err)
	}
	cfg := "[[connection]]\nname = \"shop\"\ndriver = \"sqlite\"\ndsn = \"file:x.db\"\n"
	if err := os.WriteFile(filepath.Join(dir, "dbc.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckWorkspace(dir); err == nil || !strings.Contains(err.Error(), "no warehouse connection") {
		t.Errorf("shop only: %v", err)
	}
}

// fake is a Runner that answers from a table keyed by the joined args,
// and records every call.
type fake struct {
	answers map[string]answer
	calls   []string
}

type answer struct {
	out, err string
	code     int
}

func (f *fake) run(_ context.Context, _ string, args []string, stdout, stderr io.Writer) (int, error) {
	key := strings.Join(args, " ")
	f.calls = append(f.calls, key)
	a, ok := f.answers[key]
	if !ok {
		return 1, nil // unknown: a failed dbc, so a missing answer shows as a failed check
	}
	io.WriteString(stdout, a.out)
	io.WriteString(stderr, a.err)
	return a.code, nil
}

func fakeEnv(t *testing.T, answers map[string]answer) (*Env, *fake) {
	f := &fake{answers: answers}
	return &Env{Ctx: context.Background(), Dir: t.TempDir(), Dbc: f.run}, f
}

func TestRowCountChecks(t *testing.T) {
	e, _ := fakeEnv(t, map[string]answer{
		"-c warehouse -t json SELECT count(*) AS n FROM orders":   {out: `[{"n": 6}]`},
		"-c warehouse -t json SELECT count(*) AS n FROM load_log": {out: `[{"n": 0}]`},
		"-c warehouse -t json SELECT count(*) AS n FROM gone":     {err: `level=error msg="no such table" error="no such table"`, code: 1},
	})
	if err := rowsEqual("warehouse", "orders", 6)(e); err != nil {
		t.Errorf("6 = 6: %v", err)
	}
	if err := rowsEqual("warehouse", "orders", 4)(e); err == nil || !strings.Contains(err.Error(), "has 6 rows, want 4") {
		t.Errorf("6 vs 4: %v", err)
	}
	if err := rowsAtLeast("warehouse", "load_log", 1)(e); err == nil || !strings.Contains(err.Error(), "want at least 1") {
		t.Errorf("0 >= 1: %v", err)
	}
	if err := rowsEqual("warehouse", "gone", 1)(e); err == nil || err.Error() != "warehouse.gone: no such table" {
		t.Errorf("missing table: %v", err)
	}
}

func TestCleanAndFileChecks(t *testing.T) {
	e, _ := fakeEnv(t, map[string]answer{
		"pipeline check good": {},
		"pipeline check bad":  {out: "bad.json: node dst: conn is required\n", code: 1},
	})
	if err := clean("pipeline", "check", "good")(e); err != nil {
		t.Errorf("clean: %v", err)
	}
	err := clean("pipeline", "check", "bad")(e)
	if err == nil || !strings.Contains(err.Error(), "exited 1: bad.json: node dst: conn is required") {
		t.Errorf("unclean: %v", err)
	}

	os.MkdirAll(filepath.Join(e.Dir, "data", "exports"), 0o755)
	if err := fileMatches("data/exports/big_orders_*.csv")(e); err == nil {
		t.Error("glob matched in an empty dir")
	}
	os.WriteFile(filepath.Join(e.Dir, "data", "exports", "big_orders_2026-10-12.csv"), nil, 0o644)
	if err := fileMatches("data/exports/big_orders_*.csv")(e); err != nil {
		t.Errorf("glob: %v", err)
	}
	if err := filesExist("data/exports/big_orders_2026-10-12.csv", "data/x.csv")(e); err == nil || err.Error() != "missing data/x.csv" {
		t.Errorf("filesExist: %v", err)
	}
}

func TestRunRecordCheck(t *testing.T) {
	e, _ := fakeEnv(t, map[string]answer{
		"runs -t json --job nightly-shop --status succeeded": {out: `[{"id":"a","trigger":"cli","status":"succeeded"}]`},
	})
	if err := runRecord("nightly-shop", "")(e); err != nil {
		t.Errorf("any trigger: %v", err)
	}
	if err := runRecord("nightly-shop", "script")(e); err == nil || !strings.Contains(err.Error(), "trigger script") {
		t.Errorf("script trigger: %v", err)
	}
}

// testLesson is a lesson over the fake: one file, a command that fails on
// purpose, one that must pass, and a row-count check.
func testLesson() Lesson {
	return Lesson{
		N: 0, Title: "Set up a workspace",
		Files: []string{"seed.sql"},
		Commands: []Command{
			{Args: []string{"copy", "again"}, ExpectFail: true},
			{Args: []string{"-f", "seed.sql"}},
		},
		Checks: []Check{{What: "shop has 6 orders", Hint: "seed it", Test: rowsEqual("shop", "orders", 6)}},
	}
}

const countOrders = "-c shop -t json SELECT count(*) AS n FROM orders"

func TestRunLesson(t *testing.T) {
	e, f := fakeEnv(t, map[string]answer{
		"copy again":  {err: "duplicate primary key\n", code: 1},
		"-f seed.sql": {out: "seeded\n"},
	})
	var out bytes.Buffer
	// the check fails before the run (no answer for it), so the lesson runs;
	// the answer is added once the commands have run, as the seed would
	ran := false
	e.Dbc = func(ctx context.Context, dir string, args []string, stdout, stderr io.Writer) (int, error) {
		if strings.Join(args, " ") == "-f seed.sql" {
			ran = true
			f.answers[countOrders] = answer{out: `[{"n":6}]`}
		}
		return f.run(ctx, dir, args, stdout, stderr)
	}
	if !Run(e, testLesson(), false, &out, io.Discard) {
		t.Fatalf("Run failed:\n%s", &out)
	}
	if !ran {
		t.Error("the seed didn't run")
	}
	for _, want := range []string{"+ seed.sql", "$ dbc copy again", "exit 1, as expected", "$ dbc -f seed.sql", "seeded", "✓ shop has 6 orders"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, &out)
		}
	}
	if _, err := os.Stat(filepath.Join(e.Dir, "seed.sql")); err != nil {
		t.Errorf("seed.sql not written: %v", err)
	}

	// Done now: a second Run runs nothing.
	f.calls = nil
	out.Reset()
	if !Run(e, testLesson(), false, &out, io.Discard) {
		t.Fatal("second Run failed")
	}
	if !strings.Contains(out.String(), "done already") || slices.Contains(f.calls, "-f seed.sql") {
		t.Errorf("a done lesson ran again: calls %q\n%s", f.calls, &out)
	}
}

// A command that fails unexpectedly stops the lesson before the next one.
func TestRunLessonStopsOnFailure(t *testing.T) {
	e, f := fakeEnv(t, map[string]answer{
		"copy again": {code: 1},
		// "-f seed.sql" has no answer: it fails
	})
	l := testLesson()
	l.Commands = append(l.Commands, Command{Args: []string{"never"}})
	var out bytes.Buffer
	if Run(e, l, false, &out, io.Discard) {
		t.Fatal("Run passed with a failing command")
	}
	if slices.Contains(f.calls, "never") {
		t.Error("ran past the failure")
	}
	if !strings.Contains(out.String(), "the lesson stops here") {
		t.Errorf("no stop message:\n%s", &out)
	}
}

func TestCheckLessonReportsHint(t *testing.T) {
	e, _ := fakeEnv(t, map[string]answer{countOrders: {out: `[{"n":2}]`}})
	var out bytes.Buffer
	if CheckLesson(e, testLesson(), &out) {
		t.Fatal("passed with 2 orders")
	}
	for _, want := range []string{"✗ shop has 6 orders", "found: shop.orders has 2 rows, want 6", "hint:  seed it"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, &out)
		}
	}
	out.Reset()
	if !CheckLesson(e, Lesson{N: 11, Title: "UIs"}, &out) || !strings.Contains(out.String(), "nothing to check") {
		t.Errorf("a lesson without checks: %s", &out)
	}
}

func TestShow(t *testing.T) {
	l, _ := Find(1)
	var out bytes.Buffer
	Show(&out, t.TempDir(), l, true)
	for _, want := range []string{
		"§1  Your first script", "#1-your-first-script",
		"+ scripts/order_summary.go", "$ dbc script order_summary",
		"── scripts/order_summary.go", "func Run(s *sdb.S) error",
		"dbc tutorial check 1", "dbc tutorial step 2",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("Show lacks %q:\n%s", want, &out)
		}
	}
}
