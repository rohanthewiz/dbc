package config

import (
	"fmt"
	"os"
)

// expandEnv expands the environment references in s — ${NAME} and $NAME —
// through value, as os.Expand does, with one difference: "$$" is a literal
// "$". Every ${VAR} the config reads goes through it: a DSN (ExpandDSN), the
// tls_* paths (ExpandTLS) and the *_dir and pg_bin paths (resolvePath), in
// both config.toml and connections.toml.
//
// WHY "$$". os.Expand reads "$$" as a reference to the shell's special
// variable "$" (its PID), which no process environment holds, so it expanded
// to "" with an unset-variable warning — and there was no way to write a "$"
// that a password needs. S3cr$t9 lost "$t9" to a lookup of $t9, S3cr$$t9 lost
// "$$" as well, and authentication failed either way. The form's password
// field writes the DSN with the "$" as typed (db.BuildDSN), so a password
// typed there had the same fate. "$$" is the escape a Makefile and docker
// compose use, and it changes nothing that worked: "$$" only ever expanded
// to "".
//
// The rest of the syntax stays os.Expand's, which db/dsn.go's envRef (the
// form's split of a stored DSN) mirrors:
//
//	${NAME}, $NAME       value(NAME); NAME is letters, digits and _ after a bare $
//	$$                   "$"
//	$1, $!, $?, $#, …    value("1"), …: shell specials no environment holds, so
//	                     they expand to "" with the warning that names $$
//	$ before anything    kept as is ("100$ ", "a$/b"), as is a $ at the end
//	else
//
// "${$}" is the same special name as "$$" to os.Expand, so it is a "$" too.
func expandEnv(s string, value func(name string) string) string {
	return os.Expand(s, func(name string) string {
		if name == "$" {
			return "$"
		}
		return value(name)
	})
}

// unsetWarning is the load-time warning for a reference to a variable the
// environment does not hold; where says what held it ("connection \"pg\":
// DSN"). It names the "$$" escape because an "unset variable" is as often a
// "$" that was meant literally (S3cr$t9) as a misspelled name, and the user
// reading it is the one who can tell which.
func unsetWarning(where, name string) string {
	return fmt.Sprintf("%s references unset env var $%s (expanded to empty; write $$ for a literal $)",
		where, name)
}
