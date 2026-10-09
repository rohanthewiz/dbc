package pipeline

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/rohanthewiz/dbc/etl"
	"github.com/rohanthewiz/dbc/model"
)

// Host is what a node may ask of the session running it: the connections,
// a statement on one of them, the script output, the results view, and an
// etl end for streaming. *sdb.S satisfies it, so inside a Go node the
// familiar e.S.Query and e.S.Print work as they do in a script. It is an
// interface rather than *sdb.S so this package does not import sdb (which
// imports it) and so a test can run a fragment on a bare *sql.DB.
type Host interface {
	// Ctx is the run's context: canceled when the user stops the run.
	Ctx() context.Context
	// Conns names the configured connections.
	Conns() []string
	// Query runs a statement with parameters on conn and returns the result
	// (capped at max_rows, values rendered); Exec returns rows affected.
	Query(conn, query string, args ...any) (*model.Result, error)
	Exec(conn, stmt string, args ...any) (int64, error)
	// Print writes a line to the script output.
	Print(format string, args ...any)
	// Show pushes a result to the results view.
	Show(r *model.Result)
	// DB is the raw database/sql handle for a connection.
	DB(conn string) (*sql.DB, error)
	// ETLConn resolves a connection name to its streaming end: the pool
	// and its SQL dialect, with DDL tracing wired to the script output.
	ETLConn(name string) (etl.Conn, error)
}

// Env is what a node runs in: the session, the pipeline's parameters and
// the values earlier fragments published, the fragment's batch size, and
// a logger that tags lines with the node. One Env per node, sharing the
// maps, so a node's log lines say where they came from.
type Env struct {
	Ctx context.Context
	S   Host
	// Params are the pipeline's parameters, defaults and overrides
	// resolved; Vars are the values earlier fragments published, keyed
	// "frag.<fragment>.<name>" ("frag.orders.rows"). Both are what ${…}
	// in a node's config was substituted from; a Go node reads them here.
	Params map[string]string
	Vars   map[string]string
	// Batch is the fragment's batch size: how many rows a source yields
	// per Next, and so how many a transform sees per Apply.
	Batch int
	// Pipeline, Fragment and Node name where the node is, for messages.
	Pipeline, Fragment, Node string
	// SourceEngine is the fragment's source engine ("postgres", "mysql",
	// "sqlite", "bytdb"; "" when the rows did not come from a database),
	// set by the source's Open. A sink creating a table on the same engine
	// keeps the source's exact column types with it.
	SourceEngine string
	// Logf writes a line to the run's log, prefixed with the node.
	Logf func(format string, args ...any)

	stop *bool // set by Stop; read by the fragment runner
}

// Stop asks the fragment to stop pulling batches from the source once the
// current batch has gone through — what rows.limit does on reaching its
// count. The sinks still commit what they have: a stop is an early end,
// not a failure.
func (e *Env) Stop() {
	if e != nil && e.stop != nil {
		*e.stop = true
	}
}

// Where names the node for messages: "orders/clean".
func (e *Env) Where() string {
	if e == nil {
		return ""
	}
	return e.Fragment + "/" + e.Node
}

// Source yields a fragment's rows, one Batch per Next. Open runs before
// the first Next; Close after the last, or after a failure (ok false), so
// a database read can commit or roll back its transaction accordingly.
type Source interface {
	Open(e *Env) error
	// Next is the next batch, or nil, nil at the end. A batch may hold
	// fewer rows than e.Batch (the last one; a source that reads a line
	// at a time), never more.
	Next(e *Env) (*Batch, error)
	Close(ok bool) error
}

// Transform reshapes batches. Apply may change the batch in place and
// return it, return a new one (other columns, say), or return nil, nil to
// drop it. Flush is called once after the last Apply, for a transform
// that holds rows back (an aggregation, a de-duplication over the run):
// what it returns goes on through the fragment; nil means nothing more.
type Transform interface {
	Open(e *Env) error
	Apply(e *Env, b *Batch) (*Batch, error)
	Flush(e *Env) (*Batch, error)
	Close() error
}

// Sink loads batches. Open is called with the columns of the first batch
// — lazily, so a sink creating a table sees the columns as the transforms
// before it left them — then Write per batch, then Commit once at the end,
// or Abort on any failure. Nothing a sink loads is visible to others
// before Commit; after Abort it is as if the fragment never ran.
type Sink interface {
	Open(e *Env, cols []Col) error
	Write(e *Env, b *Batch) error
	Commit(e *Env) (Stats, error)
	// Abort is idempotent and a no-op after Commit, so the runner can call
	// it on every sink after a failure without tracking which were done.
	Abort() error
}

