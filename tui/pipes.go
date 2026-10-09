package tui

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/config"
	"github.com/rohanthewiz/dbc/jobs"
	"github.com/rohanthewiz/dbc/pipeline"
	"github.com/rohanthewiz/dbc/scripts"
	"github.com/rohanthewiz/dbc/userdata"
)

// The Pipelines & jobs browser (Ctrl+J, or ⇉ in the scripts browser): the scripts
// browser (scripts.go) for the JSON specs dbc web draws on its canvases —
// jobs in jobs_dir, pipelines in pipelines_dir, the built-in examples, and
// both trashes — over the same stores (userdata/pipelines.go, jobs.go) and
// the same checks (pipeline.Check, jobs.CheckJob). The terminal does not
// draw a canvas: a spec is edited as JSON in $EDITOR and checked when the
// editor exits, and a run is watched as a tree (runs.go).
//
//	╭ Pipelines & jobs ─────────────────────────────────────────────────── ✕ ╮
//	│ ⌕ / filters by name and …      ƒ Scripts ^O   ◷ Runs ⌥J   + New       │
//	│  Jobs                                                                 │
//	│  nightly.json     Load, then sync … · ◷ 0 2 * * *              ● 3m12s │
//	│  Pipelines                                                            │
//	│  orders.json      Orders from prod into the warehouse              2h │
//	│  Examples · read-only — Enter runs one, d makes your own copy         │
//	│  nightly.json     The demo pipelines as a DAG …                    job │
//	│  copy-cats.json   Copy the demo cats table …                  pipeline │
//	│  Trash (1) ▸ t shows it                                               │
//	│ Enter run · p params · e edit · n new · d duplicate · r rename · …    │
//	╰───────────────────────────────────────────────────────────────────────╯
//
// THE KEYS ARE THE SCRIPTS BROWSER'S, so a hand that knows Ctrl+O knows
// this: the list has the keyboard, bare letters act, / enters the filter
// and Esc or Tab leaves it. Two are new: p runs with every parameter asked
// (Enter asks only for those with no default, so most runs start at once),
// and h lists the row's runs. (The plan sketched d for trash and r for
// the runs; the scripts browser's d duplicate and r rename won, since
// the two browsers sit side by side.)
//
// EACH ROW KIND HAS ONE THING ENTER DOES: a spec of yours runs; an example
// runs too — unlike a script example, a spec example is runnable as it
// is, by name, from `dbc pipeline run` — and d or e makes your own copy of
// it to change; a trashed spec is restored.
//
// A RUN OPENS ITS MONITOR (runs.go) in place of the browser; Esc there
// leaves the run going, shown on the status bar until it ends.

// specKind is which of the two kinds of spec a row is.
type specKind int

const (
	specPipeline specKind = iota + 1
	specJob
)

// word names the kind as a run record does ("pipeline", "job").
func (k specKind) word() string {
	if k == specJob {
		return jobs.KindJob
	}
	return jobs.KindPipeline
}

// pipeRowKind is what a row of the browser is.
type pipeRowKind int

const (
	prMine      pipeRowKind = iota + 1 // a spec in pipelines_dir or jobs_dir
	prExample                          // a built-in example: read-only, runnable
	prTrash                            // a trashed spec
	prTrashHead                        // the Trash heading: a click folds it
)

// pipeRow is a row's listItem.data: what picking it acts on.
type pipeRow struct {
	kind  pipeRowKind
	spec  specKind
	name  string // the file name, "orders.json"; a trashed spec's old name
	trash userdata.SpecTrashInfo
}

// pipeSel is where the browser opens its cursor: a spec of yours, by kind
// and file name. The zero value is "wherever".
type pipeSel struct {
	spec specKind
	name string
}

// specInfo is a spec as the browser lists it, either kind.
type specInfo struct {
	name, desc string
	mod        time.Time
	sched      []string // a job's cron lines
}

// trashed is a trashed spec with its kind.
type trashed struct {
	spec specKind
	info userdata.SpecTrashInfo
}

// pipesModal is the browser. As the scripts browser, it holds what it read
// when it opened (or last changed something) and narrows that by the
// filter.
type pipesModal struct {
	modalBase
	jobs, pipes  []specInfo
	jobExamples  []scripts.Job
	pipeExamples []scripts.Pipeline
	trash        []trashed
	showTrash    bool
	filter       *editor
	filtering    bool
	lst          *list
	// the header's chips: ƒ Scripts goes back to the twin browser (the
	// toolbar has no room for a button of this one's, so the scripts
	// browser has the way over), ◷ Runs opens the Runs list
	filterRect, newBtn, runsBtn, scriptsBtn Rect
	hoverBtn                                int // 0 none, 1 + New, 2 ◷ Runs, 3 ƒ Scripts
	now                                     func() time.Time
	pipesDir, jobsDir                       string
}

// openPipes opens the browser, its cursor on sel when that is listed.
func (m *Model) openPipes(sel pipeSel) {
	m.syncPlugins()
	pm := &pipesModal{filter: newEditor(true), lst: newList(), now: time.Now,
		pipesDir: m.cfg.PipelinesDir, jobsDir: m.cfg.JobsDir}
	pm.filter.placeholder = "/ filters by name and description"
	if err := pm.load(); err != nil {
		m.logf(logErr, "pipelines & jobs: %s", serr.StringFromErr(err))
		return
	}
	pm.build(m)
	pm.selectSpec(sel)
	m.openModal(pm)
}

