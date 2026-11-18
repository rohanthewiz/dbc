package e2e

import (
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
)

// a11yBasics: the keyboard and screen-reader basics of N-300, each the
// thing a mouse user never notices is missing.
//
//   - A dialog keeps Tab inside it, both ways round.
//   - The result grid is an ARIA grid: counts on the root, indices on the
//     rows and cells drawn, and aria-activedescendant following the cursor.
//   - The status line is spoken through #announce, a live region.
//   - The light theme's text colours reach 4.5:1 on the bars they sit on.
//   - The topbar's buttons stay on screen at widths below ~1100 px.
//   - A failed page of rows is retried with a backoff and logged once,
//     then fills in by itself.
//
// Stands on boot alone (DBC_E2E_STEPS=a11y): the query is a recursive
// CTE every engine the suite runs on reads the same.
func a11yBasics(t *testing.T, _ *env, p *rod.Page) {
	// ── a dialog keeps Tab ────────────────────────────────────────────────
	// the Add a connection form: a dozen fields, so Tab and Shift+Tab both
	// wrap at least once in stops+2 presses
	p.MustElement("#conn-add").MustClick()
	waitFor(t, p, "the + menu", `() => !!document.querySelector(".menu")`)
	menuPick(t, p, "Add a connection…")
	waitFor(t, p, "the form", `() => !!document.getElementById("cf-name") && !!document.getElementById("cf-name").closest(".modal")`)
	stops := int(eval(t, p, `() => [...document.querySelector(".modal").querySelectorAll(
	  'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])')].filter((x) => !x.disabled && x.getClientRects().length).length`).(float64))
	if stops < 2 {
		t.Fatalf("the connection form has %d tab stops; the trap needs two to show a wrap", stops)
	}
	inModal := `() => !!document.activeElement && !!document.activeElement.closest(".modal")`
	for i := 0; i < stops+2; i++ {
		p.Keyboard.MustType(input.Tab)
		if !eval(t, p, inModal).(bool) {
			t.Fatalf("Tab %d of %d left the dialog: focus on %s", i+1, stops+2,
				evalStr(t, p, `() => document.activeElement ? document.activeElement.outerHTML.slice(0, 120) : "nothing"`))
		}
	}
	for i := 0; i < stops+2; i++ {
		if err := p.Keyboard.Press(input.ShiftLeft); err != nil {
			t.Fatal(err)
		}
		p.Keyboard.MustType(input.Tab)
		if err := p.Keyboard.Release(input.ShiftLeft); err != nil {
			t.Fatal(err)
		}
		if !eval(t, p, inModal).(bool) {
			t.Fatalf("Shift+Tab %d of %d left the dialog", i+1, stops+2)
		}
	}
	p.Keyboard.MustType(input.Escape)
	waitFor(t, p, "the dialog closed", `() => !document.querySelector(".modal")`)

	// ── the grid's roles, and the status spoken ───────────────────────────
	before := gridSeq(t, p)
	eval(t, p, `() => dbc.editor.setText("WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 1000) SELECT i, i * 2 AS twice FROM n")`)
	p.MustElement("#run").MustClick()
	waitResult(t, p, before, "i", "twice")
	waitFor(t, p, "the run's status spoken", `() => {
	  const a = document.getElementById("announce"), s = document.getElementById("status");
	  return !!a && a.getAttribute("role") === "status" && a.textContent.replace(/ $/, "") === s.textContent && !/…$/.test(s.textContent);
	}`)
	p.MustElement("#grid").MustFocus()
	aria := evalStr(t, p, `() => {
	  const g = document.getElementById("grid"), cur = document.getElementById(g.getAttribute("aria-activedescendant") || "-");
	  const row = document.querySelector('#grid .gb [role="row"][aria-rowindex="2"]');
	  return [g.getAttribute("role"), g.getAttribute("aria-rowcount"), g.getAttribute("aria-colcount"),
	    row ? row.querySelectorAll('[role="gridcell"]').length : -1,
	    cur ? cur.getAttribute("role") + "@" + cur.closest('[role="row"]').getAttribute("aria-rowindex") + "," + cur.getAttribute("aria-colindex") + "=" + cur.textContent : "none",
	    document.querySelectorAll('#grid .gh [role="columnheader"]').length].join(" ");
	}`)
	if aria != "grid 1001 3 2 gridcell@2,2=1 3" {
		t.Fatalf("the grid's ARIA = %q, want %q (role, rows+1, cols+1, cells in row 1, the cursor's cell, headers)", aria, "grid 1001 3 2 gridcell@2,2=1 3")
	}
	p.Keyboard.MustType(input.ArrowDown, input.ArrowRight)
	waitFor(t, p, "aria-activedescendant on row 2's twice", `() => {
	  const c = document.getElementById(document.getElementById("grid").getAttribute("aria-activedescendant") || "-");
	  return !!c && c.getAttribute("aria-colindex") === "3" && c.closest('[role="row"]').getAttribute("aria-rowindex") === "3" && c.textContent === "4";
	}`)

	// ── a failed page: backed off, logged once, filled in by itself ───────
	// Rows 200–399 are page 1. Its fetches fail while the view sits on row
	// 300 and is nudged every 100 ms, as a scroll would; each nudge draws,
	// and every draw used to ask (and log) again.
	eval(t, p, `() => {
	  window.__a11yFetch = window.fetch;
	  window.__a11yFails = 0;
	  window.fetch = (u, o) => {
	    if (String(u).includes("/result?from=200&")) { window.__a11yFails++; return Promise.reject(new TypeError("Failed to fetch")); }
	    return window.__a11yFetch(u, o);
	  };
	  const g = document.getElementById("grid");
	  g.scrollTop = 300 * 22;
	  let k = 0;
	  window.__a11yNudge = setInterval(() => { g.scrollTop += (k++ % 2 ? -1 : 1); }, 100);
	}`)
	logged := `() => [...document.querySelectorAll("#log > div")].filter((d) => d.textContent.includes("could not load rows")).length`
	waitFor(t, p, "the failed page logged", logged+` === 1`)
	time.Sleep(2500 * time.Millisecond)
	fails := int(eval(t, p, `() => window.__a11yFails`).(float64))
	lines := int(eval(t, p, logged).(float64))
	eval(t, p, `() => { clearInterval(window.__a11yNudge); window.fetch = window.__a11yFetch; }`)
	t.Logf("page 1 failing for ~3 s: %d fetches, %d log lines", fails, lines)
	// 1 s, then 2 s: three tries at most in the ~3 s since the first
	if fails < 1 || fails > 3 || lines != 1 {
		t.Fatalf("with the page failing for ~3 s under a nudged view: %d fetches and %d log lines, want 1–3 and 1", fails, lines)
	}
	waitFor(t, p, "row 301 filled in without a scroll, and the log saying so", `() => {
	  const r = document.querySelector('#grid .gb [role="row"][aria-rowindex="302"]');
	  return !!r && !r.querySelector(".gc.wait") && r.querySelector('[role="gridcell"]').textContent === "301" &&
	    [...document.querySelectorAll("#log > div")].some((d) => d.textContent.includes("rows load again"));
	}`)

	// ── the light theme's text colours ────────────────────────────────────
	p.MustElement("#theme-btn").MustClick()
	waitFor(t, p, "the light theme", `() => document.documentElement.dataset.theme === "light"`)
	vars := strings.Fields(evalStr(t, p, `() => { const s = getComputedStyle(document.documentElement);
	  return ["--fg", "--muted", "--accent", "--warn", "--err", "--bg", "--panel2"].map((v) => s.getPropertyValue(v).trim()).join(" "); }`))
	p.MustElement("#theme-btn").MustClick()
	waitFor(t, p, "the theme back", `() => document.documentElement.dataset.theme !== "light"`)
	if len(vars) != 7 {
		t.Fatalf("the light theme's variables: %q", vars)
	}
	for i, name := range []string{"fg", "muted", "accent", "warn", "err"} {
		for j, on := range []string{"bg", "panel2"} {
			if r := contrast(t, vars[i], vars[5+j]); r < 4.5 {
				t.Errorf("light --%s (%s) on --%s (%s) is %.2f:1, under the 4.5:1 small text needs", name, vars[i], on, vars[5+j], r)
			}
		}
	}

	// ── the topbar at narrower widths ─────────────────────────────────────
	defer p.MustSetViewport(1400, 900, 1, false)
	for _, w := range []int{1100, 1000, 900, 760} {
		p.MustSetViewport(w, 900, 1, false)
		waitFor(t, p, "the page laid out at "+strconv.Itoa(w)+"px", `(w) => window.innerWidth === w`, w)
		if off := evalStr(t, p, `() => [...document.querySelectorAll(".topbar button")]
		  .filter((b) => b.getClientRects().length && b.getBoundingClientRect().right > window.innerWidth + 0.5)
		  .map((b) => b.textContent.trim()).join(", ")`); off != "" {
			t.Errorf("at %dpx the topbar's %s run off the right edge", w, off)
		}
	}
}

// contrast is the WCAG contrast ratio of two #rrggbb colours.
func contrast(t *testing.T, a, b string) float64 {
	t.Helper()
	lum := func(h string) float64 {
		if len(h) != 7 || h[0] != '#' {
			t.Fatalf("not a #rrggbb colour: %q", h)
		}
		var l float64
		for i, w := range []float64{0.2126, 0.7152, 0.0722} {
			n, err := strconv.ParseUint(h[1+2*i:3+2*i], 16, 8)
			if err != nil {
				t.Fatal(err)
			}
			v := float64(n) / 255
			if v <= 0.03928 {
				v /= 12.92
			} else {
				v = math.Pow((v+0.055)/1.055, 2.4)
			}
			l += w * v
		}
		return l
	}
	x, y := lum(a), lum(b)
	return (math.Max(x, y) + 0.05) / (math.Min(x, y) + 0.05)
}