// Action is a fragment without rows: a statement, a script, a check. It
// is the only node of its fragment.
type Action interface {
	Run(e *Env) (Stats, error)
}

// Stats is what a sink or an action reports at the end.
type Stats struct {
	Rows    int64 // rows loaded, or rows affected
	Skipped int64 // rows dropped along the way, when the node counts them
	Direct  bool  // the fragment ran as a direct Postgres COPY (see run.go)
	// Shown marks a sink that showed its rows rather than wrote them (a
	// preview): its Rows count toward the fragment's only when no sink
	// wrote any, so a preview beside a load does not double the total.
	Shown bool
	Note  string
	// Vars are values for later fragments: a sink publishes "rows"; an
	// sql.exec publishes "affected". They are read as
	// ${frag.<fragment>.<name>}.
	Vars map[string]string
}

// SourceBase, TransformBase and SinkBase give the optional methods their
// do-nothing versions, for embedding in a plugin that has no use for them.
type SourceBase struct{}

func (SourceBase) Open(*Env) error  { return nil }
func (SourceBase) Close(bool) error { return nil }

type TransformBase struct{}

func (TransformBase) Open(*Env) error            { return nil }
func (TransformBase) Flush(*Env) (*Batch, error) { return nil, nil }
func (TransformBase) Close() error               { return nil }

type SinkBase struct{}

func (SinkBase) Abort() error { return nil }

// Status is where a run, a fragment or a node is in its life.
type Status string

const (
	Queued    Status = "queued"
	Running   Status = "running"
	Succeeded Status = "succeeded"
	Failed    Status = "failed"
	Canceled  Status = "canceled"
	Skipped   Status = "skipped"
)

// NodeStats counts what went through one node.
type NodeStats struct {
	ID      string        `json:"id"`
	Plugin  string        `json:"plugin"`
	In      int64         `json:"in"`      // rows handed to it
	Out     int64         `json:"out"`     // rows it handed on (a sink: rows it committed)
	Batches int64         `json:"batches"` // Apply/Write calls
	Elapsed time.Duration `json:"elapsed"` // time inside the node
	Error   string        `json:"error,omitempty"`
}

// FragmentStats is one fragment's run.
type FragmentStats struct {
	Name    string            `json:"name"`
	Status  Status            `json:"status"`
	Started time.Time         `json:"started"`
	Ended   time.Time         `json:"ended"`
	Rows    int64             `json:"rows"`   // rows the sinks committed, or an action's rows affected
	Direct  bool              `json:"direct"` // ran as a direct Postgres COPY
	Nodes   []NodeStats       `json:"nodes"`
	Vars    map[string]string `json:"vars,omitempty"`
	Error   string            `json:"error,omitempty"`
}

// Elapsed is the fragment's wall time so far, or in all.
func (f *FragmentStats) Elapsed() time.Duration {
	if f.Ended.IsZero() {
		return time.Since(f.Started)
	}
	return f.Ended.Sub(f.Started)
}

// RunStats is a pipeline's run: its fragments, in order, and the outcome.
// It is what a script gets back from s.RunPipeline and what the run
// record (package jobs) holds.
type RunStats struct {
	Pipeline  string          `json:"pipeline"`
	Status    Status          `json:"status"`
	Started   time.Time       `json:"started"`
	Ended     time.Time       `json:"ended"`
	Preview   bool            `json:"preview,omitempty"` // nothing was written (Options.PreviewRows)
	Fragments []FragmentStats `json:"fragments"`
	Error     string          `json:"error,omitempty"`
}

// Rows is the rows every fragment committed, in all.
func (r *RunStats) Rows() int64 {
	var n int64
	for _, f := range r.Fragments {
		n += f.Rows
	}
	return n
}

// String is a one-line summary: "pipeline orders-nightly: 2 fragments,
// 48213 rows in 1.4s (succeeded)".
func (r *RunStats) String() string {
	if r == nil {
		return ""
	}
	d := r.Ended.Sub(r.Started)
	if r.Ended.IsZero() {
		d = time.Since(r.Started)
	}
	what := "pipeline"
	if r.Preview {
		what = "preview of"
	}
	return fmt.Sprintf("%s %s: %s, %d rows in %s (%s)", what, r.Pipeline,
		plural(len(r.Fragments), "fragment"), r.Rows(), d.Round(time.Millisecond), r.Status)
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}
