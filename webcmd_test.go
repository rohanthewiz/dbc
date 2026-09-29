package main

import (
	"io"
	"testing"
	"time"
)

// exitOnEOF must wait out anything written while the pipe is open and stop
// only once its write end is closed — the app wrapper's lifeline.
func TestExitOnEOF(t *testing.T) {
	r, w := io.Pipe()
	stopped := make(chan struct{})
	go exitOnEOF(r, func() { close(stopped) })

	if _, err := w.Write([]byte("noise the server ignores\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
		t.Fatal("stopped while the pipe was still open")
	case <-time.After(50 * time.Millisecond):
	}

	_ = w.Close()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("did not stop after the pipe closed")
	}
}

// A read error ends the wait as EOF does: nothing is left to watch.
func TestExitOnEOFReadError(t *testing.T) {
	r, w := io.Pipe()
	stopped := make(chan struct{})
	go exitOnEOF(r, func() { close(stopped) })
	_ = w.CloseWithError(io.ErrUnexpectedEOF)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("did not stop after a read error")
	}
}
