//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || solaris || illumos

package db

import (
	"crypto/tls"
	"net"
	"syscall"
)

// sockQuiet reports whether nothing has arrived on an idle connection's
// socket: no bytes waiting and no FIN. It makes no round trip. It asks the
// kernel with one non-blocking recv, which finds any bytes the server sent
// while the connection sat in the pool.
//
// This is the check go-sql-driver/mysql runs on every checkout (connCheck),
// with one difference: MSG_PEEK. The MySQL driver discards a connection
// that has any bytes waiting, so it can read the byte. Here waiting bytes only
// trigger a ping. They may be legitimate (a NOTIFY for a LISTEN a
// user ran, a ParameterStatus), and pgx has to read them intact.
//
// Under TLS the peek goes to the TCP socket beneath, where pending bytes are
// an encrypted record. That is still "something arrived", which is all the
// caller needs.
//
// Anything it cannot check (a conn with no file descriptor, a syscall
// error) counts as quiet. The caller then falls back to pgx's own rule, so
// the peek can only add pings, never skip one pgx would have made.
func sockQuiet(c net.Conn) bool {
	if tc, ok := c.(*tls.Conn); ok {
		c = tc.NetConn()
	}
	sc, ok := c.(syscall.Conn)
	if !ok {
		return true
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return true
	}
	quiet := true
	err = raw.Read(func(fd uintptr) bool {
		var buf [1]byte
		// The Go runtime keeps its sockets non-blocking, so an empty
		// socket answers EAGAIN at once rather than waiting.
		n, _, rerr := syscall.Recvfrom(int(fd), buf[:], syscall.MSG_PEEK)
		switch {
		case n > 0: // bytes waiting: an async message, or a FATAL before a close
			quiet = false
		case n == 0 && rerr == nil: // FIN: the server closed its end
			quiet = false
		case rerr == syscall.EAGAIN || rerr == syscall.EWOULDBLOCK:
			// nothing arrived: quiet
		default: // e.g. ECONNRESET: the connection is broken
			quiet = false
		}
		// true: done. Returning false would park this goroutine in the
		// netpoller until the socket became readable, which on a healthy
		// idle connection may never happen.
		return true
	})
	return err != nil || quiet
}