// load reads both directories, both trashes and the examples.
func (pm *pipesModal) load() error {
	ps, err := userdata.ListPipelines(pm.pipesDir)
	if err != nil {
		return err
	}
	js, err := userdata.ListJobs(pm.jobsDir)
	if err != nil {
		return err
	}
	pm.pipes, pm.jobs = nil, nil
	for _, p := range ps {
		pm.pipes = append(pm.pipes, specInfo{name: p.Name, desc: p.Desc, mod: p.Mod})
	}
	for _, j := range js {
		pm.jobs = append(pm.jobs, specInfo{name: j.Name, desc: j.Desc, mod: j.Mod, sched: j.Schedule})
	}
	pm.trash = nil
	for _, t := range userdata.ListJobTrash(pm.jobsDir) {
		pm.trash = append(pm.trash, trashed{spec: specJob, info: t})
	}
	for _, t := range userdata.ListPipelineTrash(pm.pipesDir) {
		pm.trash = append(pm.trash, trashed{spec: specPipeline, info: t})
	}
	// newest trashed first, whichever kind
	slices.SortStableFunc(pm.trash, func(a, b trashed) int { return b.info.Trashed.Compare(a.info.Trashed) })
	pm.jobExamples, pm.pipeExamples = scripts.Jobs(), scripts.Pipelines()
	return nil
}

// reload re-reads after a change made from the browser, keeping the filter
// and the fold, the cursor on sel if given (else near where it was).
func (pm *pipesModal) reload(m *Model, sel pipeSel) {
	if err := pm.load(); err != nil {
		m.logf(logErr, "pipelines & jobs: %s", serr.StringFromErr(err))
	}
	cur := pm.lst.cur
	pm.build(m)
	pm.lst.cur = cur
	pm.lst.set(pm.lst.items)
	pm.selectSpec(sel)
}

// selectSpec puts the cursor on a spec of yours, if listed.
func (pm *pipesModal) selectSpec(sel pipeSel) {
	if sel.name == "" {
		return
	}
	for i, it := range pm.lst.items {
		if r, ok := it.data.(pipeRow); ok && r.kind == prMine && r.spec == sel.spec && r.name == sel.name {
			pm.lst.cur = i
			pm.lst.ensureVisible()
			return
		}
	}
}

// runningMark is the right column's word for a spec with a run going in
// this process ("● 3m12s"), or "".
func runningMark(m *Model, k specKind, file string) string {
	stem := strings.TrimSuffix(file, ".json")
	for _, r := range m.liveRuns {
		if r.Kind == k.word() && (r.Source == file || r.Name == stem) {
			if r.Status == pipeline.Queued {
				return "○ queued"
			}
			return "● " + shortDur(time.Since(r.Started))
		}
	}
	return ""
}

// build lays the rows out for the filter.
func (pm *pipesModal) build(m *Model) {
	q := strings.ToLower(strings.TrimSpace(pm.filter.Text()))
	hit := func(name, desc string) bool {
		return q == "" || strings.Contains(strings.ToLower(name), q) || strings.Contains(strings.ToLower(desc), q)
	}
	now := pm.now()
	var items []listItem
	head := func(label string, data any) { items = append(items, listItem{label: label, head: true, data: data}) }

	section := func(title string, k specKind, infos []specInfo, dir string) {
		head(title, nil)
		shown := 0
		for _, in := range infos {
			desc := in.desc
			if len(in.sched) > 0 {
				desc = strings.TrimSpace(desc + " · ◷ " + strings.Join(in.sched, "; "))
			}
			if !hit(in.name, desc) {
				continue
			}
			sub := runningMark(m, k, in.name)
			if sub == "" {
				sub = ago(in.mod, now)
			}
			items = append(items, listItem{label: in.name, desc: desc, sub: sub, data: pipeRow{kind: prMine, spec: k, name: in.name}})
			shown++
		}
		switch {
		case len(infos) == 0:
			head("  none yet in "+config.TildePath(dir)+" — n starts one; d on an example copies it", nil)
		case shown == 0:
			head("  no "+k.word()+" matches", nil)
		}
	}
	section("Jobs", specJob, pm.jobs, pm.jobsDir)
	section("Pipelines", specPipeline, pm.pipes, pm.pipesDir)

	var ex []listItem
	for _, x := range pm.jobExamples {
		if hit(x.Name, x.Desc) {
			ex = append(ex, listItem{label: x.Name, desc: x.Desc, sub: "job", data: pipeRow{kind: prExample, spec: specJob, name: x.Name}})
		}
	}
	for _, x := range pm.pipeExamples {
		if hit(x.Name, x.Desc) {
			ex = append(ex, listItem{label: x.Name, desc: x.Desc, sub: "pipeline", data: pipeRow{kind: prExample, spec: specPipeline, name: x.Name}})
		}
	}
	if len(ex) > 0 {
		head("Examples · read-only — Enter runs one, d makes your own copy", nil)
		items = append(items, ex...)
	}

	if len(pm.trash) > 0 {
		fold := "▸ t shows it"
		if pm.showTrash {
			fold = "▾"
		}
		head(fmt.Sprintf("Trash (%d) %s", len(pm.trash), fold), pipeRow{kind: prTrashHead})
		if pm.showTrash {
			for _, t := range pm.trash {
				if hit(t.info.Name, "") {
					items = append(items, listItem{label: t.info.Name, desc: t.spec.word() + " · trashed " + agoWords(t.info.Trashed, now),
						sub: "Enter restores", muted: true, data: pipeRow{kind: prTrash, spec: t.spec, name: t.info.Name, trash: t.info}})
				}
			}
		}
	}

	w := 0
	for _, it := range items {
		if !it.head {
			w = max(w, width(it.label))
		}
	}
	pm.lst.descCol = min(w, 32) + 2
	pm.lst.set(items)
}

