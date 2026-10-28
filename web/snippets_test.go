package web

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/userdata"
)

// The list, a new snippet, a refused one, a rename, the expansion in
// Monaco's syntax, and a delete — over the memory store the test server
// gets, seeded with the starters.
func TestSnippetsAPI(t *testing.T) {
	e := newTestEnv(t)
	type listed struct {
		Snippets []snippetRow `json:"snippets"`
	}
	list := func() map[string]snippetRow {
		t.Helper()
		out := map[string]snippetRow{}
		for _, r := range decodeData[listed](t, e.api("GET", "/api/v1/snippets", "", 200)).Snippets {
			out[r.Name] = r
		}
		return out
	}
	rows := list()
	if len(rows) != len(userdata.StarterSnippets) || !rows["cte"].Selection || rows["sel"].Selection {
		t.Fatalf("starters = %+v", rows)
	}

	body := func(name, desc, b string) string {
		j, _ := json.Marshal(snippetSave{Name: name, Desc: desc, Body: b})
		return string(j)
	}
	e.api("POST", "/api/v1/snippets", body("mine", "mine", "SELECT $VAR1:x$ FROM $VAR2$"), 200)
	// refusals: a broken template and a bad name are 400s with the reason,
	// a taken name a 409
	if env := e.api("POST", "/api/v1/snippets", body("bad", "", "$VAR1"), 400); !strings.Contains(env.Error, "not closed") {
		t.Errorf("broken body: %q", env.Error)
	}
	e.api("POST", "/api/v1/snippets", body("a-b", "", "x"), 400)
	e.api("POST", "/api/v1/snippets", body("sel", "", "x"), 409)
	// rename
	e.api("PUT", "/api/v1/snippets/mine", body("ours", "ours", "SELECT $VAR1:x$ FROM $VAR2$ -- $SELECTION$"), 200)
	rows = list()
	if _, still := rows["mine"]; still || !rows["ours"].Selection {
		t.Fatalf("after rename: %+v", rows)
	}

	expand := func(name string, req snippetExpandReq, want int) snippetExpandResp {
		t.Helper()
		j, _ := json.Marshal(req)
		return decodeData[snippetExpandResp](t, e.api("POST", "/api/v1/snippets/"+name+"/expand", string(j), want))
	}
	x := expand("ours", snippetExpandReq{Selection: "$1", Indent: "  "}, 200)
	if x.Snippet != `SELECT ${1:x} FROM ${2} -- \$1$0` || x.Text != "SELECT x FROM  -- $1" || x.Stops != 2 {
		t.Fatalf("expand = %+v", x)
	}
	x = expand("cnt", snippetExpandReq{Indent: "    "}, 200)
	if !strings.Contains(x.Text, "\n    FROM table") {
		t.Fatalf("indent not applied: %q", x.Text)
	}
	e.api("POST", "/api/v1/snippets/nosuch/expand", "{}", 404)

	e.api("DELETE", "/api/v1/snippets/ours", "", 200)
	e.api("DELETE", "/api/v1/snippets/ours", "", 404)
	if _, still := list()["ours"]; still {
		t.Fatal("ours survived its delete")
	}
}
