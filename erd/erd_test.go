package erd

import (
	"bytes"
	"fmt"
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/raster"
	"github.com/rohanthewiz/dbc/theme"
)

// fixture builds a schema with every shape the diagram has to handle:
//
//	countries ◄── owners ◄── cats ◄══ visits      a chain; visits' key is
//	                 ▲         ▲                    identifying (in its PK)
//	      profiles ──┘ (1:1)   └── cats.mother_id   a self-reference
//	emps.boss_id ──► emps                          a lone self-reference
//	wide (50 columns), notes                       no relationships
func fixture() *Schema {
	s := &Schema{Conn: "demo", Driver: "bytdb"}
	tbl := func(name string, pk []string, cols ...string) *Table {
		t := &Table{Schema: "public", Name: name, Label: name, PK: pk}
		for _, c := range cols {
			// "name type" or "name type null"
			f := strings.Fields(c)
			t.Cols = append(t.Cols, &Column{Name: f[0], Type: f[1], Nullable: len(f) > 2})
		}
		s.Tables = append(s.Tables, t)
		return t
	}
	countries := tbl("countries", []string{"code"}, "code char(2)", "name text")
	owners := tbl("owners", []string{"id"}, "id bigint", "email text null", "country char(2)")
	owners.Uniques = [][]string{{"email"}}
	cats := tbl("cats", []string{"id"}, "id bigint", "owner_id bigint null", "mother_id bigint null", "name text")
	visits := tbl("visits", []string{"cat_id", "seq"}, "cat_id bigint", "seq int", "note text null")
	profiles := tbl("profiles", []string{"id"}, "id bigint", "owner_id bigint", "bio text null")
	profiles.Uniques = [][]string{{"owner_id"}}
	emps := tbl("emps", []string{"id"}, "id bigint", "boss_id bigint null")
	var wideCols []string
	for i := range 50 {
		wideCols = append(wideCols, fmt.Sprintf("c%02d numeric(10,2) null", i))
	}
	tbl("wide", nil, wideCols...)
	tbl("notes", nil, "body text null")

	rel := func(name string, child, parent *Table, cc, pc string) {
		s.Rels = append(s.Rels, &Rel{Name: name, Child: child, Parent: parent,
			ChildCols: []string{cc}, ParentCols: []string{pc}})
	}
	rel("owners_country_fkey", owners, countries, "country", "code")
	rel("cats_owner_id_fkey", cats, owners, "owner_id", "id")
	rel("cats_mother_id_fkey", cats, cats, "mother_id", "id")
	rel("visits_cat_id_fkey", visits, cats, "cat_id", "id")
	rel("profiles_owner_id_fkey", profiles, owners, "owner_id", "id")
	rel("emps_boss_id_fkey", emps, emps, "boss_id", "id")
	s.MarkKeys()
	s.Sort()
	return s
}

func rel(s *Schema, name string) *Rel {
	for _, r := range s.Rels {
		if r.Name == name {
			return r
		}
	}
	return nil
}

func TestRelNotation(t *testing.T) {
	s := fixture()
	cases := []struct {
		name                         string
		optional, oneToOne, identify bool
	}{
		{"cats_owner_id_fkey", true, false, false},     // nullable key: zero or one owner
		{"owners_country_fkey", false, false, false},   // NOT NULL: exactly one country
		{"visits_cat_id_fkey", false, false, true},     // in visits' PK: identifying
		{"profiles_owner_id_fkey", false, true, false}, // unique key: 1:1
		{"emps_boss_id_fkey", true, false, false},
	}
	for _, c := range cases {
		r := rel(s, c.name)
		if r.Optional() != c.optional || r.OneToOne() != c.oneToOne || r.Identifying() != c.identify {
			t.Errorf("%s: optional=%v oneToOne=%v identifying=%v, want %v %v %v", c.name,
				r.Optional(), r.OneToOne(), r.Identifying(), c.optional, c.oneToOne, c.identify)
		}
	}
	// a composite key's single column is not the whole key: not 1:1
	if rel(s, "visits_cat_id_fkey").OneToOne() {
		t.Error("visits.cat_id is only part of visits' PK, so visits is many per cat")
	}
}

func TestMarkKeys(t *testing.T) {
	s := fixture()
	v, _ := s.Find("visits")
	if c := v.Col("cat_id"); !c.PK || !c.FK || badgeTag(c) != "PF" {
		t.Errorf("visits.cat_id = %+v, badge %q", c, badgeTag(c))
	}
	o, _ := s.Find("owners")
	if c := o.Col("email"); !c.Unique || c.PK || badgeTag(c) != "UK" {
		t.Errorf("owners.email = %+v", c)
	}
	if c := o.Col("id"); c.Nullable {
		t.Error("a primary key column is never nullable")
	}
}

