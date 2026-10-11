package jobs

import (
	"errors"
	"os"
	"sync"
)

// Stopping a run another process is running (N-183). A run belongs to the
// engine that runs it; another process — dbc web's Runs view, the TUI's
// run monitor, `dbc run cancel` — has no hold on it. When the record says
// the process is one whose interrupt stops exactly its runs (Run.PID, set
// by a Signalable engine: a headless `dbc job run` or `dbc pipeline run`)
// and on this machine (Run.Host), an interrupt is sent to it: its own
// Ctrl+C handling cancels the run, the fragments in flight roll back, and
// the record it goes on writing ends canceled — as if stopped there.
//
//	Cancel(id) ─► not live here ─► record running (heartbeat fresh)
//	                                 │
//	                                 ├─ PID, this host ─► SIGINT ─► its process cancels, exits 130
//	                                 └─ else           ─► ErrElsewhere: "stop it there"
//
// A record only names its process while that process writes it: a
// heartbeat gone stale makes the record read interrupted before any
// signal is sent, so a PID the system has since given to another process
// is not signalled for a run long dead. Windows has no SIGINT to send:
// there the run is refused as before.

// errNoSignal is a record that names no process this one can interrupt.
var errNoSignal = errors.New("the run's process cannot be interrupted from here")

// stampProc names this process in a run's record when its interrupt
// stops the run (Options.Signalable).
func (e *Engine) stampProc(r *Run) {
	if e.opt.Signalable {
		r.PID, r.Host = os.Getpid(), hostname()
	}
}

// signalRun interrupts the process running r, when r names one on this
// machine other than this process; errNoSignal when it names none, or the
// signal's own error (Windows: not supported).
func signalRun(r Run) error {
	if r.PID <= 0 || r.PID == os.Getpid() || r.Host == "" || r.Host != hostname() {
		return errNoSignal
	}
	p, err := os.FindProcess(r.PID)
	if err != nil {
		return err
	}
	return p.Signal(os.Interrupt)
}

// hostname is this machine's name, read once; "" when the system will not
// say, which no record matches.
var hostname = sync.OnceValue(func() string {
	h, _ := os.Hostname()
	return h
})
