//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || solaris || illumos

package db

import (
	"io"
	"net"
	"testing"
	"time"
)

// tcpPair returns both ends of a loopback TCP connection. sockQuiet peeks at
// a real socket, so net.Pipe (no file descriptor) would not exercise it.
func tcpPair(t *testing.T) (client, server net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		accepted <- c
	}()
	client, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	server = <-accepted
	if server == nil {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() { client.Close(); server.Close() })
	return client, server
}

// eventually polls sockQuiet until it reports want. Bytes and FINs cross the
// loopback asynchronously, so a single look straight after the server acts
// could race them.
func eventually(t *testing.T, c net.Conn, want bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for sockQuiet(c) != want {
		if time.Now().After(deadline) {
			t.Fatalf("sockQuiet = %v, want %v", !want, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSockQuiet(t *testing.T) {
	t.Run("idle", func(t *testing.T) {
		client, _ := tcpPair(t)
		// Nothing was sent, so this must answer at once and not block
		// waiting for bytes.
		done := make(chan bool, 1)
		go func() { done <- sockQuiet(client) }()
		select {
		case q := <-done:
			if !q {
				t.Error("an idle, open socket reported not quiet")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("sockQuiet blocked on an idle socket")
		}
	})

	t.Run("bytes waiting are peeked, not consumed", func(t *testing.T) {
		client, server := tcpPair(t)
		if _, err := server.Write([]byte("N")); err != nil {
			t.Fatalf("write: %v", err)
		}
		eventually(t, client, false)
		// A NOTIFY that arrived while idle has to reach pgx intact.
		buf := make([]byte, 1)
		_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := io.ReadFull(client, buf); err != nil || buf[0] != 'N' {
			t.Errorf("after the peek, read %q, %v; want the byte still there", buf, err)
		}
	})

	t.Run("peer closed", func(t *testing.T) {
		client, server := tcpPair(t)
		server.Close()
		eventually(t, client, false)
	})

	// Without a file descriptor there is nothing to peek at. It counts as
	// quiet, so the caller keeps pgx's own rule.
	t.Run("no descriptor", func(t *testing.T) {
		a, b := net.Pipe()
		defer a.Close()
		defer b.Close()
		if !sockQuiet(a) {
			t.Error("a conn without a descriptor reported not quiet")
		}
	})
}
