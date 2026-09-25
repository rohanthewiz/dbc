package explain

import (
	"encoding/json"
	"html"
	"os"
	"strings"
	"testing"
)

// htmlFixtures parses a spread of the captured plans — every parser, with
// and without measurements, one and several top-level trees — for the HTML
// tests to render.
func htmlFixtures(t *testing.T) map[string]*Plan {
	t.Helper()
	read := func(f string) []byte {
		b, err := os.ReadFile("testdata/" + f)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	out := map[string]*Plan{}
	for _, f := range []string{"pg_analyze_join.json", "pg_subplan.json", "pg_estimate_join.json"} {
		p, err := ParsePostgresJSON(read(f))
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		out[f] = p
	}
	p, err := ParseMySQLTree(string(read("my_analyze_subq.txt")))
	if err != nil {
		t.Fatal(err)
	}
	out["my_analyze_subq.txt"] = p

	lines := strings.Split(strings.TrimSpace(string(read("my_tabular_subq.tsv"))), "\n")
	var rows [][]string
	for _, l := range lines[1:] {
		rows = append(rows, strings.Split(l, "\t"))
	}
	p, err = ParseMySQLTable(strings.Split(lines[0], "\t"), rows)
	if err != nil {
		t.Fatal(err)
	}
	out["my_tabular_subq.tsv"] = p
	return out
}

// planData extracts the embedded plan JSON the way a browser would: the
// text between the data block's start tag and the FIRST "</script" after it.
func planData(t *testing.T, page string) string {
	t.Helper()
	const open = `<script type="application/json" id="plan-data">`
	i := strings.Index(page, open)
	if i < 0 {
		t.Fatal("no plan-data block")
	}
	rest := page[i+len(open):]
	j := strings.Index(strings.ToLower(rest), "</script")
	if j < 0 {
		t.Fatal("plan-data block never ends")
	}
	return rest[:j]
}

func TestHTMLRendersEveryFixture(t *testing.T) {
	for name, p := range htmlFixtures(t) {
		p.Statement = "SELECT u.city, count(*) FROM orders o JOIN users u ON u.id = o.user_id WHERE o.status = 'paid'"
		page, err := p.HTML()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(page, "<title>"+html.EscapeString(p.Headline())+"</title>") {
			t.Errorf("%s: title is not the headline %q", name, p.Headline())
		}
		for _, ph := range []string{"{{PLAN_JSON}}", "{{TITLE}}", "/*{{PALETTE}}*/"} {
			if strings.Contains(page, ph) {
				t.Errorf("%s: placeholder %s left in the page", name, ph)
			}
		}
		if !strings.Contains(page, "--accent:#") {
			t.Errorf("%s: palette not injected", name)
		}
		var doc struct {
			Headline string   `json:"headline"`
			Metrics  []string `json:"metrics"`
			Engine   string   `json:"engine"`
			Root     struct {
				ID      int                `json:"id"`
				Weights map[string]float64 `json:"weights"`
			} `json:"root"`
		}
		if err := json.Unmarshal([]byte(planData(t, page)), &doc); err != nil {
			t.Fatalf("%s: embedded JSON does not parse: %v", name, err)
		}
		if doc.Headline != p.Headline() || doc.Engine != p.Engine || len(doc.Metrics) == 0 {
			t.Errorf("%s: embedded doc = %+v", name, doc)
		}
		if len(doc.Root.Weights) == 0 {
			t.Errorf("%s: root has no weights, so the page could not size anything", name)
		}
	}
}

// A statement is the user's text and may contain anything — including the
// one sequence that would end the data block early and let the rest of the
// JSON run as markup.
func TestHTMLStatementCannotBreakOut(t *testing.T) {
	p := htmlFixtures(t)["pg_analyze_join.json"]
	evil := `SELECT '</script><script>alert(1)</script>' AS x -- </SCRIPT >`
	p.Statement = evil
	page, err := p.HTML()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Statement string `json:"statement"`
	}
	if err := json.Unmarshal([]byte(planData(t, page)), &doc); err != nil {
		t.Fatalf("embedded JSON was cut short: %v", err)
	}
	if doc.Statement != evil {
		t.Errorf("statement came back as %q", doc.Statement)
	}
	if strings.Contains(page, "<script>alert(1)") {
		t.Error("the statement's markup reached the page unescaped")
	}
}

// The page is opened from temp files and attachments, often offline: it must
// not reference anything on the network.
func TestHTMLIsSelfContained(t *testing.T) {
	for name, p := range htmlFixtures(t) {
		page, err := p.HTML()
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{"http://", "https://", "//cdn", "@import", "<link "} {
			if strings.Contains(page, bad) {
				t.Errorf("%s: page references %q", name, bad)
			}
		}
	}
}
