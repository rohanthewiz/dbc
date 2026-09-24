package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"

	"github.com/rohanthewiz/dbc/config"
)

// faultDriver is a database/sql driver whose connections fail on command, so
// Classify can be tested against each way a connection goes wrong without a
// server to kill. Statements:
//
//	CUT     the connection dies mid-statement: a network error, not
//	        ErrBadConn (the statement was sent), and IsValid goes false
//	GONE    the driver knew before sending: driver.ErrBadConn
//	FAIL    an ordinary SQL error on a healthy connection
//	other   succeed
type faultDriver struct{}

type faultConn struct{ dead bool }

func (faultDriver) Open(string) (driver.Conn, error) { return &faultConn{}, nil }

func (c *faultConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("no prepare") }
func (c *faultConn) Close() error                        { return nil }
func (c *faultConn) Begin() (driver.Tx, error)           { return nil, errors.New("no tx") }
func (c *faultConn) IsValid() bool                       { return !c.dead }

func (c *faultConn) ExecContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Result, error) {
	switch q {
	case "CUT":
		c.dead = true
		return nil, errors.New("read tcp 127.0.0.1:5432: connection reset by peer")
	case "GONE":
		c.dead = true
		return nil, driver.ErrBadConn
	case "FAIL":
		return nil, errors.New(`syntax error at or near "FAIL"`)
	}
	return driver.RowsAffected(0), nil
}

func init() { sql.Register("dbc-fault", faultDriver{}) }

// faultSession pins a connection of the fault driver in a Session, the way
// Manager.Session would.
func faultSession(t *testing.T) *Session {
	t.Helper()
	dbh, err := sql.Open("dbc-fault", "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = dbh.Close() })
	c, err := dbh.Conn(context.Background())
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	mgr := NewManager(&config.Config{MaxRows: 1000})
	s := &Session{m: mgr, name: "fault", conn: c}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSessionClassify(t *testing.T) {
	cases := []struct {
		name  string
		setup []string // run first; all succeed
		fail  string
		want  Fault
	}{
		{"sql error, live connection", nil, "FAIL", FaultNone},
		{"sql error, stateful but live", []string{"SET x = 1"}, "FAIL", FaultNone},
		// the bug N-034 was: a cut mid-statement is not ErrBadConn, so it
		// used to be reported as an ordinary error and the dead session kept
		{"cut mid-statement, stateless", nil, "CUT", FaultDrop},
		{"cut mid-statement, stateful", []string{"SET x = 1"}, "CUT", FaultLost},
		{"bad conn, stateless", nil, "GONE", FaultRetry},
		{"bad conn, stateful", []string{"BEGIN"}, "GONE", FaultLost},
	}
	ctx := context.Background()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := faultSession(t)
			for _, stmt := range c.setup {
				if _, err := s.Run(ctx, stmt); err != nil {
					t.Fatalf("%s: %v", stmt, err)
				}
			}
			_, err := s.Run(ctx, c.fail)
			if err == nil {
				t.Fatalf("%s succeeded", c.fail)
			}
			if got := s.Classify(err); got != c.want {
				t.Errorf("Classify = %d, want %d (err: %v)", got, c.want, err)
			}
		})
	}
	if got := faultSession(t).Classify(nil); got != FaultNone {
		t.Errorf("Classify(nil) = %d, want FaultNone", got)
	}
}