func TestFind(t *testing.T) {
	s := &Schema{Tables: []*Table{
		{Schema: "a", Name: "cats", Label: "a.cats"},
		{Schema: "b", Name: "cats", Label: "b.cats"},
		{Schema: "b", Name: "Dogs", Label: "b.Dogs"},
		{Schema: "b", Name: "dogs", Label: "b.dogs"},
		{Schema: "b", Name: "Owls", Label: "b.Owls"},
	}}
	for _, c := range []struct {
		name, want string
	}{
		{"a.cats", "a.cats"},
		{"cats", ""}, // in two schemas: not guessed
		{"b.Dogs", "b.Dogs"},
		{"dogs", "b.dogs"}, // exact case wins over the fold
		{"DOGS", ""},       // two folds, no exact
		{"owls", "b.Owls"}, // the only fold
		{"nope", ""},
	} {
		got, ok := s.Find(c.name)
		if (c.want == "") == ok || (ok && got.Label != c.want) {
			t.Errorf("Find(%q) = %v %v, want %q", c.name, got, ok, c.want)
		}
	}
}

func labels(s *Schema) string {
	var out []string
	for _, t := range s.Tables {
		out = append(out, t.Label)
	}
	return strings.Join(out, " ")
}

func TestAround(t *testing.T) {
	s := fixture()
	cases := []struct {
		names []string
		depth int
		want  string
		rels  int
	}{
		{[]string{"cats"}, 0, "cats", 1}, // its self-reference stays
		{[]string{"cats"}, 1, "cats owners visits", 3},
		{[]string{"cats"}, 2, "cats countries owners profiles visits", 5},
		{[]string{"visits"}, -1, "cats countries owners profiles visits", 5},
		{[]string{"visits", "emps"}, 0, "emps visits", 1},
		{[]string{"notes"}, 3, "notes", 0},
	}
	for _, c := range cases {
		got, missing := s.Around(c.names, c.depth)
		if labels(got) != c.want || len(got.Rels) != c.rels || len(missing) > 0 {
			t.Errorf("Around(%v, %d) = %q with %d rels (missing %v), want %q with %d",
				c.names, c.depth, labels(got), len(got.Rels), missing, c.want, c.rels)
		}
	}
	if _, missing := s.Around([]string{"cats", "ghosts"}, 1); len(missing) != 1 || missing[0] != "ghosts" {
		t.Errorf("missing = %v", missing)
	}
}

func TestWithoutViews(t *testing.T) {
	s := fixture()
	countries, _ := s.Find("countries")
	countries.View = true
	got := s.WithoutViews()
	if strings.Contains(labels(got), "countries") || rel(got, "owners_country_fkey") != nil {
		t.Errorf("the view or its line survived: %q", labels(got))
	}
}

func TestMermaid(t *testing.T) {
	out := fixture().Mermaid()
	for _, want := range []string{
		"erDiagram\n",
		"%% dbc · demo (bytdb) · 8 tables · 6 relationships",
		`owners |o..o{ cats : "owner_id"`,     // nullable, many, non-identifying
		`countries ||..o{ owners : "country"`, // NOT NULL
		`cats ||--o{ visits : "cat_id"`,       // identifying: solid
		`owners ||..o| profiles : "owner_id"`, // 1:1
		`emps |o..o{ emps : "boss_id"`,        // self-reference
		"        bigint cat_id PK, FK\n",
		"        text email UK\n",
		`        numeric(10_2) c00 "numeric(10,2)"`, // the comma is not a word character
		"    notes {\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("mermaid lacks %q:\n%s", want, out)
		}
	}
	// every line is a statement Mermaid's grammar can take: a comment, a
	// relationship, an entity opening, an attribute or a closing brace
	stmt := regexp.MustCompile(`^(erDiagram|    %%.*|    [\w-]+(\["[^"]*"\])? [|}][|o](--|\.\.)[o|][|{] [\w-]+ : "[^"]*"|    [\w-]+(\["[^"]*"\])?( \{)?|        [\w()\[\]-]+ [\w()\[\]-]+( (PK|FK|UK)(, (PK|FK|UK))*)?( "[^"]*")?|    \})$`)
	for line := range strings.SplitSeq(strings.TrimSuffix(out, "\n"), "\n") {
		if !stmt.MatchString(line) {
			t.Errorf("line Mermaid would not parse: %q", line)
		}
	}
}