// row is the row under the cursor, if it is a pickable one.
func (pm *pipesModal) row() (pipeRow, bool) {
	it, ok := pm.lst.current()
	if !ok {
		return pipeRow{}, false
	}
	r, ok := it.data.(pipeRow)
	return r, ok
}

func (pm *pipesModal) title() string { return "Pipelines & jobs" }

func (pm *pipesModal) size(w, h int) (int, int) {
	return max(min(w-4, 110), min(72, w-4)), max(12, min(len(pm.lst.items)+5, h*3/4))
}

func (pm *pipesModal) footer() string {
	if pm.filtering {
		return "typing filters · ↑↓ move · Enter: the row's action · Esc/Tab: back to the list"
	}
	// y (copy the path) is left to the right-click menu and F1: the line
	// fits the dialog's width whole this way
	return "Enter run · p params · e edit · n new · d duplicate · r rename · Del trash · h runs · t trash · / filter"
}

func (pm *pipesModal) draw(m *Model, s Surface) *caret {
	bg := m.st.panel
	s.Put(1, 0, "⌕", onBg(m.st.accent, bg))
	newLabel, runsLabel := " + New ", " ◷ Runs ⌥J "
	pm.newBtn = chip(s, s.W()-1-width(newLabel), 0, newLabel, pick(pm.hoverBtn == 1, m.st.buttonHover, m.st.button))
	pm.runsBtn = chip(s, pm.newBtn.X-s.Rect().X-1-width(runsLabel), 0, runsLabel, pick(pm.hoverBtn == 2, m.st.buttonHover, m.st.button))
	scriptsLabel := " ƒ Scripts ^O "
	pm.scriptsBtn = chip(s, pm.runsBtn.X-s.Rect().X-1-width(scriptsLabel), 0, scriptsLabel, pick(pm.hoverBtn == 3, m.st.buttonHover, m.st.button))
	field := s.Sub(Rect{3, 0, max(pm.scriptsBtn.X-s.Rect().X-5, 10), 1})
	pm.filterRect = field.Rect()
	fieldBg := bg
	if pm.filtering {
		fieldBg = m.st.raised
	}
	cx, cy, ok := pm.filter.Draw(field, m.st, fieldBg, [2]int{}, true)
	pm.lst.draw(s.Sub(Rect{0, 2, s.W(), s.H() - 3}), m.st, bg, true, "")
	s.Put(1, s.H()-1, truncate(pm.footer(), s.W()-2), onBg(m.st.muted, bg))
	if ok && pm.filtering {
		return &caret{cx, cy}
	}
	return nil
}

func (pm *pipesModal) key(m *Model, k tea.KeyPressMsg) (tea.Cmd, bool) {
	if pm.filtering {
		return pm.filterKey(m, k)
	}
	switch k.String() {
	case "esc":
		return nil, true
	case "/":
		pm.filtering = true
		return nil, false
	case "enter":
		return pm.act(m, false)
	case "p":
		return pm.run(m, true)
	case "e":
		return pm.act(m, true)
	case "n", "alt+n":
		pm.newMenu(m, pm.newBtn.X, pm.newBtn.Y+1)
		return nil, false
	case "d":
		return pm.duplicate(m)
	case "r", "f2":
		pm.rename(m)
		return nil, false
	case "delete", "x":
		pm.trashIt(m)
		return nil, false
	case "y":
		return pm.copyPath(m), false
	case "t":
		pm.toggleTrash(m)
		return nil, false
	case "h":
		return pm.runs(m), false
	case "alt+j":
		return m.openRuns("", ""), false
	case "ctrl+o":
		m.openScripts("") // the twin browser takes the dialog's place
		return nil, false
	}
	pm.lst.key(k)
	return nil, false
}

// filterKey is the keyboard while it is in the filter (the scripts
// browser's filterKey).
func (pm *pipesModal) filterKey(m *Model, k tea.KeyPressMsg) (tea.Cmd, bool) {
	switch k.String() {
	case "esc", "tab", "shift+tab":
		pm.filtering = false
		return nil, false
	case "enter":
		pm.filtering = false
		return pm.act(m, false)
	case "up", "down", "pgup", "pgdown", "ctrl+p", "ctrl+n":
		pm.lst.key(k)
		return nil, false
	}
	before := pm.filter.Text()
	pm.filter.HandleKey(k)
	if pm.filter.Text() != before {
		pm.lst.cur = 0
		pm.build(m)
	}
	return nil, false
}

