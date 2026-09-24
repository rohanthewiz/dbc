//go:build darwin

package clip

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"strings"

	"github.com/rohanthewiz/serr"
)

// macOS: both flavors in one NSPasteboard write, through JavaScript for
// Automation.
//
// WHY osascript AND NOT cgo. dbc builds with CGO_ENABLED=0 everywhere (the
// SQLite driver is pure Go precisely so it can), and NSPasteboard is an
// Objective-C API. osascript ships with every macOS and its JavaScript
// dialect has a full Objective-C bridge, so a dozen lines of script reach
// NSPasteboard with no build-time cost. pbcopy cannot do this: it writes one
// flavor, plain text.
//
// WHY THE PAYLOAD GOES IN ON STDIN AS JSON. Passing the text as an argument
// would hit ARG_MAX on a large result and need AppleScript-string escaping
// that no one gets right the first time. JSON on stdin has no size limit
// worth worrying about and one well-tested encoder on each side.
//
// Both flavors are set after a single clearContents, which is what makes them
// one clipboard entry: a paste target sees text and HTML for the same copy.
const pasteboardJXA = `
ObjC.import('AppKit');
var data = $.NSFileHandle.fileHandleWithStandardInput.readDataToEndOfFile;
var str = $.NSString.alloc.initWithDataEncoding(data, $.NSUTF8StringEncoding);
var p = JSON.parse(ObjC.unwrap(str));
var pb = $.NSPasteboard.generalPasteboard;
pb.clearContents;
var ok = pb.setStringForType($(p.text), $.NSPasteboardTypeString);
if (p.html) { ok = pb.setStringForType($(p.html), $.NSPasteboardTypeHTML) && ok; }
if (!ok) { throw new Error('the pasteboard refused the write'); }
`

// platformWriteRich sets the plain-text and HTML flavors together.
func platformWriteRich(c Content) error {
	payload, err := json.Marshal(struct {
		Text string `json:"text"`
		HTML string `json:"html"`
	}{c.Text, c.HTML})
	if err != nil {
		return serr.Wrap(err, "op", "clipboard-rich")
	}
	cmd := exec.Command("osascript", "-l", "JavaScript", "-e", pasteboardJXA)
	cmd.Stdin = bytes.NewReader(payload)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err = cmd.Run(); err != nil {
		// osascript's own message ("execution error: …") is the useful part;
		// the exit status alone says nothing.
		return serr.Wrap(err, "op", "clipboard-rich",
			"stderr", strings.TrimSpace(stderr.String()))
	}
	return nil
}