func TestMermaidNames(t *testing.T) {
	s := &Schema{Tables: []*Table{
		{Schema: "public", Name: "cats", Label: "public.cats", Cols: []*Column{
			{Name: "first name", Type: "character varying(80)"},
			{Name: "_id", Type: "int"},
			{Name: `we"ird`, Type: ""},
		}},
		{Schema: "x", Name: "t1", Label: "t1"}, // a real table already called t1
		{Schema: "x", Name: "my table", Label: "my table"},
	}}
	s.Rels = []*Rel{{Name: "k", Child: s.Tables[2], Parent: s.Tables[0], ChildCols: []string{"a"}, ParentCols: []string{"_id"}}}
	out := s.Mermaid()
	for _, want := range []string{
		`t2["public.cats"] {`, // aliased; t1 is taken
		"    t1\n",            // the real t1 keeps its name
		`t3["my table"]`,
		`t2 |o..o{ t3 : "a"`, // the relationship uses the ids
		`character_varying(80) first_name "first name: character varying(80)"`,
		"        int _id\n",       // a name's own _ survives
		`unknown we_ird "we'ird"`, // no type, and the quote
	} {
		if !strings.Contains(out, want) {
			t.Errorf("mermaid lacks %q:\n%s", want, out)
		}
	}
}

