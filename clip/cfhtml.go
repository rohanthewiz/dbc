package clip

import "fmt"

// cfHTML wraps an HTML fragment in the envelope Windows' "HTML Format"
// clipboard flavor requires (CF_HTML). It is platform-neutral Go so it can be
// tested on every machine, even though only the Windows writer uses it.
//
// The envelope is a text header of BYTE offsets into the whole payload,
// followed by the document:
//
//	Version:0.9
//	StartHTML:0000000105      ← offset of "<html>"
//	EndHTML:0000000262        ← offset just past "</html>"
//	StartFragment:0000000141  ← offset just past "<!--StartFragment-->"
//	EndFragment:0000000226    ← offset of "<!--EndFragment-->"
//	<html><body>
//	<!--StartFragment--> …the fragment… <!--EndFragment-->
//	</body></html>
//
// The pasting app reads the header, then slices the fragment out by those
// offsets, so a single wrong byte count pastes nothing or garbage. Two
// choices keep the arithmetic simple and exact:
//
//   - Every offset is printed as ten zero-padded digits, so the header's own
//     length is a constant and the offsets can be computed before they are
//     written. (Microsoft's own samples do this.)
//   - Offsets count UTF-8 bytes, which is what len() of a Go string gives.
//     CF_HTML is defined as UTF-8, so a fragment with "café" in it is two
//     bytes longer than it has runes, and counting runes would be off by one.
func cfHTML(fragment string) string {
	const (
		headerFmt = "Version:0.9\r\n" +
			"StartHTML:%010d\r\n" +
			"EndHTML:%010d\r\n" +
			"StartFragment:%010d\r\n" +
			"EndFragment:%010d\r\n"
		pre  = "<html><body>\r\n<!--StartFragment-->"
		post = "<!--EndFragment-->\r\n</body></html>"
	)
	// Length of the header with any offsets filled in — constant, thanks to
	// the fixed-width fields.
	headerLen := len(fmt.Sprintf(headerFmt, 0, 0, 0, 0))

	startHTML := headerLen
	startFrag := startHTML + len(pre)
	endFrag := startFrag + len(fragment)
	endHTML := endFrag + len(post)

	return fmt.Sprintf(headerFmt, startHTML, endHTML, startFrag, endFrag) +
		pre + fragment + post
}
