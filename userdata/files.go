package userdata

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
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
// The temp file is a dotfile so a directory listing taken mid-write does
// not show it as a script or a console.
func writeAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
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
	}
	return err
}
