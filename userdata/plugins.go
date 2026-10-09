package userdata

// Plugins: the user's pipeline plugin files in the plugins directory
// (config.Config.PluginsDir, absolute) — Go files, each one kind of node,
// loaded by package script. A plugin file is a .go file edited, saved,
// renamed and trashed exactly as a script is, so the scripts store
// (scripts.go) keeps them: these are its functions under the plugins'
// names, so a call site says which directory it means and a later
// difference has one place to go.
//
//	<plugins dir>/
//	    mask_email.go
//	    .trash/
//	        old_mask.1759769402123.go
//
// The store's errors are the scripts' (ErrScriptExists, ErrBadScriptName);
// a host words them for plugins.

// PluginInfo is one plugin file, as a list shows it: its file name and the
// first sentence of its opening comment.
type PluginInfo = ScriptInfo

// PluginTrashInfo is one trashed plugin file.
type PluginTrashInfo = TrashInfo

// ValidPluginName reports whether name may name a plugin file: a script
// name (letters, digits, . _ - ending in .go).
func ValidPluginName(name string) bool { return ValidScriptName(name) }

// ListPlugins lists the plugin files in dir by name; a missing dir is none.
func ListPlugins(dir string) ([]PluginInfo, error) { return ListScripts(dir) }

// ReadPlugin returns a plugin file's text and revision.
func ReadPlugin(dir, name string) (text, rev string, err error) { return ReadScript(dir, name) }

// SavePlugin writes a plugin file by SaveScript's rules: base "" creates
// and never overwrites; a changed file is a conflict, not an error.
func SavePlugin(dir, name, text, base string) (rev string, conflict bool, err error) {
	return SaveScript(dir, name, text, base)
}

// RenamePlugin renames a plugin file, refusing to replace another.
func RenamePlugin(dir, from, to string) error { return RenameScript(dir, from, to) }

// TrashPlugin moves a plugin file into .trash — where the loader does not
// look, so its plugin leaves the palette — and returns its trash ID.
func TrashPlugin(dir, name string) (id string, err error) { return TrashScript(dir, name) }

// ListPluginTrash lists the trashed plugin files, newest first.
func ListPluginTrash(dir string) []PluginTrashInfo { return ListTrash(dir) }

// RestorePlugin moves trashed plugin file id back, as to (or its old name).
func RestorePlugin(dir, id, to string) (string, error) { return RestoreScript(dir, id, to) }
