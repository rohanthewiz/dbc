package config

import (
	"os"
	"strings"
)

// Where a pipeline's files are: files_dir is the directory a relative path
// in a file node (csv.read, jsonl.read, csv.write, jsonl.write, a plugin's
// through Env.Path) resolves against, in every process that runs one.
// Before it, each process resolved against its own working directory: the
// shell's for `dbc pipeline run`, wherever dbc web was started, $HOME for
// dbc.app's helper. So a pipeline that read orders.csv in a terminal
// failed, or wrote somewhere else, once dbc web or its scheduler ran it.
// With one directory, the same spec means the same files from every host,
// as scripts_dir made a script's name mean one file (scripts.go):
//
//	files_dir in config?
//	 ├─ no ──► the home directory   (dbc.app's runs already resolved there)
//	 └─ yes ─► resolved as scripts_dir is: ${VAR}s expanded, ~ for $HOME,
//	           a relative one joined to the config file's directory
//
// The home directory, not a directory beside the scripts like the other
// *_dir keys: these are the user's own files (an export to open, a download
// to load), not dbc's, and ~/.config is hidden in Finder. A checkout that
// wants its pipelines' paths relative to itself sets files_dir = "." in its
// ./dbc.toml, which then means the checkout from any process that loads it.
//
// The directory is not created here: a write creates whatever directories
// its file needs, and a read of a missing directory should fail naming it.

// DefaultFilesDir is the home directory, or "." (the working directory,
// as before files_dir) when there is none.
func DefaultFilesDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return home
}

// ResolveFilesDir turns files_dir as written into the directory a relative
// file path is joined to, by ResolveScriptsDir's rules.
func ResolveFilesDir(raw, base string) (string, []string) {
	if strings.TrimSpace(raw) == "" {
		return absOr(DefaultFilesDir()), nil
	}
	return resolvePath("files_dir", raw, base)
}