func (pm *pipesModal) paste(m *Model, s string) {
	pm.filtering = true
	pm.filter.Insert(s)
	pm.lst.cur = 0
	pm.build(m)
}

func (pm *pipesModal) click(m *Model, x, y, clicks int, shift bool) (tea.Cmd, bool) {
	switch {
	case m.modalClose().Contains(x, y):
		return nil, true
	case pm.newBtn.Contains(x, y):
		pm.newMenu(m, pm.newBtn.X, pm.newBtn.Y+1)
		return nil, false
	case pm.runsBtn.Contains(x, y):
		return m.openRuns("", ""), false
	case pm.scriptsBtn.Contains(x, y):
		m.openScripts("")
		return nil, false
	case pm.filterRect.Contains(x, y):
		pm.filtering = true
		pm.filter.Click(x, y, clicks, shift)
		return nil, false
	}
	i := pm.lst.indexAt(x, y)
	if i < 0 {
		return nil, false
	}
	it := pm.lst.items[i]
	if it.head {
		if r, ok := it.data.(pipeRow); ok && r.kind == prTrashHead {
			pm.toggleTrash(m)
		}
		return nil, false
	}
	pm.lst.cur, pm.filtering = i, false
	if clicks >= 2 {
		// a double-click edits (an example: copies it to edit; a trashed
		// one: restores it), as in the scripts browser — never a run,
		// which a stray second click should not start
		return pm.act(m, true)
	}
	return nil, false
}

// rightClick opens the menu of what can be done with the row clicked.
func (pm *pipesModal) rightClick(m *Model, x, y int) tea.Cmd {
	i := pm.lst.indexAt(x, y)
	if i < 0 || pm.lst.items[i].head {
		return nil
	}
	pm.lst.cur, pm.filtering = i, false
	r, _ := pm.row()
	keep := func(f func(m *Model) (tea.Cmd, bool)) func(m *Model) tea.Cmd {
		return func(m *Model) tea.Cmd { cmd, _ := f(m); return cmd }
	}
	var items []menuItem
	switch r.kind {
	case prMine:
		items = []menuItem{
			heading(r.name + " · " + r.spec.word()),
			{label: "▶ Run", key: "Enter", act: keep(func(m *Model) (tea.Cmd, bool) { return pm.run(m, false) })},
			{label: "▶ Run with parameters…", key: "p", act: keep(func(m *Model) (tea.Cmd, bool) { return pm.run(m, true) })},
			{label: "✎ Edit in " + editorName(), key: "e", act: keep(func(m *Model) (tea.Cmd, bool) { return pm.act(m, true) })},
			{label: "◷ Its runs", key: "h", act: func(m *Model) tea.Cmd { return pm.runs(m) }},
			{label: "Duplicate…", key: "d", act: keep(pm.duplicate)},
			{label: "Rename…", key: "r", act: func(m *Model) tea.Cmd { pm.rename(m); return nil }},
			{label: "Move to the trash", key: "Del", act: func(m *Model) tea.Cmd { pm.trashIt(m); return nil }},
			{label: "Copy path", key: "y", act: func(m *Model) tea.Cmd { return pm.copyPath(m) }},
		}
	case prExample:
		items = []menuItem{
			heading(r.name + " · " + r.spec.word() + " example"),
			{label: "▶ Run", key: "Enter", act: keep(func(m *Model) (tea.Cmd, bool) { return pm.run(m, false) })},
			{label: "▶ Run with parameters…", key: "p", act: keep(func(m *Model) (tea.Cmd, bool) { return pm.run(m, true) })},
			{label: "⧉ Copy into mine…", key: "d", act: keep(pm.duplicate)},
			{label: "◷ Its runs", key: "h", act: func(m *Model) tea.Cmd { return pm.runs(m) }},
		}
	case prTrash:
		items = []menuItem{heading(r.name),
			{label: "↺ Restore", key: "Enter", act: keep(func(m *Model) (tea.Cmd, bool) { return pm.act(m, false) })}}
	default:
		return nil
	}
	m.openMenu(x, y, items)
	return nil
}

func (pm *pipesModal) hover(m *Model, x, y int) {
	pm.lst.hover = pm.lst.indexAt(x, y)
	if pm.lst.hover >= 0 && pm.lst.items[pm.lst.hover].head {
		pm.lst.hover = -1
	}
	pm.hoverBtn = 0
	switch {
	case pm.newBtn.Contains(x, y):
		pm.hoverBtn = 1
	case pm.runsBtn.Contains(x, y):
		pm.hoverBtn = 2
	case pm.scriptsBtn.Contains(x, y):
		pm.hoverBtn = 3
	}
}

func (pm *pipesModal) wheel(m *Model, x, y, dy int) { pm.lst.scroll(dy) }

func (pm *pipesModal) toggleTrash(m *Model) {
	if len(pm.trash) == 0 {
		m.log(logMuted, "the trash is empty")
		return
	}
	pm.showTrash = !pm.showTrash
	pm.build(m)
}

// act does what the row under the cursor is for: run it (edit: open it in
// the editor instead — an example is copied first), or restore it.
func (pm *pipesModal) act(m *Model, edit bool) (tea.Cmd, bool) {
	r, ok := pm.row()
	if !ok {
		return nil, false
	}
	switch r.kind {
	case prMine:
		if edit {
			return m.editSpec(r.spec, r.name), true
		}
		return pm.run(m, false)
	case prExample:
		if edit {
			return pm.duplicate(m)
		}
		return pm.run(m, false)
	case prTrash:
		m.restoreSpec(r.spec, r.trash)
		return nil, false
	}
	return nil, false
}

