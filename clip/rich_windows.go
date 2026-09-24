//go:build windows

package clip

import (
	"runtime"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/rohanthewiz/serr"
)

// Windows: "HTML Format" (CF_HTML) and CF_UNICODETEXT set in one
// open/empty/set/close sequence, straight through user32 — no PowerShell,
// no cgo.
//
// THE SEQUENCE AND ITS RULES:
//
//	OpenClipboard(0)            may fail while another app holds it → retry
//	EmptyClipboard()            makes this process the owner; required before Set
//	SetClipboardData(fmt, hmem) once per flavor; on success Windows OWNS hmem
//	CloseClipboard()            always, or every other app's copy breaks
//
// The memory handed to SetClipboardData must come from GlobalAlloc with
// GMEM_MOVEABLE, filled while locked. After a successful Set it belongs to
// the system and must not be freed; after a failed one it is still ours, and
// is freed here so a refused write does not leak.
//
// The clipboard is owned per THREAD, and a goroutine can migrate between OS
// threads at any function call, so the whole sequence runs locked to one.

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procOpenClipboard           = user32.NewProc("OpenClipboard")
	procCloseClipboard          = user32.NewProc("CloseClipboard")
	procEmptyClipboard          = user32.NewProc("EmptyClipboard")
	procSetClipboardData        = user32.NewProc("SetClipboardData")
	procRegisterClipboardFormat = user32.NewProc("RegisterClipboardFormatW")

	procGlobalAlloc  = kernel32.NewProc("GlobalAlloc")
	procGlobalFree   = kernel32.NewProc("GlobalFree")
	procGlobalLock   = kernel32.NewProc("GlobalLock")
	procGlobalUnlock = kernel32.NewProc("GlobalUnlock")

	// RtlMoveMemory does the copy into the locked block. Copying with a Go
	// slice would mean converting GlobalLock's uintptr into an unsafe.Pointer,
	// which go vet rightly flags: the GC cannot know what that integer points
	// at. Letting Windows copy from a Go pointer into its own address keeps
	// every conversion in the direction the unsafe rules allow.
	procRtlMoveMemory = kernel32.NewProc("RtlMoveMemory")
)

const (
	cfUnicodeText = 13
	gmemMoveable  = 0x0002
)

// platformWriteRich sets the plain-text and HTML flavors together.
func platformWriteRich(c Content) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	name, err := windows.UTF16PtrFromString("HTML Format")
	if err != nil {
		return serr.Wrap(err, "op", "clipboard-rich")
	}
	htmlFmt, _, callErr := procRegisterClipboardFormat.Call(uintptr(unsafe.Pointer(name)))
	if htmlFmt == 0 {
		return serr.Wrap(callErr, "op", "clipboard-rich", "step", "RegisterClipboardFormat")
	}

	if err = openClipboard(); err != nil {
		return err
	}
	defer procCloseClipboard.Call()

	if r, _, callErr := procEmptyClipboard.Call(); r == 0 {
		return serr.Wrap(callErr, "op", "clipboard-rich", "step", "EmptyClipboard")
	}

	// CF_UNICODETEXT is NUL-terminated UTF-16; CF_HTML is NUL-terminated
	// UTF-8 in its offset envelope (see cfHTML).
	text := utf16.Encode([]rune(c.Text + "\x00"))
	textBytes := unsafe.Slice((*byte)(unsafe.Pointer(&text[0])), len(text)*2)
	if err = setData(cfUnicodeText, textBytes); err != nil {
		return err
	}
	return setData(htmlFmt, []byte(cfHTML(c.HTML)+"\x00"))
}

// openClipboard retries briefly: another program (a clipboard manager, the
// app the user just copied from) may hold the clipboard for a few
// milliseconds, and failing the copy over that would be a coin flip.
func openClipboard() error {
	var lastErr error
	for range 10 {
		r, _, callErr := procOpenClipboard.Call(0)
		if r != 0 {
			return nil
		}
		lastErr = callErr
		time.Sleep(20 * time.Millisecond)
	}
	return serr.Wrap(lastErr, "op", "clipboard-rich", "step", "OpenClipboard")
}

// setData copies b into moveable global memory and hands it to the clipboard
// under the given format.
func setData(format uintptr, b []byte) error {
	h, _, callErr := procGlobalAlloc.Call(gmemMoveable, uintptr(len(b)))
	if h == 0 {
		return serr.Wrap(callErr, "op", "clipboard-rich", "step", "GlobalAlloc")
	}
	p, _, callErr := procGlobalLock.Call(h)
	if p == 0 {
		procGlobalFree.Call(h)
		return serr.Wrap(callErr, "op", "clipboard-rich", "step", "GlobalLock")
	}
	if len(b) > 0 {
		procRtlMoveMemory.Call(p, uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)))
	}
	procGlobalUnlock.Call(h)

	if r, _, callErr := procSetClipboardData.Call(format, h); r == 0 {
		procGlobalFree.Call(h) // still ours: the system only takes it on success
		return serr.Wrap(callErr, "op", "clipboard-rich", "step", "SetClipboardData")
	}
	return nil
}
