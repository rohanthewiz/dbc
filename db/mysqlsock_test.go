//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || solaris || illumos

package db

import (
	"context"
	"database/sql"
	"encoding/binary"
	"io"
	"net"
	"testing"

	"github.com/go-sql-driver/mysql"
)

// fakeMySQL accepts one client and does just enough of the MySQL protocol
// for go-sql-driver/mysql to call it connected: a v10 handshake offering
// mysql_native_password, then OK to whatever auth the client sends. The
// accepted socket is handed back so the test can hang up as a server would.
func fakeMySQL(t *testing.T) (addr string, accepted <-chan net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	out := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		t.Cleanup(func() { c.Close() })
		packet := func(seq byte, body []byte) {
			h := []byte{byte(len(body)), byte(len(body) >> 8), byte(len(body) >> 16), seq}
			_, _ = c.Write(append(h, body...))
		}
		// capabilities: LONG_PASSWORD | PROTOCOL_41 | TRANSACTIONS |
		// SECURE_CONNECTION | PLUGIN_AUTH
		const caps = 0x1 | 0x200 | 0x2000 | 0x8000 | 0x80000
		hs := []byte{0x0a}
		hs = append(hs, "8.4.0-fake\x00"...)
		hs = binary.LittleEndian.AppendUint32(hs, 42) // connection id
		hs = append(hs, "abcdefgh"...)                // auth data, part 1
		hs = append(hs, 0)
		hs = binary.LittleEndian.AppendUint16(hs, caps&0xffff)
		hs = append(hs, 0xff)                               // utf8mb4_0900_ai_ci
		hs = binary.LittleEndian.AppendUint16(hs, 0x0002)   // autocommit
		hs = binary.LittleEndian.AppendUint16(hs, caps>>16) // upper capabilities
		hs = append(hs, 21)                                 // auth data length
		hs = append(hs, make([]byte, 10)...)                // reserved
		hs = append(hs, "ijklmnopqrst\x00"...)              // auth data, part 2
		hs = append(hs, "mysql_native_password\x00"...)
		packet(0, hs)
		var h [4]byte
		if _, err := io.ReadFull(c, h[:]); err != nil {
			return
		}
		n := int(h[0]) | int(h[1])<<8 | int(h[2])<<16
		if _, err := io.ReadFull(c, make([]byte, n)); err != nil {
			return
		}
		packet(h[3]+1, []byte{0x00, 0, 0, 0x02, 0, 0, 0}) // OK
		out <- c
	}()
	return ln.Addr().String(), out
}

// mysqlNetConn reaches go-sql-driver/mysql's socket, and sockQuiet on it
// sees the server hang up, which is the session guard's early check on
// MySQL (N-088). It reads an unexported field by name, so this is also the
// test that fails when a driver release renames it.
func TestMySQLNetConnFindsTheSocket(t *testing.T) {
	addr, accepted := fakeMySQL(t)
	cfg := mysql.NewConfig()
	cfg.Net, cfg.Addr, cfg.User, cfg.Passwd = "tcp", addr, "u", "p"
	cn, err := mysql.NewConnector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dbh := sql.OpenDB(cn)
	defer dbh.Close()
	conn, err := dbh.Conn(context.Background())
	if err != nil {
		t.Fatalf("connect to the fake server: %v", err)
	}
	defer conn.Close()
	server := <-accepted

	var nc net.Conn
	_ = conn.Raw(func(dc any) error {
		nc = mysqlNetConn(dc)
		return nil
	})
	if nc == nil {
		t.Fatal("mysqlNetConn found no socket: has go-sql-driver/mysql renamed mysqlConn.netConn?")
	}
	if !sockQuiet(nc) {
		t.Fatal("an idle connection reads as not quiet")
	}
	// a KILL or wait_timeout: the server's error packet, then its FIN
	server.Close()
	eventually(t, nc, false)

	if mysqlNetConn(&struct{ netConn net.Conn }{}) != nil || mysqlNetConn(nil) != nil {
		t.Error("only go-sql-driver/mysql's connection is read")
	}
}