// run runs the spec under the cursor, asking for its parameters first —
// every one when all, else only those with no default.
func (pm *pipesModal) run(m *Model, all bool) (tea.Cmd, bool) {
	r, ok := pm.row()
	if !ok || (r.kind != prMine && r.kind != prExample) {
		if ok {
			m.log(logWarn, "a trashed "+r.spec.word()+" does not run — Enter restores it first")
		}
		return nil, false
	}
	text, err := m.specText(r)
	if err != nil {
		m.logf(logErr, "could not read %s: %s", r.name, serr.StringFromErr(err))
		return nil, false
	}
	return m.runSpec(r.spec, r.name, text, all), false
}

// runs is h: the Runs list narrowed to the row's job or pipeline. A run
// record names the spec by its own name, which a spec made here shares
// with its file, but the file's text is asked, in case it does not.
func (pm *pipesModal) runs(m *Model) tea.Cmd {
	r, ok := pm.row()
	if !ok || r.kind == prTrash {
		return m.openRuns("", "")
	}
	name := strings.TrimSuffix(r.name, ".json")
	if text, err := m.specText(r); err == nil {
		name = specName(r.spec, text, name)
	}
	return m.openRuns(r.spec.word(), name)
}

// newMenu offers the two kinds of spec, as dbc web's "+ New ▾".
func (pm *pipesModal) newMenu(m *Model, x, y int) {
	m.openMenu(x, y, []menuItem{
		heading("new"),
		{label: "Pipeline — a source into a preview, to build on", act: func(m *Model) tea.Cmd { m.newSpec(specPipeline); return nil }},
		{label: "Job — pipelines in a DAG, run by hand or on a schedule", act: func(m *Model) tea.Cmd { m.newSpec(specJob); return nil }},
	})
}

// mineOnly is the row under the cursor when it is a spec of yours, or a log
// line saying the action is for those.
func (pm *pipesModal) mineOnly(m *Model, verb string) (pipeRow, bool) {
	r, ok := pm.row()
	if !ok {
		return r, false
	}
	if r.kind != prMine {
		what := "a built-in example (d makes your own copy)"
		if r.kind == prTrash {
			what = "in the trash (Enter restores it)"
		}
		m.logf(logWarn, "only a pipeline or job of yours can be %s — this row is %s", verbPast(verb), what)
		return r, false
	}
	return r, true
}

// duplicate copies the spec (or example) under the cursor under a new
// name, then edits the copy.
func (pm *pipesModal) duplicate(m *Model) (tea.Cmd, bool) {
	r, ok := pm.row()
	if !ok || r.kind == prTrash {
		if ok {
			m.log(logWarn, "a trashed "+r.spec.word()+" is restored with Enter, then duplicated")
		}
		return nil, false
	}
	text, err := m.specText(r)
	if err != nil {
		m.logf(logErr, "could not read %s: %s", r.name, serr.StringFromErr(err))
		return nil, false
	}
	what := "Duplicate " + r.name
	if r.kind == prExample {
		what = "Copy the example " + r.name
	}
	m.makeSpec(r.spec, r.name, text, what)
	return nil, false // the prompt has replaced the browser
}

// rename asks for the spec's new file name. The spec's own "name" inside
// is left as it is, as dbc web's rename leaves it.
func (pm *pipesModal) rename(m *Model) {
	r, ok := pm.mineOnly(m, "rename")
	if !ok {
		return
	}
	kit := m.specKit(r.spec)
	p := m.openPromptCmd("Rename "+r.name, specNameHint, "Rename", r.name, func(m *Model, typed string) (tea.Cmd, error) {
		to := jsonName(typed)
		if to == r.name {
			m.openPipes(pipeSel{r.spec, r.name})
			return nil, nil
		}
		if err := kit.rename(kit.dir, r.name, to); err != nil {
			return nil, specNameErr(kit, err, to)
		}
		m.logf(logOk, "renamed %s to %s", r.name, to)
		m.openPipes(pipeSel{r.spec, to})
		return nil, nil
	})
	selectJSONStem(p.field)
	p.cancel = func(m *Model) { m.openPipes(pipeSel{r.spec, r.name}) }
}

// trashIt moves the spec under the cursor to its trash; nothing is asked,
// as in the scripts browser — the Trash lists it, and Enter restores.
func (pm *pipesModal) trashIt(m *Model) {
	r, ok := pm.mineOnly(m, "trash")
	if !ok {
		return
	}
	kit := m.specKit(r.spec)
	if _, err := kit.trash(kit.dir, r.name); err != nil {
		m.logf(logErr, "could not trash %s: %s", r.name, serr.StringFromErr(err))
		return
	}
	m.logf(logInfo, "moved the %s %s to the trash — t shows the Trash, Enter there restores it", r.spec.word(), r.name)
	pm.reload(m, pipeSel{})
}

