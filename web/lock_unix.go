//go:build !windows

package web

import (
	"errors"
	"os"
	"syscall"
)

// lockFile takes an exclusive advisory lock on f without waiting. flock
// locks belong to the open file, so the OS drops the lock when the process
// exits — a crashed dbc web leaves no stale lock behind.
func lockFile(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return errLocked
	}
	return err
}
