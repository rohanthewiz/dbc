//go:build windows

package main

import "syscall"

// detachAttr starts the child as a process group of its own with no
// console: closing the parent's console, or its Ctrl+C, does not reach
// the run. (DETACHED_PROCESS, 0x8, has no constant in syscall.)
func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | 0x00000008}
}