// copyPath copies the spec's absolute path.
func (pm *pipesModal) copyPath(m *Model) tea.Cmd {
	r, ok := pm.mineOnly(m, "copy")
	if !ok {
		return nil
	}
	return m.copyString(filepath.Join(m.specKit(r.spec).dir, r.name), "the "+r.spec.word()+"'s path")
}

// ---------------------------------------------------------------------------
// The two stores
// ---------------------------------------------------------------------------

// specKit is what differs between the kinds: where they live, and their
// store's functions and errors (userdata/pipelines.go, jobs.go — one specs
// store underneath, in two words).
type specKit struct {
	dir     string
	read    func(dir, name string) (string, string, error)
	save    func(dir, name, text, base string) (string, bool, error)
	rename  func(dir, from, to string) error
	trash   func(dir, name string) (string, error)
	restore func(dir, id, to string) (string, error)
	list    func(dir string) []string
	exists  error
	badName error
}

func (m *Model) specKit(k specKind) specKit {
	if k == specJob {
		return specKit{dir: m.cfg.JobsDir, read: userdata.ReadJob, save: userdata.SaveJob, rename: userdata.RenameJob,
			trash: userdata.TrashJob, restore: userdata.RestoreJob, exists: userdata.ErrJobExists, badName: userdata.ErrBadJobName,
			list: func(dir string) []string {
				infos, _ := userdata.ListJobs(dir)
				var out []string
				for _, in := range infos {
					out = append(out, in.Name)
				}
				return out
			}}
	}
	return specKit{dir: m.cfg.PipelinesDir, read: userdata.ReadPipeline, save: userdata.SavePipeline, rename: userdata.RenamePipeline,
		trash: userdata.TrashPipeline, restore: userdata.RestorePipeline, exists: userdata.ErrPipelineExists,
		badName: userdata.ErrBadPipelineName,
		list: func(dir string) []string {
			infos, _ := userdata.ListPipelines(dir)
			var out []string
			for _, in := range infos {
				out = append(out, in.Name)
			}
			return out
		}}
}

// specText is a row's spec text: the file of yours, or the example's.
func (m *Model) specText(r pipeRow) (string, error) {
	if r.kind == prExample {
		if r.spec == specJob {
			if ex, ok := scripts.JobByName(r.name); ok {
				return ex.Text, nil
			}
		} else if ex, ok := scripts.PipelineByName(r.name); ok {
			return ex.Text, nil
		}
		return "", serr.New("no such example", "name", r.name)
	}
	kit := m.specKit(r.spec)
	text, _, err := kit.read(kit.dir, r.name)
	return text, err
}

// specName is the name a spec gives itself (what its runs are recorded
// under), or def when the text does not parse.
func specName(k specKind, text, def string) string {
	if k == specJob {
		if s, err := jobs.ParseJob(text); err == nil && s.Name != "" {
			return s.Name
		}
		return def
	}
	if s, err := pipeline.Parse(text); err == nil && s.Name != "" {
		return s.Name
	}
	return def
}

// runSpec parses a spec, asks for the parameters it needs (all of them,
// when all) and starts its run. A spec that does not parse is said in the
// log, with the way to fix it.
func (m *Model) runSpec(k specKind, file, text string, all bool) tea.Cmd {
	var ps []paramSpec
	switch k {
	case specJob:
		spec, err := jobs.ParseJob(text)
		if err != nil {
			m.logf(logErr, "%s does not parse: %s — e edits it", file, strings.TrimPrefix(err.Error(), "json: "))
			return nil
		}
		ps = paramsOf(spec.Params)
	default:
		spec, err := pipeline.Parse(text)
		if err != nil {
			m.logf(logErr, "%s does not parse: %s — e edits it", file, strings.TrimPrefix(err.Error(), "json: "))
			return nil
		}
		ps = paramsOf(spec.Params)
	}
	if all && len(ps) == 0 {
		m.logf(logMuted, "%s has no parameters — running it", file)
	}
	back := func(m *Model) { m.openPipes(pipeSel{k, file}) }
	return m.askParams(strings.TrimSuffix(file, ".json"), ps, all, back, func(m *Model, values map[string]string) tea.Cmd {
		if k == specJob {
			return m.startJob(file, text, values)
		}
		return m.startPipeline(file, text, values)
	})
}

// ---------------------------------------------------------------------------
// Making, restoring and editing specs
// ---------------------------------------------------------------------------

const specNameHint = "letters, digits, '.', '-' and '_', ending in .json"

// jsonName makes what was typed a spec file name: trimmed, with ".json"
// added when left off.
func jsonName(s string) string {
	s = strings.TrimSpace(s)
	if s != "" && !strings.HasSuffix(strings.ToLower(s), ".json") {
		s += ".json"
	}
	return s
}

// freeSpecName is base, or base-2.json, base-3.json … — the first not in
// taken.
func freeSpecName(base string, taken []string) string {
	stem := strings.TrimSuffix(base, ".json")
	n := stem + ".json"
	for i := 2; slices.Contains(taken, n); i++ {
		n = fmt.Sprintf("%s-%d.json", stem, i)
	}
	return n
}

// selectJSONStem selects a "name.json" field's stem, so typing replaces the
// name and keeps the extension.
func selectJSONStem(ed *editor) {
	if t := ed.Text(); strings.HasSuffix(t, ".json") && !strings.Contains(t, "\n") {
		ed.anc, ed.cur, ed.sel = pos{0, 0}, pos{0, len([]rune(t)) - 5}, true
	}
}

