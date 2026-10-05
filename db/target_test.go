package db

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rohanthewiz/dbc/config"
)

// ConsoleTarget keys a console by where a connection lands, not by its
// name: host and port plus database for the servers, the absolute file for
// the embedded engines, and the name only when the DSN cannot be read.
func TestConsoleTarget(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		cc   config.Connection
		want Target
	}{
		{"postgres url", config.Connection{Name: "p", Driver: "postgres",
			DSN: "postgres://u:pw@db.example.com:5433/app?sslmode=disable"},
			Target{"db.example.com:5433", "app"}},
		{"postgres keywords, default port", config.Connection{Name: "p", Driver: "pg",
			DSN: "host=localhost user=u dbname=shop sslmode=disable"},
			Target{"localhost:5432", "shop"}},
		{"postgres derived onto another database", config.Connection{Name: "p/analytics", Driver: "postgres",
			DSN: "postgres://u@db.example.com/app?sslmode=disable", Base: "p", Database: "analytics"},
			Target{"db.example.com:5432", "analytics"}},
		{"mysql", config.Connection{Name: "m", Driver: "mysql",
			DSN: "u:pw@tcp(127.0.0.1:3306)/shop?parseTime=true"},
			Target{"127.0.0.1:3306", "shop"}},
		{"mysql derived", config.Connection{Name: "m/logs", Driver: "mariadb",
			DSN: "u:pw@tcp(127.0.0.1:3306)/", Base: "m", Database: "logs"},
			Target{"127.0.0.1:3306", "logs"}},
		{"sqlite relative file", config.Connection{Name: "s", Driver: "sqlite",
			DSN: "file:scratch.db?_pragma=foreign_keys(1)"},
			Target{"local", filepath.Join(wd, "scratch.db")}},
		{"sqlite in memory", config.Connection{Name: "mem", Driver: "sqlite",
			DSN: "file:x?mode=memory&cache=shared"},
			Target{"memory", "mem"}},
		{"bytdb", config.Connection{Name: "b", Driver: "bytdb", DSN: "/data/notes.bytdb"},
			Target{"local", "/data/notes.bytdb"}},
		{"unreadable dsn", config.Connection{Name: "odd", Driver: "mysql", DSN: "not a dsn"},
			Target{"", "odd"}},
		{"unknown driver", config.Connection{Name: "x", Driver: "oracle", DSN: "whatever"},
			Target{"", "x"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ConsoleTarget(c.cc); got != c.want {
				t.Errorf("ConsoleTarget = %+v, want %+v", got, c.want)
			}
		})
	}
}

// Two names for one database share a target, so they share a console.
func TestConsoleTargetIgnoresNameAndUser(t *testing.T) {
	a := ConsoleTarget(config.Connection{Name: "prod", Driver: "postgres",
		DSN: "postgres://admin@db.example.com/app"})
	b := ConsoleTarget(config.Connection{Name: "prod-ro", Driver: "postgres",
		DSN: "postgres://reader@db.example.com/app"})
	if a != b {
		t.Errorf("same database, different targets: %+v vs %+v", a, b)
	}
}
