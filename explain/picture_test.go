package explain

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/rohanthewiz/dbc/theme"
)

// The shareable renderings: the Mermaid chart, the picture, and the PDF
// around it. The picture's looks are checked by eye (it is a picture);
// these hold what a machine can: every fixture renders, the files are what
// they claim to be, the sizes stay bounded, and nothing the user's SQL
// holds can break the chart's syntax.

func TestMermaidShape(t *testing.T) {
	for name, p := range htmlFixtures(t) {
		src := p.Mermaid()
		if !strings.HasPrefix(src, "%% Plan · ") || !strings.Contains(src, "\nflowchart BT\n") {
			t.Errorf("%s: header:\n%s", name, src)
		}
		// one node line per step, and every step but the root feeds one
		// parent: n<child> --> n<parent>
		nodes := regexp.MustCompile(`(?m)^  n\d+\["`).FindAllString(src, -1)
		edges := regexp.MustCompile(`(?m)^  n(\d+) -->(?:\|"[^"]*"\|)? n(\d+)$`).FindAllStringSubmatch(src, -1)
		if len(nodes) != len(p.Nodes()) || len(edges) != len(p.Nodes())-1 {
			t.Errorf("%s: %d nodes, %d edges for %d steps:\n%s", name, len(nodes), len(edges), len(p.Nodes()), src)
		}
		for _, e := range edges {
			c, _ := strconv.Atoi(e[1])
			par, _ := strconv.Atoi(e[2])
			if p.Node(c).Parent() != p.Node(par) {
				t.Errorf("%s: edge n%d --> n%d is not child to parent", name, c, par)
			}
		}
	}
}

// Every label is one double-quoted string whatever the plan says: a quote,
// a # (Mermaid's entity marker), < > & (the label is HTML), a backtick (a
// "markdown string") or a newline in a step's text is written as a code.
func TestMermaidEscapesHostileText(t *testing.T) {
	p := &Plan{Engine: Postgres, Conn: "prod\nline", Root: &Node{Op: `Seq Scan "x"`, Relation: "t#1",
		Props:    []Prop{{Key: "Filter", Value: "(a <> '</b>' AND b = `x` & c)\nOR d"}},
		Children: []*Node{{Op: "Index Scan", Relationship: `Sub|"Plan"`}}}}
	p.Finalize()
	src := p.Mermaid()
	for _, line := range strings.Split(strings.TrimSpace(src), "\n") {
		if strings.HasPrefix(line, "%%") || !strings.Contains(line, `"`) {
			continue
		}
		// between the label's own quotes, no quote, <, >, &, backtick or a
		// # that does not start an entity code
		inner := line[strings.Index(line, `"`)+1 : strings.LastIndex(line, `"`)]
		inner = strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(inner, "<br/>", ""), "<i>", ""), "</i>", "")
		inner = regexp.MustCompile(`#(quot|\d+);`).ReplaceAllString(inner, "")
		if strings.ContainsAny(inner, "\"<>&`#") {
			t.Errorf("unescaped syntax in %q", line)
		}
	}
	if strings.Contains(src, "prod\nline") {
		t.Error("a newline in the connection name broke out of its %% comment")
	}
	if !strings.Contains(src, "Seq Scan #quot;x#quot;") || !strings.Contains(src, "#60;#62; '#60;/b#62;'") {
		t.Errorf("escaping:\n%s", src)
	}
}

// A hot step and a step with a finding are classed, so a renderer that
// draws styles colors them.
func TestMermaidClasses(t *testing.T) {
	p := htmlFixtures(t)["my_analyze_subq.txt"]
	src := p.Mermaid()
	for _, want := range []string{"classDef hot ", "classDef crit ", "class "} {
		if !strings.Contains(src, want) {
			t.Errorf("no %q in:\n%s", want, src)
		}
	}
}

func TestPictureRendersEveryFixture(t *testing.T) {
	for name, p := range htmlFixtures(t) {
		for _, pal := range []theme.Palette{{}, theme.Light()} {
			img, err := p.Picture(PictureOptions{Palette: pal, Scale: 1})
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			b := img.Bounds()
			if b.Dx() < int(picMinW) || b.Dy() < 300 {
				t.Errorf("%s: %v is too small for a header, a tree and findings", name, b)
			}
			// the corner is the page background: the zero palette is dbc's dark
			want := theme.Default().Bg
			if pal.Bg != "" {
				want = pal.Bg
			}
			if got := hexOf(img.At(1, 1)); got != want {
				t.Errorf("%s: background %s, want %s", name, got, want)
			}
		}
	}
}

// Scale multiplies the picture, and an unknown metric falls back to the
// plan's own instead of failing.
func TestPictureScaleAndMetric(t *testing.T) {
	p := htmlFixtures(t)["pg_analyze_join.json"]
	one, _ := p.Picture(PictureOptions{Scale: 1})
	two, _ := p.Picture(PictureOptions{})
	if two.Bounds().Dx() != 2*one.Bounds().Dx() && two.Bounds().Dx() != 2*one.Bounds().Dx()-1 {
		t.Errorf("default scale: %v vs %v at 1×", two.Bounds(), one.Bounds())
	}
	if _, err := p.Picture(PictureOptions{Metric: "nonsense", Scale: 1}); err != nil {
		t.Error(err)
	}
}

