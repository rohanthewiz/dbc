//go:build unix

package main

import "syscall"

// detachAttr starts the child in a session of its own: no controlling
// terminal, so a shell's hangup or Ctrl+C after the parent returned does
// not reach the run.
func detachAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }
