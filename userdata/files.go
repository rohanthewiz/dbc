package userdata

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rohanthewiz/btypedb"
	"github.com/rohanthewiz/serr"
)

// Pieces shared by the stores that keep user text as plain files (consoles,
// scripts, schema picks): a content revision, and a write that never leaves
// a half-written file behind.

// textRev is the revision of a file's text: a hash of the content, so any
// writer (the TUI, another dbc web window, vim) changing the file changes
// it, with no counter to keep in step. A file that does not exist is
// revision "", which is how a save tells "create" from "overwrite".
func textRev(text string, exists bool) string {
	if !exists {
		return ""
	}
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:8])
}

// writeAtomic replaces path with data through a temp file in the same
// directory renamed over it. A reader (another process, `dbc script` from
// cron) sees the old file or the new one, never a torn write, and a crash
// mid-write leaves the old file intact. The parent directory is created.
//
// The temp file is synced before the rename and the directory after it:
// without the first, a power loss soon after can leave the renamed file
// zero-length (the rename reached the disk, the data not yet); without the
// second, the rename itself can be lost. The directory's sync is best
// effort — Windows cannot sync a directory, and the file's own sync is the
// part that keeps the text. This is what every user file goes through:
// consoles and the editor buffer, scripts and pipeline specs, run records,
// the layout, conversations, schema picks, the history's trim (N-255).
//
// A path that is a symlink stays one: the file it points at is replaced,
// not the link (a console or a script linked into a repository).
//
// The temp file is a dotfile so a directory listing taken mid-write does
// not show it as a script or a console. One left behind by a crash between
// create and rename is swept the first time this process writes into that
// directory (sweepTemps).
func writeAtomic(path string, data []byte, perm os.FileMode) error {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	sweepTemps(dir)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+tmpMark+"*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	// CreateTemp makes the file 0600; perm is what the caller wants kept
	if err == nil {
		err = os.Chmod(tmp.Name(), perm)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	syncDir(dir)
	return nil
}

// tmpMark is in every temp file writeAtomic makes: ".<name>.tmp-<random>".
const tmpMark = ".tmp-"

// syncDir flushes a directory's entries — a rename into it — to disk, as
// far as the OS allows; an error is ignored (see writeAtomic).
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

// staleTemp is how old a temp file must be to count as leaked. A write
// renames its temp file within milliseconds; an hour-old one belongs to a
// process that died mid-write.
const staleTemp = time.Hour

// swept is the directories sweepTemps has already looked at, this process.
var swept sync.Map

// sweepTemps removes leaked temp files from dir: writeAtomic's dotfiles
// (".<name>.tmp-…") and the layout's older ".tui-layout-…" ones, past
// staleTemp. Once per directory per process, on its first write, so a
// directory written every few seconds (a running job's record, an
// autosaved console) is read once, not at every write.
func sweepTemps(dir string) {
	if _, done := swept.LoadOrStore(dir, true); done {
		return
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasPrefix(n, ".") || !(strings.Contains(n, tmpMark) || strings.HasPrefix(n, ".tui-layout-")) {
			continue
		}
		if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > staleTemp {
			_ = os.Remove(filepath.Join(dir, n))
		}
	}
}

// fileLockWait bounds how long a writer waits for another's lock on a
// file (lockFile). Writers hold it for milliseconds. (Snippets' lockWait is
// their bytdb file's, not this.)
const fileLockWait = 3 * time.Second

// lockFile takes path's sidecar lock (btypedb.AcquireLock: path.lock, flock
// on Unix, a share mode on Windows, dropped by the OS if the holder dies),
// waiting up to fileLockWait for another process to finish. It orders writers
// of one file across processes — the history's appends against its trim —
// where the in-process mutex cannot. config's saved-connections file uses
// the same lock the same way.
func lockFile(path string) (io.Closer, error) {
	deadline := time.Now().Add(fileLockWait)
	for {
		lock, err := btypedb.AcquireLock(path)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, btypedb.ErrLocked) || time.Now().After(deadline) {
			return nil, serr.Wrap(err, "op", "lock", "path", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
