package tutorial

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/rohanthewiz/serr"
)

// Runner runs dbc with args in dir, its output to stdout and stderr, and
// returns its exit code. err is for a dbc that could not be started at
// all; a dbc that ran and failed is a non-zero code with a nil err.
//
// The command passes ExecRunner; the unit tests pass a fake that answers
// from a table, so lessons and checks are tested without a database.
type Runner func(ctx context.Context, dir string, args []string, stdout, stderr io.Writer) (code int, err error)

// ExecRunner runs the dbc binary at bin as a child process with its
// working directory set to the workspace. That cwd is what makes the
// workspace's ./dbc.toml the config (dbc's resolution: --config, else
// ./dbc.toml, else ~/.config/dbc/config.toml), so the child runs exactly
// what the learner would type there. env nil = this process's environment.
//
// Running in process instead would not work: dbc's headless paths end
// with os.Exit (fail, usage), and the config is found from the cwd.
func ExecRunner(bin string, env []string) Runner {
	return func(ctx context.Context, dir string, args []string, stdout, stderr io.Writer) (int, error) {
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Dir = dir
		cmd.Env = env
		cmd.Stdout, cmd.Stderr = stdout, stderr
		// No stdin: dbc reads piped stdin as SQL, so a child must never
		// inherit a pipe meant for its parent. Nil is /dev/null.
		cmd.Stdin = nil
		// On Ctrl+C the terminal interrupts the child too; ctx's cancel is
		// the fallback for a parent canceled some other way. An interrupt
		// lets dbc roll back what is in flight, as it does for the learner.
		cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
		err := cmd.Run()
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode(), nil
		}
		if err != nil {
			return -1, serr.Wrap(err, "op", "run dbc", "bin", bin, "args", strings.Join(args, " "))
		}
		return 0, nil
	}
}

// CommandLine is args as the learner would type them after "dbc", quoted
// for a POSIX shell where needed.
func CommandLine(args []string) string {
	parts := []string{"dbc"}
	for _, a := range args {
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ")
}

// shellQuote leaves a plain word alone and quotes anything else: double
// quotes when that needs no escaping (it reads better around SQL with
// '…' literals, as the doc writes it), else single quotes.
func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n'\"\\$`*?[]{}()<>|&;#~!") {
		return s
	}
	if !strings.ContainsAny(s, "\"\\$`!") {
		return `"` + s + `"`
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
