//go:build !linux && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd && !solaris && !illumos

package db

import "net"

// sockQuiet has no non-blocking peek on these platforms, so every socket
// counts as quiet. Postgres checkouts then follow pgx's own rule (ping after
// 1s idle), as go-sql-driver/mysql's connCheck does on the same platforms.
func sockQuiet(net.Conn) bool { return true }
