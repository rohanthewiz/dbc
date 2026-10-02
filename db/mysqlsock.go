package db

import (
	"net"
	"reflect"
	"unsafe"
)

// mysqlConnType is go-sql-driver/mysql's driver connection type, as
// reflect names it. mysqlNetConn reads only that type.
const (
	mysqlConnPkg  = "github.com/go-sql-driver/mysql"
	mysqlConnName = "mysqlConn"
)

// mysqlNetConn returns the socket under a go-sql-driver/mysql driver
// connection (what sql.Conn.Raw hands out), or nil for anything else.
//
// The driver keeps it in an unexported field, netConn (the *tls.Conn over
// the socket when TLS is on, which sockQuiet unwraps), and offers no
// accessor. The session guard needs it for the same socket peek it does on
// Postgres (sockQuiet): without it a MySQL session cut by the server less
// than sessionPingIdle after its last statement sent the next one into the
// dead socket and failed (N-088), where Postgres found the cut first and
// retried.
//
// Reading the field takes reflect plus unsafe, since reflect will not
// Interface() an unexported field. That is safe here: the caller is inside
// sql.Conn.Raw, which holds the connection exclusively, and the driver
// assigns netConn only while connecting (and swaps in the TLS conn during
// the handshake), never while a connection is in use. The lookup is by
// name and type, so a driver release that renames or retypes the field
// makes this return nil, which is the old behavior (the idle threshold
// alone), not a crash; TestMySQLNetConnFindsTheSocket fails on such a
// release so it is noticed.
func mysqlNetConn(dc any) net.Conn {
	v := reflect.ValueOf(driverConn(dc)) // past mysqlKillConn, to the driver's own
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return nil
	}
	e := v.Elem()
	if e.Kind() != reflect.Struct || e.Type().PkgPath() != mysqlConnPkg || e.Type().Name() != mysqlConnName {
		return nil
	}
	f := e.FieldByName("netConn")
	if !f.IsValid() || f.Type() != reflect.TypeFor[net.Conn]() || !f.CanAddr() {
		return nil
	}
	// a readable copy of the field: same type, same address, without the
	// read-only flag reflect puts on an unexported field
	nc, _ := reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem().Interface().(net.Conn)
	return nc
}
