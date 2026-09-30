package web

import (
	"image/png"
	"io"
	"strings"
	"testing"
)

// The ERD dialog's requests: the description and Mermaid source, the three
// files, the theme and download switches, and refusals as words.
func TestERD(t *testing.T) {
	e := newTestEnv(t)
	id, s := e.connected()
	e.api("POST", "/api/v1/ws/"+id+"/run", `{"buffer":"CREATE TABLE owners (id INTEGER PRIMARY KEY); CREATE TABLE pets (id INTEGER PRIMARY KEY, owner_id INTEGER REFERENCES owners(id))","caret":0,"all":true}`, 200)
	s.await(t, "run")

	type info struct {
		Title        string
		Tables, Rels int
		Text         string
	}
	all := decodeData[info](t, e.api("GET", "/api/v1/ws/"+id+"/erd", "", 200))
	if all.Tables != 3 || all.Rels != 1 || !strings.Contains(all.Text, `owners |o..o{ pets : "owner_id"`) ||
		!strings.Contains(all.Title, "demo-sqlite (sqlite)") {
		t.Errorf("whole diagram = %+v", all)
	}
	around := decodeData[info](t, e.api("GET", "/api/v1/ws/"+id+"/erd?table=pets&depth=0", "", 200))
	if around.Tables != 1 {
		t.Errorf("pets alone = %+v", around)
	}

	for ext, want := range map[string]struct{ mime, magic string }{
		"jpg": {"image/jpeg", "\xff\xd8\xff"},
		"png": {"image/png", "\x89PNG"},
		"mmd": {"text/plain; charset=utf-8", "erDiagram"},
	} {
		res := e.req("GET", "/api/v1/ws/"+id+"/erd."+ext+"?table=pets&download=1", "", nil)
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 200 || res.Header.Get("Content-Type") != want.mime || !strings.HasPrefix(string(b), want.magic) {
			t.Errorf("erd.%s = %d %q, starts %.12q", ext, res.StatusCode, res.Header.Get("Content-Type"), b)
		}
		if cd := res.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, `attachment; filename="erd-demo-sqlite-`) ||
			!strings.HasSuffix(cd, "."+ext+`"`) {
			t.Errorf("erd.%s Content-Disposition = %q", ext, cd)
		}
	}

	// the light theme reaches the picture: its corner is the light background
	res := e.req("GET", "/api/v1/ws/"+id+"/erd.png?theme=light", "", nil)
	img, err := png.Decode(res.Body)
	res.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if r, _, _, _ := img.At(1, 1).RGBA(); r>>8 < 0xc0 {
		t.Errorf("theme=light drew a dark corner (r=%d)", r>>8)
	}
	if res.Header.Get("Content-Disposition") != "" {
		t.Error("without download=1 the picture shows in the tab")
	}

	// an unknown table is refused with words, as JSON and as a file
	env := e.api("GET", "/api/v1/ws/"+id+"/erd?table=ghosts", "", 400)
	if !strings.Contains(env.Error, "no table ghosts") {
		t.Errorf("refusal = %+v", env)
	}
	res = e.req("GET", "/api/v1/ws/"+id+"/erd.png?table=ghosts", "", nil)
	res.Body.Close()
	if res.StatusCode != 400 {
		t.Errorf("erd.png for a ghost = %d", res.StatusCode)
	}
}
