//go:build unix

package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rohanthewiz/logger"
)

// A headless run's first Ctrl+C cancels its context; a second exits at
// once with status 130, whatever the run is doing (N-268). Before, every
// signal after the first was caught and dropped, and a run that ignored
// its cancel could only be ended with SIGKILL. In a child process, which
// interrupts itself twice and then waits as a stuck run would.
func TestSecondInterruptExits(t *testing.T) {
	if os.Getenv("DBC_TEST_INTERRUPT") == "1" {
		logger.InitLog(logger.LogConfig{Formatter: "text", LogLevel: "warn"}) // as main does
		ctx, stop := interruptible()
		defer stop()
		_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
		select {
		case <-ctx.Done():
			fmt.Fprintln(os.Stderr, "first: canceled")
		case <-time.After(5 * time.Second):
			fmt.Fprintln(os.Stderr, "first: not canceled")
			os.Exit(4)
		}
		_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
		time.Sleep(5 * time.Second) // a run that does not stop
		os.Exit(0)                  // the second signal was swallowed
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestSecondInterruptExits$")
	cmd.Env = append(os.Environ(), "DBC_TEST_INTERRUPT=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	start := time.Now()
	err := cmd.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 130 {
		t.Fatalf("child: %v after %s (stderr %q), want exit status 130", err, time.Since(start), stderr.String())
	}
	if time.Since(start) > 4*time.Second {
		t.Errorf("the second signal took %s to end it, want at once", time.Since(start))
	}
	for _, want := range []string{"first: canceled", "interrupted again"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr %q lacks %q", stderr.String(), want)
		}
	}
}