// specNameErr is a store error as the name prompt shows it.
func specNameErr(kit specKit, err error, name string) error {
	switch {
	case errors.Is(err, kit.exists):
		return fmt.Errorf("%s exists — pick another name", name)
	case errors.Is(err, kit.badName) || !userdata.ValidPipelineName(name):
		return errors.New("not a name: " + specNameHint)
	}
	return errors.New(serr.StringFromErr(err))
}

// named is text with the spec's own "name" set to the file's stem, as dbc
// web makes them agree when it makes a spec: a job's step names its
// pipeline by file, and the runs are recorded by the spec's name, so a
// copy that kept the example's name would file its runs with the
// example's. Text that does not parse is kept as it is.
func named(k specKind, text, stem string) string {
	var out string
	var err error
	if k == specJob {
		var s *jobs.Spec
		if s, err = jobs.ParseJob(text); err == nil {
			s.Name = stem
			out, err = s.JSON()
		}
	} else {
		var s *pipeline.Spec
		if s, err = pipeline.Parse(text); err == nil {
			s.Name = stem
			out, err = s.JSON()
		}
	}
	if err != nil {
		return text
	}
	return out
}

// newSpec starts a spec of kind k from the smallest one that runs: a
// pipeline reading from the active connection into a preview (dbc web's
// new pipeline), or a job of one step that runs the first pipeline there
// is — so a new job checks out from its first save, and its file shows the
// shape to grow (after, params, triggers, policy).
func (m *Model) newSpec(k specKind) {
	if k == specJob {
		first := "copy-cats"
		if mine := m.specKit(specPipeline).list(m.cfg.PipelinesDir); len(mine) > 0 {
			first = strings.TrimSuffix(mine[0], ".json")
		} else if exs := scripts.Pipelines(); len(exs) > 0 {
			first = strings.TrimSuffix(exs[0].Name, ".json")
		}
		spec := &jobs.Spec{Name: "job", Desc: "", Pipelines: []jobs.Step{{ID: "first", Pipeline: first}}}
		text, err := spec.JSON()
		if err != nil {
			m.logf(logErr, "new job: %s", serr.StringFromErr(err))
			return
		}
		m.makeSpec(specJob, "job.json", text, "New job")
		return
	}
	conn := cmpOr(m.ws.Active(), m.cfg.DefaultConnection, config.DemoSQLite)
	spec := &pipeline.Spec{Name: "pipeline", Fragments: []pipeline.Fragment{{
		Name: "load",
		Nodes: []pipeline.Node{
			{ID: "src", Plugin: "sql.read", Cfg: pipeline.Config{"conn": conn, "query": "SELECT 1 AS one"}},
			{ID: "peek", Plugin: "preview", Cfg: pipeline.Config{"rows": "50"}},
		},
		Edges: []pipeline.Edge{{From: "src", To: "peek"}},
	}}}
	text, err := spec.JSON()
	if err != nil {
		m.logf(logErr, "new pipeline: %s", serr.StringFromErr(err))
		return
	}
	m.makeSpec(specPipeline, "pipeline.json", text, "New pipeline")
}

