package explain

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/rohanthewiz/serr"
)

// PDF renders the picture (see picture.go) as a one-page PDF, the page
// sized to the picture so nothing is scaled or split: a wide plan makes a
// wide page, which a viewer fits to its window and a printer to its paper.
//
// WHY A RASTER INSIDE A PDF, NOT VECTOR PDF. A vector page would need the
// text laid out a second time with PDF's own font machinery — embedding
// and subsetting the Go fonts as CID fonts to reach beyond Latin-1 — to
// draw the same picture the raster already is. At the 2× scale the picture
// is drawn at, the page is sharp on screen and on paper; what it gives up
// is selecting the text, and the statement and findings are one "copy as
// text" away in every dbc view. So the PDF is a thin, dependency-free
// wrapper, written by hand below, around the tested renderer.
//
// The file, object by object:
//
//	1 Catalog ─► 2 Pages ─► 3 Page ─┬─► 4 content stream: "q W 0 0 H 0 0 cm /Im0 Do Q"
//	                                └─► 5 Image XObject (RGB, Flate + PNG "Up" predictor)
//	6 Info (Title, Creator, CreationDate)
//	xref table, trailer
//
// The image is Flate-compressed after the PNG "Up" filter (each row stored
// as its difference from the row above). The picture is mostly flat fills,
// so most rows are all zeros after the filter and compress to almost
// nothing; plain Flate on raw pixels is several times larger.
func (p *Plan) PDF(opt PictureOptions) ([]byte, error) {
	img, k, err := p.picture(opt)
	if err != nil {
		return nil, err
	}
	w, h := img.Bounds().Dx(), img.Bounds().Dy()

	// The page in points. The picture's CSS pixel is the web's 1/96 inch,
	// and a point is 1/72, so one CSS pixel is 0.75 pt — the page prints
	// at the size the Plan tab shows on screen.
	pw, ph := float64(w)/k*0.75, float64(h)/k*0.75

	// Rows with the PNG Up filter: a filter-type byte (2) and then each
	// byte minus the byte above it. The first row's "above" is zeros.
	var raw bytes.Buffer
	zw := zlib.NewWriter(&raw)
	row := make([]byte, 1+3*w)
	prev := make([]byte, 3*w)
	cur := make([]byte, 3*w)
	for y := range h {
		off := y * img.Stride
		for x := range w {
			// the picture is opaque (painted over a solid background), so
			// dropping alpha loses nothing
			copy(cur[3*x:3*x+3], img.Pix[off+4*x:off+4*x+3])
		}
		row[0] = 2
		for i := range cur {
			row[1+i] = cur[i] - prev[i]
		}
		if _, err = zw.Write(row); err != nil {
			return nil, serr.Wrap(err, "op", "compress plan pdf image")
		}
		prev, cur = cur, prev
	}
	if err = zw.Close(); err != nil {
		return nil, serr.Wrap(err, "op", "compress plan pdf image")
	}

	content := fmt.Sprintf("q %.2f 0 0 %.2f 0 0 cm /Im0 Do Q", pw, ph)
	title := "dbc plan"
	if p.Root != nil {
		title = p.Headline()
	}

	var b bytes.Buffer
	var offsets []int
	obj := func(body string, stream []byte) {
		offsets = append(offsets, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\n", len(offsets), body)
		if stream != nil {
			b.WriteString("stream\n")
			b.Write(stream)
			b.WriteString("\nendstream\n")
		}
		b.WriteString("endobj\n")
	}
	// the header's second line is binary, as the spec recommends, so a
	// transfer tool that sniffs content does not treat the file as text
	b.WriteString("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")
	obj("<< /Type /Catalog /Pages 2 0 R >>", nil)
	obj("<< /Type /Pages /Kids [3 0 R] /Count 1 >>", nil)
	obj(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %.2f %.2f] "+
		"/Resources << /XObject << /Im0 5 0 R >> >> /Contents 4 0 R >>", pw, ph), nil)
	obj(fmt.Sprintf("<< /Length %d >>", len(content)), []byte(content))
	obj(fmt.Sprintf("<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace /DeviceRGB "+
		"/BitsPerComponent 8 /Filter /FlateDecode /DecodeParms << /Predictor 12 /Colors 3 /Columns %d >> "+
		"/Length %d >>", w, h, w, raw.Len()), raw.Bytes())
	obj("<< /Title "+pdfText(title)+" /Creator (dbc) /CreationDate "+
		pdfText(time.Now().Format("D:20060102150405-07'00'"))+" >>", nil)

	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, o := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R /Info %d 0 R >>\nstartxref\n%d\n%%%%EOF\n",
		len(offsets)+1, len(offsets), xref)
	return b.Bytes(), nil
}

// pdfText encodes s as a PDF text string. Plain ASCII is a literal string
// with its three special characters escaped; anything else — a headline's
// "·", a connection named in Cyrillic — is UTF-16BE with a byte-order mark,
// the one encoding every reader decodes for metadata, written in hex so no
// byte needs escaping.
func pdfText(s string) string {
	ascii := true
	for _, r := range s {
		if r < 0x20 || r > 0x7e {
			ascii = false
			break
		}
	}
	if ascii {
		return "(" + strings.NewReplacer(`\`, `\\`, "(", `\(`, ")", `\)`).Replace(s) + ")"
	}
	var b strings.Builder
	b.WriteString("<FEFF")
	for _, u := range utf16.Encode([]rune(s)) {
		fmt.Fprintf(&b, "%04X", u)
	}
	b.WriteString(">")
	return b.String()
}
