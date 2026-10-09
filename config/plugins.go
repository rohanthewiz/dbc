package config

import "strings"

// Where user plugins live: plugins_dir, resolved exactly as jobs_dir is
// (jobs.go) — absent, beside the scripts in ~/.config/dbc; relative,
// against the config file's directory. Each .go file there is one kind of
// pipeline node (package script's loader reads them); unlike scripts,
// pipelines and jobs, a plugin is not found by name on the command line —
// it is placed in a pipeline by its plugin name, from the registry.

// DefaultPluginsDir is ~/.config/dbc/plugins.
func DefaultPluginsDir() string { return defaultDir("plugins") }

// ResolvePluginsDir turns plugins_dir as written into the directory dbc
// loads plugins from.
func ResolvePluginsDir(raw, base string) (string, []string) {
	if strings.TrimSpace(raw) == "" {
		return absOr(DefaultPluginsDir()), nil
	}
	return resolvePath("plugins_dir", raw, base)
}