// cmpOr is the first of ss that is not "".
func cmpOr(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// makeSpec asks for a name (offered: suggest, made free), writes text as
// that new spec — its own name set to match — then edits it. Cancelling
// goes back to the browser.
func (m *Model) makeSpec(k specKind, suggest, text, title string) {
	kit := m.specKit(k)
	p := m.openPromptCmd(title, specNameHint, "Create", freeSpecName(suggest, kit.list(kit.dir)), func(m *Model, typed string) (tea.Cmd, error) {
		name := jsonName(typed)
		if !userdata.ValidPipelineName(name) {
			return nil, specNameErr(kit, kit.badName, name)
		}
		if _, _, err := kit.save(kit.dir, name, named(k, text, strings.TrimSuffix(name, ".json")), ""); err != nil {
			return nil, specNameErr(kit, err, name)
		}
		m.logf(logOk, "created the %s %s in %s", k.word(), name, config.TildePath(kit.dir))
		return m.editSpec(k, name), nil
	})
	selectJSONStem(p.field)
	p.cancel = func(m *Model) { m.openPipes(pipeSel{}) }
}

// restoreSpec puts a trashed spec back under its old name, or asks for
// another when that name has been taken since.
func (m *Model) restoreSpec(k specKind, t userdata.SpecTrashInfo) {
	kit := m.specKit(k)
	name, err := kit.restore(kit.dir, t.ID, "")
	if err == nil {
		m.logf(logOk, "restored the %s %s", k.word(), name)
		m.openPipes(pipeSel{k, name})
		return
	}
	if !errors.Is(err, kit.exists) {
		m.logf(logErr, "could not restore %s: %s", t.Name, serr.StringFromErr(err))
		return
	}
	p := m.openPromptCmd("Restore "+t.Name+" as…", "a "+k.word()+" named "+t.Name+" exists — restore this one under another name",
		"Restore", freeSpecName(t.Name, kit.list(kit.dir)), func(m *Model, typed string) (tea.Cmd, error) {
			to := jsonName(typed)
			got, err := kit.restore(kit.dir, t.ID, to)
			if err != nil {
				return nil, specNameErr(kit, err, to)
			}
			m.logf(logOk, "restored %s as %s", t.Name, got)
			m.openPipes(pipeSel{k, got})
			return nil, nil
		})
	selectJSONStem(p.field)
	p.cancel = func(m *Model) { m.openPipes(pipeSel{}) }
}

// specEditedMsg is the editor's exit, back in Update.
type specEditedMsg struct {
	spec      specKind
	dir, name string
	before    string // the spec's revision when the editor started
	err       error
}

// editSpec suspends the TUI and opens the spec's JSON in the user's
// editor; specEdited checks it on return. As editScript.
func (m *Model) editSpec(k specKind, name string) tea.Cmd {
	kit := m.specKit(k)
	path := filepath.Join(kit.dir, name)
	_, before, err := kit.read(kit.dir, name)
	if err != nil {
		m.logf(logErr, "could not open %s: %s", name, serr.StringFromErr(err))
		return nil
	}
	line := editorLine()
	bin, err := exec.LookPath(line[0])
	if err != nil {
		m.logf(logErr, "no editor %q on PATH — set $VISUAL or $EDITOR (e.g. EDITOR=nvim, or \"code -w\")", line[0])
		return nil
	}
	m.logf(logMuted, "editing %s in %s — dbc comes back when it exits", config.TildePath(path), filepath.Base(line[0]))
	cmd := exec.Command(bin, append(line[1:], path)...)
	return execEditor(cmd, func(err error) tea.Msg {
		return specEditedMsg{spec: k, dir: kit.dir, name: name, before: before, err: err}
	})
}

// placedDiag is a check finding with where it is in the text (0 when it
// could not be placed).
type placedDiag struct {
	pipeline.Diag
	line, col int
}

// checkSpec checks a spec's text as dbc web's JSON view and `dbc pipeline
// check` / `dbc job check` do: a parse error placed at its offset, else
// the kind's check, each finding placed by its where.
func (m *Model) checkSpec(k specKind, text string) []placedDiag {
	m.syncPlugins() // a node may place a plugin file edited since
	parseErr := func(err error) []placedDiag {
		line, col := pipeline.ParseErrorAt(text, err)
		return []placedDiag{{Diag: pipeline.Diag{Severity: pipeline.SevError, Msg: strings.TrimPrefix(err.Error(), "json: ")}, line: line, col: col}}
	}
	var diags []pipeline.Diag
	locate := pipeline.Locate
	if k == specJob {
		spec, err := jobs.ParseJob(text)
		if err != nil {
			return parseErr(err)
		}
		var conns []string
		for _, c := range m.cfg.Conns() {
			conns = append(conns, c.Name)
		}
		diags = jobs.CheckJob(spec, jobs.CheckOptions{Find: jobs.PipelineFinder(m.cfg.PipelinesDir), Conns: conns})
		locate = jobs.Locate
	} else {
		spec, err := pipeline.Parse(text)
		if err != nil {
			return parseErr(err)
		}
		diags = pipeline.Check(spec, pipeline.CheckOptions{Conns: jobs.CheckConns(m.cfg, spec)})
	}
	out := make([]placedDiag, len(diags))
	for i, d := range diags {
		out[i] = placedDiag{Diag: d}
		out[i].line, out[i].col = locate(text, d.Where)
	}
	return out
}

// specEdited checks the spec the editor left behind, logs the findings as
// "path:line:col: where: msg" (the form an editor jumps to; without the
// place when it could not be found), and reopens the browser on it.
func (m *Model) specEdited(msg specEditedMsg) tea.Cmd {
	if msg.err != nil {
		m.logf(logWarn, "the editor exited with %v", msg.err)
	}
	kit := m.specKit(msg.spec)
	text, rev, err := kit.read(msg.dir, msg.name)
	if err != nil {
		m.logf(logWarn, "%s is gone: %s", msg.name, serr.StringFromErr(err))
		m.openPipes(pipeSel{})
		return nil
	}
	diags := m.checkSpec(msg.spec, text)
	changed := "saved"
	if rev == msg.before {
		changed = "unchanged"
	}
	if len(diags) == 0 {
		m.logf(logOk, "%s %s — checked, no problems (Enter runs it)", msg.name, changed)
		m.openPipes(pipeSel{msg.spec, msg.name})
		return nil
	}
	errs := 0
	for _, d := range diags {
		if d.Severity == pipeline.SevError {
			errs++
		}
	}
	kind := pick(errs > 0, logErr, logWarn)
	m.logf(kind, "%s %s — the check found %d error(s), %d warning(s):", msg.name, changed, errs, len(diags)-errs)
	shown := config.TildePath(filepath.Join(msg.dir, msg.name))
	for i, d := range diags {
		if i == scriptDiagMax {
			m.logf(logMuted, "… and %d more (dbc %s check %s lists them all)", len(diags)-i, msg.spec.word(), msg.name)
			break
		}
		at := shown
		if d.line > 0 {
			at = fmt.Sprintf("%s:%d:%d", shown, d.line, d.col)
		}
		m.logf(pick(d.Severity == pipeline.SevError, logErr, logWarn), "%s: %s", at, d.Diag.String())
	}
	m.openPipes(pipeSel{msg.spec, msg.name})
	return nil
}