// TestPicture renders the fixture in both palettes and checks the picture
// decodes, is the palette's background at the corner, and holds more than
// the background (something was drawn). Set ERD_PICTURE_DIR to keep the
// files and look at them.
func TestPicture(t *testing.T) {
	s := fixture()
	for _, pal := range []theme.Palette{theme.Default(), theme.Light()} {
		b, err := s.PNG(Options{Palette: pal})
		if err != nil {
			t.Fatal(err)
		}
		img, err := png.Decode(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		r, g, bl, _ := theme.ParseHex(pal.Bg)
		cr, cg, cb, _ := img.At(1, 1).RGBA()
		if uint8(cr>>8) != r || uint8(cg>>8) != g || uint8(cb>>8) != bl {
			t.Errorf("corner = %d,%d,%d, want the palette's bg %s", cr>>8, cg>>8, cb>>8, pal.Bg)
		}
		if w := img.Bounds().Dx(); w < 2*int(minPicture) {
			t.Errorf("width %d at 2× is under the minimum", w)
		}
		if dir := os.Getenv("ERD_PICTURE_DIR"); dir != "" {
			_ = os.WriteFile(filepath.Join(dir, "erd-"+pal.Bg[1:]+".png"), b, 0o644)
		}
	}
	j, err := s.JPEG(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = jpeg.Decode(bytes.NewReader(j)); err != nil {
		t.Fatal(err)
	}
}

// Every box sits inside the picture and no two boxes overlap, whatever the
// schema's shape — the layout's one hard promise.
func TestLayoutNoOverlap(t *testing.T) {
	for _, s := range []*Schema{fixture(), star(45), chain(12)} {
		fs, err := raster.Fonts()
		if err != nil {
			t.Fatal(err)
		}
		m := raster.NewFaces(fs, 1)
		l := newLayout(s, m)
		m.Close()
		for i, a := range l.boxes {
			if a.x < margin-0.5 || a.y < margin || a.x+a.w > l.w-margin+0.5 || a.y+a.h > l.h-margin+0.5 {
				t.Errorf("%s: %s at %.0f,%.0f %.0fx%.0f is outside %.0fx%.0f", s.Conn, a.t.Label, a.x, a.y, a.w, a.h, l.w, l.h)
			}
			for _, b := range l.boxes[i+1:] {
				if a.x < b.x+b.w && b.x < a.x+a.w && a.y < b.y+b.h && b.y < a.y+a.h {
					t.Errorf("%s: %s and %s overlap", s.Conn, a.t.Label, b.t.Label)
				}
			}
		}
	}
}

// In a chain each child sits in a column right of its parent.
func TestLayoutParentsLeft(t *testing.T) {
	s := chain(5)
	fs, _ := raster.Fonts()
	m := raster.NewFaces(fs, 1)
	defer m.Close()
	l := newLayout(s, m)
	for _, r := range s.Rels {
		if c, p := l.byT[r.Child], l.byT[r.Parent]; c.x <= p.x+p.w {
			t.Errorf("%s (x %.0f) is not right of its parent %s (x %.0f)", c.t.Label, c.x, p.t.Label, p.x)
		}
	}
}

// A hub with many children wraps its children's rank into several columns
// rather than one very tall strip.
func TestLayoutWrapsTallRanks(t *testing.T) {
	s := star(45)
	fs, _ := raster.Fonts()
	m := raster.NewFaces(fs, 1)
	defer m.Close()
	l := newLayout(s, m)
	if l.h > 2.5*l.w {
		t.Errorf("a 45-child star is %.0f wide and %.0f tall: not wrapped", l.w, l.h)
	}
}

// A schema too big for maxPixels at 2× is drawn smaller, not refused.
func TestPictureScalesDown(t *testing.T) {
	s := star(400)
	img, err := s.Picture(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if n := img.Bounds().Dx() * img.Bounds().Dy(); n > maxPixels {
		t.Errorf("%d pixels is over maxPixels", n)
	}
}

// star is a hub table referenced by n children.
func star(n int) *Schema {
	s := &Schema{Conn: fmt.Sprintf("star%d", n)}
	hub := &Table{Name: "hub", Label: "hub", PK: []string{"id"}, Cols: []*Column{{Name: "id", Type: "int"}}}
	s.Tables = append(s.Tables, hub)
	for i := range n {
		c := &Table{Name: fmt.Sprintf("child%03d", i), PK: []string{"id"},
			Cols: []*Column{{Name: "id", Type: "int"}, {Name: "hub_id", Type: "int"}, {Name: "payload", Type: "text"}}}
		c.Label = c.Name
		s.Tables = append(s.Tables, c)
		s.Rels = append(s.Rels, &Rel{Name: c.Name + "_fk", Child: c, Parent: hub, ChildCols: []string{"hub_id"}, ParentCols: []string{"id"}})
	}
	s.MarkKeys()
	s.Sort()
	return s
}

// chain is t0 ◄── t1 ◄── … ◄── t(n-1).
func chain(n int) *Schema {
	s := &Schema{Conn: fmt.Sprintf("chain%d", n)}
	var prev *Table
	for i := range n {
		t := &Table{Name: fmt.Sprintf("t%02d", i), PK: []string{"id"},
			Cols: []*Column{{Name: "id", Type: "int"}, {Name: "up_id", Type: "int"}}}
		t.Label = t.Name
		s.Tables = append(s.Tables, t)
		if prev != nil {
			s.Rels = append(s.Rels, &Rel{Name: t.Name + "_fk", Child: t, Parent: prev, ChildCols: []string{"up_id"}, ParentCols: []string{"id"}})
		}
		prev = t
	}
	s.MarkKeys()
	s.Sort()
	return s
}

func TestFileName(t *testing.T) {
	s := &Schema{Conn: "prod db/1"}
	if got := s.FileName("png"); !regexp.MustCompile(`^erd-prod_db_1-\d{8}-\d{6}\.png$`).MatchString(got) {
		t.Errorf("FileName = %q", got)
	}
	if got := (&Schema{}).FileName("mmd"); !strings.HasPrefix(got, "erd-") || strings.HasPrefix(got, "erd--") {
		t.Errorf("FileName = %q", got)
	}
}

func TestSelect(t *testing.T) {
	s := fixture()
	owners, _ := s.Find("owners")
	owners.View = true // pretend: a view in the middle of the chain
	all, _ := s.Select(Selection{})
	if strings.Contains(labels(all), "owners") {
		t.Errorf("views are off by default: %q", labels(all))
	}
	withViews, _ := s.Select(Selection{Views: true})
	if len(withViews.Tables) != len(s.Tables) {
		t.Errorf("Views kept %d of %d", len(withViews.Tables), len(s.Tables))
	}
	// a view asked for by name is shown even with views off; its
	// neighbours come too
	got, missing := s.Select(Selection{Tables: []string{"owners", "nope"}, Depth: 1})
	if labels(got) != "cats countries owners profiles" || len(missing) != 1 {
		t.Errorf("Select around a view = %q, missing %v", labels(got), missing)
	}
}

// farRanks is a chain t00 ◄── … ◄── t05 whose every link also has a second
// key straight back to t00, so lines to t02…t05 skip columns; t00 has four
// more children stacked beside t01, so those lines also have boxes of
// their own rank to thread between (rank 1 wraps into two columns).
func farRanks() *Schema {
	s := chain(6)
	s.Conn = "farranks"
	root := s.Tables[0]
	for _, t := range s.Tables {
		t.Cols = append(t.Cols, &Column{Name: "root_id", Type: "int"}, &Column{Name: "a", Type: "text"}, &Column{Name: "b", Type: "text"})
	}
	for _, t := range s.Tables[1:] {
		s.Rels = append(s.Rels, &Rel{Name: t.Name + "_root", Child: t, Parent: root, ChildCols: []string{"root_id"}, ParentCols: []string{"id"}})
	}
	for i := range 4 {
		c := &Table{Name: fmt.Sprintf("side%d", i), PK: []string{"id"},
			Cols: []*Column{{Name: "id", Type: "int"}, {Name: "t_id", Type: "int"}, {Name: "x", Type: "text"}, {Name: "y", Type: "text"}}}
		c.Label = c.Name
		s.Tables = append(s.Tables, c)
		s.Rels = append(s.Rels, &Rel{Name: c.Name + "_fk", Child: c, Parent: root, ChildCols: []string{"t_id"}, ParentCols: []string{"id"}})
	}
	s.MarkKeys()
	s.Sort()
	return s
}

func testLayout(t *testing.T, s *Schema) *layout {
	t.Helper()
	fs, err := raster.Fonts()
	if err != nil {
		t.Fatal(err)
	}
	m := raster.NewFaces(fs, 1)
	defer m.Close()
	return newLayout(s, m)
}

// No line passes through a box — not its own boxes, which it only touches
// at their edges, and not the boxes between them on the way to a far rank
// or a wrapped column — and every line stays inside the picture, below
// the title band.
func TestRoutesMissBoxes(t *testing.T) {
	for _, s := range []*Schema{fixture(), star(45), chain(12), farRanks(), star(120), star(400), fanOut(60, 6)} {
		l := testLayout(t, s)
		for _, r := range s.Rels {
			p := l.paths[r]
			if p == nil {
				t.Fatalf("%s: %s has no path", s.Conn, r.Name)
			}
			hit := map[*box]bool{}
			for i := 1; i < len(p.pts); i++ {
				a, b := p.pts[i-1], p.pts[i]
				// sample every CSS pixel of the segment: points alone
				// could step over a box's corner
				n := int(math.Ceil(math.Hypot(b.X-a.X, b.Y-a.Y))) + 1
				for k := 0; k <= n; k++ {
					q := pt2{X: a.X + (b.X-a.X)*float64(k)/float64(n), Y: a.Y + (b.Y-a.Y)*float64(k)/float64(n)}
					if q.X < 0 || q.X > l.w || q.Y < margin+titleH-1 || q.Y > l.h {
						t.Errorf("%s: %s leaves the picture at %.0f,%.0f", s.Conn, r.Name, q.X, q.Y)
						break
					}
					for _, bx := range l.boxes {
						// 1 px in from the border: a line's ends sit on it
						if !hit[bx] && q.X > bx.x+1 && q.X < bx.x+bx.w-1 && q.Y > bx.y+1 && q.Y < bx.y+bx.h-1 {
							hit[bx] = true
							t.Errorf("%s: %s passes through %s at %.0f,%.0f", s.Conn, r.Name, bx.t.Label, q.X, q.Y)
						}
					}
				}
			}
		}
	}
}

// The lines through one gap between two boxes keep clear of both boxes,
// however many share it.
func TestLanesFitTheirHole(t *testing.T) {
	h := &hole{top: 100, bot: 100 + stackGap}
	for i := range 9 {
		h.users = append(h.users, &crossing{idx: i, yl: float64(i * 10), yr: float64(i * 10)})
	}
	setLanes(h)
	prev := math.Inf(-1)
	for _, x := range h.users {
		y := h.lane[x]
		if y < h.top+holePad-1e-9 || y > h.bot-holePad+1e-9 {
			t.Errorf("lane %.1f is outside %.0f…%.0f less the padding", y, h.top, h.bot)
		}
		if y <= prev {
			t.Errorf("lanes out of order: %.1f after %.1f", y, prev)
		}
		prev = y
	}
}

// A line that goes over the top of a column is not drawn into the title:
// the group moves down to hold it. chain(3) plus a key from t02's id to
// t00's id puts one box in every column. All three are tall and t01 the
// tallest (so it sets the group's top), which puts the id rows the line
// joins near t01's top: its cheapest way past t01 is the open space just
// above it.
func TestRouteAboveMovesGroupDown(t *testing.T) {
	s := chain(3)
	for i, tb := range s.Tables {
		n := 8
		if i == 1 {
			n = 10
		}
		for j := range n {
			tb.Cols = append(tb.Cols, &Column{Name: fmt.Sprintf("c%d", j), Type: "text"})
		}
	}
	s.Rels = append(s.Rels, &Rel{Name: "t02_skip", Child: s.Tables[2], Parent: s.Tables[0], ChildCols: []string{"id"}, ParentCols: []string{"id"}})
	s.MarkKeys()
	s.Sort()
	l := testLayout(t, s)
	p := l.paths[rel(s, "t02_skip")]
	top := math.Inf(1)
	for _, q := range p.pts {
		top = math.Min(top, q.Y)
	}
	mid := l.byT[s.Tables[1]]
	if top >= mid.y {
		t.Fatalf("the skip line's top %.0f is not above t01 (y %.0f); the test needs it to be", top, mid.y)
	}
	if top < margin+titleH {
		t.Errorf("the skip line rises to %.0f, into the title band (below %.0f)", top, margin+titleH)
	}
}

// audited is users referenced three times by docs and once by notes:
// created_by and updated_by are NOT NULL (exactly one user), approved_by and
// notes.author_id nullable (zero or one). All four keys end at users.id.
func audited() *Schema {
	s := &Schema{Conn: "audited"}
	users := &Table{Name: "users", Label: "users", PK: []string{"id"},
		Cols: []*Column{{Name: "id", Type: "int"}, {Name: "name", Type: "text"}, {Name: "email", Type: "text"}}}
	docs := &Table{Name: "docs", Label: "docs", PK: []string{"id"}, Cols: []*Column{{Name: "id", Type: "int"},
		{Name: "created_by", Type: "int"}, {Name: "updated_by", Type: "int"}, {Name: "approved_by", Type: "int", Nullable: true}}}
	notes := &Table{Name: "notes", Label: "notes", PK: []string{"id"},
		Cols: []*Column{{Name: "id", Type: "int"}, {Name: "author_id", Type: "int", Nullable: true}}}
	s.Tables = []*Table{users, docs, notes}
	for _, k := range []struct {
		name  string
		child *Table
		col   string
	}{{"docs_created", docs, "created_by"}, {"docs_updated", docs, "updated_by"}, {"docs_approved", docs, "approved_by"}, {"notes_author", notes, "author_id"}} {
		s.Rels = append(s.Rels, &Rel{Name: k.name, Child: k.child, Parent: users, ChildCols: []string{k.col}, ParentCols: []string{"id"}})
	}
	s.MarkKeys()
	s.Sort()
	return s
}

// Keys to one parent column with different parent-end markers attach at
// different points of the row, one per marker; keys with the same marker
// share one point (and so one marker). Every point stays within the row.
func TestPortsFanOutByMarker(t *testing.T) {
	s := audited()
	l := testLayout(t, s)
	users := l.byT[rel(s, "docs_created").Parent]
	row := users.rowY("id")
	at := map[string]pt2{}
	for _, r := range s.Rels {
		p := l.paths[r]
		at[r.Name] = p.parent
		if math.Abs(p.parent.Y-row) > rowH/2 {
			t.Errorf("%s attaches at %.1f, outside users.id's row (%.1f ± %.1f)", r.Name, p.parent.Y, row, rowH/2)
		}
		want := mOne
		if r.Optional() {
			want = mZeroOne
		}
		if p.pkind != want {
			t.Errorf("%s: parent marker %d, want %d", r.Name, p.pkind, want)
		}
	}
	if at["docs_created"] != at["docs_updated"] || at["docs_approved"] != at["notes_author"] {
		t.Errorf("same-marker keys should share a port: %v", at)
	}
	if d := math.Abs(at["docs_created"].Y - at["docs_approved"].Y); d < 2*barHalf+2 {
		t.Errorf("|| and |o are %.1f px apart: their bars touch", d)
	}
	// a port with one marker keeps its end at the row's centre
	f := fixture()
	fl := testLayout(t, f)
	r := rel(f, "owners_country_fkey")
	if got := fl.paths[r].parent.Y; got != fl.byT[r.Parent].rowY("code") {
		t.Errorf("a port with one marker moved: %.1f", got)
	}
	// cats.id's right side has the mother_id loop (|o) and the key from
	// visits (||). The loop hugs the box, so it takes the slot toward its
	// other end (below), and the line to visits the one above it: the
	// other way round, the line to visits would cross the loop.
	loop, vis := fl.paths[rel(f, "cats_mother_id_fkey")], fl.paths[rel(f, "visits_cat_id_fkey")]
	if loop.pdir != vis.pdir || loop.parent.Y <= vis.parent.Y {
		t.Errorf("cats.id: loop end at %.1f, visits end at %.1f; the loop should be below", loop.parent.Y, vis.parent.Y)
	}
	if dir := os.Getenv("ERD_PICTURE_DIR"); dir != "" {
		b, err := s.PNG(Options{})
		if err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(dir, "erd-audited.png"), b, 0o644)
	}
}

// A column crossed by more lines than its gaps hold has those gaps widened
// (widen), so the lines still pass between its boxes rather than over or
// under the whole group. Before widening, 7 of star(120)'s lines and 98 of
// star(400)'s went round. A star's lines now ride buses and fit its gaps
// unwidened (TestBusesKeepStarsCompact); fanOut(60, 6)'s, each from a
// slot of its own, still need the room: 19 of them go round without it.
func TestCrowdedGapsWiden(t *testing.T) {
	for _, s := range []*Schema{star(120), star(400), fanOut(60, 6)} {
		l := testLayout(t, s)
		top, bot := math.Inf(1), math.Inf(-1)
		for _, b := range l.boxes {
			top, bot = math.Min(top, b.y), math.Max(bot, b.y+b.h)
		}
		round := 0
		for _, r := range s.Rels {
			for _, q := range l.paths[r].pts {
				if q.Y < top || q.Y > bot {
					round++
					break
				}
			}
		}
		if round > 0 {
			t.Errorf("%s: %d lines go round the group instead of between its boxes", s.Conn, round)
		}
	}
	// a diagram whose gaps all fit is placed exactly as with no widening:
	// every gap between two boxes of a column is stackGap (a gap of
	// groupGap or more is between two groups, which share an x)
	for _, s := range []*Schema{fixture(), chain(12), star(12)} {
		l := testLayout(t, s)
		byX := map[float64][]*box{}
		for _, b := range l.boxes {
			if b.hasRels {
				byX[b.x] = append(byX[b.x], b)
			}
		}
		for _, col := range byX {
			sort.Slice(col, func(i, j int) bool { return col[i].y < col[j].y })
			for i := 1; i < len(col); i++ {
				if g := col[i].y - col[i-1].y - col[i-1].h; g < groupGap && math.Abs(g-stackGap) > 0.01 {
					t.Errorf("%s: gap %.1f above %s, want stackGap", s.Conn, g, col[i].t.Label)
				}
			}
		}
	}
}

// fanOut is a root whose n keys k00…k(n-1) are each referenced by one
// leaf, so every line leaves the root from a slot of its own: nothing to
// share, as in a schema of many distinct foreign keys. Each leaf also
// references one of m mid tables (children of the root's id), which puts
// the leaves a rank past the mids: every line from a k row crosses the
// mids' column. Its gaps are crowded by lines that cannot ride a bus, so
// widen still has to make room.
//
// Keep m at 6 or so. With 8 mids (fanOut(80, 8), fanOut(100, 8)) they wrap
// into three columns that start well above root's first key and end below
// its last, so the lines from root's top and bottom keys meet a mid at
// their own height and the open side over or under the column is the
// shorter way, by hundreds of pixels against any gap: 20-odd lines then
// leave the boxes' extent, and that is the router choosing travel, not
// widen running out of room. Checked for N-092: more widen rounds (up to
// 20) change nothing, a surcharge on the open sides while measuring
// changes nothing (the choice is the final routing's), and with every
// hole's charge removed 14 still go round.
func fanOut(n, m int) *Schema {
	s := &Schema{Conn: fmt.Sprintf("fanout%d", n)}
	root := &Table{Name: "root", Label: "root", PK: []string{"id"}, Cols: []*Column{{Name: "id", Type: "int"}}}
	for i := range n {
		// each a unique key, as a foreign key's target is: key columns
		// are always shown, so no line attaches at the header (where
		// hidden rows' lines would share a slot) however big n is
		k := fmt.Sprintf("k%02d", i)
		root.Cols = append(root.Cols, &Column{Name: k, Type: "int"})
		root.Uniques = append(root.Uniques, []string{k})
	}
	s.Tables = append(s.Tables, root)
	var mids []*Table
	for j := range m {
		t := &Table{Name: fmt.Sprintf("mid%d", j), PK: []string{"id"},
			Cols: []*Column{{Name: "id", Type: "int"}, {Name: "root_id", Type: "int"}, {Name: "a", Type: "text"}, {Name: "b", Type: "text"}}}
		t.Label = t.Name
		// tall, so the mids' column is the group's tallest and a line
		// cannot slip past it through the open space above or below
		for k := range 24 {
			t.Cols = append(t.Cols, &Column{Name: fmt.Sprintf("c%02d", k), Type: "text"})
		}
		mids = append(mids, t)
		s.Tables = append(s.Tables, t)
		s.Rels = append(s.Rels, &Rel{Name: t.Name + "_fk", Child: t, Parent: root, ChildCols: []string{"root_id"}, ParentCols: []string{"id"}})
	}
	for i := range n {
		c := &Table{Name: fmt.Sprintf("leaf%02d", i), PK: []string{"id"},
			Cols: []*Column{{Name: "id", Type: "int"}, {Name: "k", Type: "int"}, {Name: "mid_id", Type: "int"}}}
		c.Label = c.Name
		s.Tables = append(s.Tables, c)
		s.Rels = append(s.Rels,
			&Rel{Name: c.Name + "_k", Child: c, Parent: root, ChildCols: []string{"k"}, ParentCols: []string{fmt.Sprintf("k%02d", i)}},
			&Rel{Name: c.Name + "_mid", Child: c, Parent: mids[i%m], ChildCols: []string{"mid_id"}, ParentCols: []string{"id"}})
	}
	s.MarkKeys()
	s.Sort()
	return s
}

// The lines on one bus share one lane through a hole, and a hole is
// charged for its lanes, not its lines: riding a bus already through it is
// free, so a slot's lines gather rather than spread.
func TestBusSharesLane(t *testing.T) {
	h := &hole{top: 100, bot: 100 + stackGap}
	hub, lone := &bus{shared: true}, &bus{}
	trunk, own := hub.grow(h), lone.grow(h)
	if hub.grow(h) != trunk || hub.step(h) != trunk || (*bus)(nil).step(h) != nil {
		t.Fatal("grow and step must find the node already made")
	}
	x1 := &crossing{idx: 0, yl: 0, yr: 300, holes: []*hole{h}, buses: []*bus{trunk}}
	x2 := &crossing{idx: 1, yl: 0, yr: 50, holes: []*hole{h}, buses: []*bus{trunk}}
	x3 := &crossing{idx: 2, yl: 200, yr: 200, holes: []*hole{h}, buses: []*bus{own}}
	for _, x := range []*crossing{x1, x2, x3} {
		h.join(x, x.buses[0])
	}
	if h.lanes() != 2 {
		t.Errorf("3 lines on 2 buses take %d lanes, want 2", h.lanes())
	}
	if c := h.cost(trunk); c != 0 {
		t.Errorf("riding a bus already through the hole costs %.0f, want 0", c)
	}
	if c := h.cost(nil); c != 2*laneLoad {
		t.Errorf("a new lane beside 2 costs %.0f, want %.0f", c, 2*laneLoad)
	}
	setLanes(h)
	if h.lane[x1] != h.lane[x2] {
		t.Errorf("one bus, two lanes: %.1f and %.1f", h.lane[x1], h.lane[x2])
	}
	// the bus comes from 0 and is headed to 300 and 50 (mean key 87.5),
	// the lone line from 200 to 200: the bus is the upper lane
	if h.lane[x1] >= h.lane[x3] {
		t.Errorf("bus at %.1f, lone line at %.1f: the bus should be above", h.lane[x1], h.lane[x3])
	}
}

// A hub's lines leave its one slot as buses (route.go's BUSES), so a big
// star needs no widening: every gap between two boxes stays stackGap, and
// still no line goes round the group (TestCrowdedGapsWiden) or through a
// box (TestRoutesMissBoxes). Before buses, star(400)'s gaps were widened
// to hold a lane per line, which made it 4426×4571 rather than 4426×3411.
//
// And the buses form a tree: two lines that share a lane through a hole
// share every point before it, back to the hub. Lines that had parted are
// never merged again, so a reader can follow a branch from the trunk.
func TestBusesKeepStarsCompact(t *testing.T) {
	for _, s := range []*Schema{star(45), star(120), star(400)} {
		l := testLayout(t, s)
		byX := map[float64][]*box{}
		for _, b := range l.boxes {
			byX[b.x] = append(byX[b.x], b)
		}
		for _, col := range byX {
			sort.Slice(col, func(i, j int) bool { return col[i].y < col[j].y })
			for i := 1; i < len(col); i++ {
				if g := col[i].y - col[i-1].y - col[i-1].h; math.Abs(g-stackGap) > 0.01 {
					t.Errorf("%s: gap %.1f above %s, want stackGap: a star should need no widening", s.Conn, g, col[i].t.Label)
				}
			}
		}

		// Every horizontal run through a column is a lane. Map each lane
		// (its x span and height) to the points that lead into it; every
		// line through it must have come the same way.
		type lane struct{ x0, x1, y float64 }
		prefix := map[lane][]pt2{}
		runs := 0
		for _, r := range s.Rels {
			pts := l.paths[r].pts
			for i := 1; i < len(pts); i++ {
				a, b := pts[i-1], pts[i]
				// a run through a hole is one straight segment as wide
				// as its column (boxMinW at least); the stubs out of and
				// into the two boxes are shorter
				if a.Y != b.Y || b.X-a.X < boxMinW {
					continue
				}
				runs++
				k := lane{a.X, b.X, a.Y}
				if want, ok := prefix[k]; !ok {
					prefix[k] = pts[:i+1]
				} else if !slices.Equal(want, pts[:i+1]) {
					t.Errorf("%s: %s joins the lane at y %.1f by another way than the lines before it", s.Conn, r.Name, a.Y)
				}
			}
		}
		// with a lane per line, every run would be its own lane; on
		// buses star(45) has 90 runs on 32 lanes, star(120) 420 on 98
		// and star(400) 2765 on 359
		if runs == 0 || len(prefix)*2 > runs {
			t.Errorf("%s: %d runs through holes on %d lanes; a star's lines should share lanes", s.Conn, runs, len(prefix))
		}
	}
}
