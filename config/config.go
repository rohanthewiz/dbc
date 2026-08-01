package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
	"github.com/rohanthewiz/serr"
)

// Connection describes one database host connection.
type Connection struct {
	Name   string `toml:"name"`
	Driver string `toml:"driver"` // postgres | mysql | sqlite
	DSN    string `toml:"dsn"`    // env vars are expanded, e.g. ${PGPASS}
}

// Config is the application configuration.
type Config struct {
	ScriptsDir        string       `toml:"scripts_dir"`
	MaxRows           int          `toml:"max_rows"`
	DefaultConnection string       `toml:"default_connection"`
	Connections       []Connection `toml:"connection"`

	Path string `toml:"-"` // file the config was loaded from ("" if none)
	Demo bool   `toml:"-"` // true when running with the built-in demo connection

	// Warnings are load-time findings worth telling the user about but not
	// worth refusing to start over, e.g. a DSN referencing an unset env var.
	Warnings []string `toml:"-"`
}

// Load reads the config from an explicit path, or searches ./dbc.toml then
// ~/.config/dbc/config.toml. With no config anywhere, it falls back to an
// in-memory SQLite demo connection so the app is usable immediately.
func Load(explicit string) (*Config, error) {
	cfg := &Config{ScriptsDir: "scripts", MaxRows: 1000}

	path := explicit
	if path == "" {
		for _, p := range searchPaths() {
			if _, err := os.Stat(p); err == nil {
				path = p
				break
			}
		}
	}

	if path == "" {
		cfg.Connections = []Connection{
			{Name: "demo", Driver: "sqlite", DSN: "file:dbcdemo?mode=memory&cache=shared"},
		}
		cfg.DefaultConnection = "demo"
		cfg.Demo = true
		return cfg, nil
	}

	if _, err := toml.DecodeFile(path, cfg); err != nil {
		return nil, serr.Wrap(err, "config_path", path)
	}
	cfg.Path = path

	if cfg.MaxRows <= 0 {
		cfg.MaxRows = 1000
	}
	if cfg.ScriptsDir == "" {
		cfg.ScriptsDir = "scripts"
	}
	if len(cfg.Connections) == 0 {
		return nil, serr.New("config has no [[connection]] entries", "config_path", path)
	}
	seen := make(map[string]bool, len(cfg.Connections))
	for i := range cfg.Connections {
		cn := &cfg.Connections[i]
		if seen[cn.Name] {
			return nil, serr.New("duplicate connection name — ConnByName would always pick the first",
				"name", cn.Name, "config_path", path)
		}
		seen[cn.Name] = true
		// expand env vars ourselves rather than via os.ExpandEnv, so a typo'd
		// ${PGPASS} warns by name instead of silently becoming "" and
		// surfacing later as a baffling auth failure
		cn.DSN = os.Expand(cn.DSN, func(key string) string {
			v, ok := os.LookupEnv(key)
			if !ok {
				cfg.Warnings = append(cfg.Warnings, fmt.Sprintf(
					"connection %q: DSN references unset env var $%s (expanded to empty)",
					cn.Name, key))
			}
			return v
		})
	}
	if cfg.DefaultConnection != "" && !seen[cfg.DefaultConnection] {
		return nil, serr.New("default_connection names no configured connection",
			"default_connection", cfg.DefaultConnection, "config_path", path)
	}
	return cfg, nil
}

// ConnByName finds a connection config by name.
func (c *Config) ConnByName(name string) (Connection, bool) {
	for _, cn := range c.Connections {
		if cn.Name == name {
			return cn, true
		}
	}
	return Connection{}, false
}

func searchPaths() []string {
	paths := []string{"dbc.toml"}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".config", "dbc", "config.toml"))
	}
	return paths
}