// A plan too big to draw at 2× is drawn smaller, inside the pixel and side
// limits, and folded below depth 5 as the page folds it.
func TestPictureBoundsAHugePlan(t *testing.T) {
	root := &Node{Op: "Append", Kind: KindSet}
	for i := 0; i < 400; i++ {
		root.Children = append(root.Children, &Node{Op: "Seq Scan", Relation: fmt.Sprintf("part_%d", i)})
	}
	p := &Plan{Engine: Postgres, Root: root}
	p.Finalize()
	img, err := p.Picture(PictureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	b := img.Bounds()
	if b.Dx()*b.Dy() > maxPixels || float64(b.Dx()) > maxSide+1 || float64(b.Dy()) > maxSide+1 {
		t.Errorf("%v is past the limits", b)
	}

	// a deep chain of 130 steps: folded at depth 5, so the tree is 6 rows
	deep := &Node{Op: "Nested Loop"}
	n := deep
	for i := 0; i < 130; i++ {
		c := &Node{Op: "Nested Loop"}
		n.Children = []*Node{c, {Op: "Index Scan"}}
		n = c
	}
	p = &Plan{Engine: Postgres, Root: deep}
	p.Finalize()
	lay := &picLayout{plan: p, metric: p.Metric, pal: newPicPalette(theme.Default())}
	fs, _ := picFonts()
	lay.meas = newPicFaces(fs, 1)
	defer lay.meas.close()
	lay.build("")
	if rows := int((lay.treeH-34+picVGap)/(picCardH+picVGap) + 0.5); rows != picFoldDepth+1 {
		t.Errorf("folded tree has %d rows, want %d", rows, picFoldDepth+1)
	}
}

func TestJPEGAndPNGDecode(t *testing.T) {
	p := htmlFixtures(t)["pg_subplan.json"]
	j, err := p.JPEG(PictureOptions{Scale: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = jpeg.Decode(bytes.NewReader(j)); err != nil {
		t.Errorf("jpeg: %v", err)
	}
	g, err := p.PNG(PictureOptions{Scale: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = png.Decode(bytes.NewReader(g)); err != nil {
		t.Errorf("png: %v", err)
	}
}

// The PDF, read back the way a reader does: the xref points at each object,
// the page is the picture's size in points, and the image stream inflates
// and un-filters to the picture's own pixels.
func TestPDFStructure(t *testing.T) {
	p := htmlFixtures(t)["pg_estimate_join.json"]
	p.Conn = "prod — Москва"
	opt := PictureOptions{Scale: 1, Palette: theme.Light()}
	doc, err := p.PDF(opt)
	if err != nil {
		t.Fatal(err)
	}
	s := string(doc)
	if !strings.HasPrefix(s, "%PDF-1.4\n") || !strings.HasSuffix(s, "%%EOF\n") {
		t.Fatalf("not a PDF: %.40q … %q", s, s[len(s)-10:])
	}

	// every xref entry lands on "N 0 obj"
	start, _ := strconv.Atoi(regexp.MustCompile(`startxref\n(\d+)`).FindStringSubmatch(s)[1])
	entries := regexp.MustCompile(`(\d{10}) 00000 n `).FindAllStringSubmatch(s[start:], -1)
	if len(entries) != 6 {
		t.Fatalf("%d xref entries, want 6", len(entries))
	}
	for i, e := range entries {
		off, _ := strconv.Atoi(e[1])
		if want := fmt.Sprintf("%d 0 obj", i+1); !strings.HasPrefix(s[off:], want) {
			t.Errorf("xref %d points at %.12q, want %q", i+1, s[off:], want)
		}
	}

	img, _ := p.Picture(opt)
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	if want := fmt.Sprintf("/MediaBox [0 0 %.2f %.2f]", float64(w)*0.75, float64(h)*0.75); !strings.Contains(s, want) {
		t.Errorf("no %s", want)
	}
	if !strings.Contains(s, "/Title <FEFF") {
		t.Error("a non-ASCII title should be UTF-16 hex")
	}

	// the image stream: inflate, undo the Up predictor, compare pixels
	m := regexp.MustCompile(`/Length (\d+) >>\nstream\n`).FindAllStringSubmatchIndex(s, -1)
	last := m[len(m)-1]
	n, _ := strconv.Atoi(s[last[2]:last[3]])
	zr, err := zlib.NewReader(bytes.NewReader(doc[last[1] : last[1]+n]))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil || len(raw) != h*(1+3*w) {
		t.Fatalf("image stream: %v, %d bytes, want %d", err, len(raw), h*(1+3*w))
	}
	prev := make([]byte, 3*w)
	for y := 0; y < h; y++ {
		row := raw[y*(1+3*w):]
		if row[0] != 2 {
			t.Fatalf("row %d filter %d, want 2 (Up)", y, row[0])
		}
		for i := range prev {
			prev[i] += row[1+i]
		}
		for _, x := range []int{0, w / 2, w - 1} {
			r, g, b, _ := img.At(x, y).RGBA()
			if prev[3*x] != uint8(r>>8) || prev[3*x+1] != uint8(g>>8) || prev[3*x+2] != uint8(b>>8) {
				t.Fatalf("pixel (%d,%d) differs from the picture", x, y)
			}
		}
	}
}

func TestPDFText(t *testing.T) {
	for in, want := range map[string]string{
		"plain":       "(plain)",
		`a (b) \ c`:   `(a \(b\) \\ c)`,
		"Plan · pg":   "<FEFF0050006C0061006E002000B7002000700067>",
		"line\nbreak": "<FEFF006C0069006E0065000A0062007200650061006B>",
	} {
		if got := pdfText(in); got != want {
			t.Errorf("pdfText(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestWriteFileNames(t *testing.T) {
	p := &Plan{Engine: SQLite, Conn: "demo sqlite"}
	path, err := p.WriteFile(t.TempDir(), "pdf", []byte("%PDF"))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`/plan-demo_sqlite-\d{8}-\d{6}\.pdf$`).MatchString(path) {
		t.Errorf("path %s", path)
	}
}

func hexOf(c color.Color) string {
	r, g, b, _ := c.RGBA()
	return fmt.Sprintf("#%02x%02x%02x", r>>8, g>>8, b>>8)
}
