package tutorial

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// The three things a learner does with a lesson: read it (Show), have it
// done for them (Run), and see whether it worked (CheckLesson). Plus the
// list of all of them with their state (Status). All write plain text to
// w; the command line decides the exit code from the bool they return.

// Show prints lesson l: what it teaches, its files (✓ there, + to write),
// the commands to type and what Check will look at. withFiles also prints
// each file's content, for typing it in by hand.
func Show(w io.Writer, dir string, l Lesson, withFiles bool) {
	fmt.Fprintf(w, "§%d  %s\n    %s#%s\n\n", l.N, l.Title, DocURL, l.Anchor())
	fmt.Fprintln(w, indent(l.Summary, "  "))

	if len(l.Files) > 0 {
		fmt.Fprintln(w, "\n  Files")
		for _, f := range l.Files {
			mark, note := "+", "not there yet: --run writes it"
			if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(f))); err == nil {
				mark, note = "✓", "there"
			}
			fmt.Fprintf(w, "    %s %-32s %s\n", mark, f, note)
		}
	}
	if len(l.Commands) > 0 {
		fmt.Fprintln(w, "\n  Commands (run them in the workspace)")
		for _, c := range l.Commands {
			fmt.Fprintf(w, "    $ %s\n", CommandLine(c.Args))
			if c.Note != "" {
				fmt.Fprintf(w, "        # %s\n", c.Note)
			}
		}
	}
	if len(l.Checks) > 0 {
		fmt.Fprintln(w, "\n  Checks")
		for _, c := range l.Checks {
			fmt.Fprintf(w, "    · %s\n", c.What)
		}
	}

	if withFiles {
		for _, f := range l.Files {
			bs, err := File(f)
			if err != nil {
				continue // embedded, so only a broken build gets here
			}
			fmt.Fprintf(w, "\n── %s %s\n%s", f, strings.Repeat("─", max(1, 60-len(f))), bs)
		}
	}

	fmt.Fprintln(w, "\n  Next")
	if len(l.Files) > 0 || len(l.Commands) > 0 {
		next(w, fmt.Sprintf("dbc tutorial step %d --run", l.N), "write the files and run the commands for you")
		if len(l.Files) > 0 && !withFiles {
			next(w, fmt.Sprintf("dbc tutorial step %d --files", l.N), "print the files, to type them in yourself")
		}
	}
	if len(l.Checks) > 0 {
		next(w, fmt.Sprintf("dbc tutorial check %d", l.N), "see whether it worked")
	}
	if after, ok := Find(l.N + 1); ok {
		next(w, fmt.Sprintf("dbc tutorial step %d", after.N), fmt.Sprintf("§%d %s", after.N, after.Title))
	}
}

// Run does lesson l for the learner: warns when an earlier lesson isn't
// done, writes the lesson's missing files, runs its commands with their
// output streamed through (each echoed as "$ dbc …" first), then checks
// it. A lesson whose checks already pass is not run again unless force:
// some commands fail a second time on purpose (§0's seed, §4's copy).
// force also replaces files already there. It reports whether the lesson
// ended with every check passing.
func Run(e *Env, l Lesson, force bool, w, errw io.Writer) bool {
	// Earlier lessons first: a warning, not a stop, for someone working
	// in their own order. Only the first unfinished one is named.
	for _, prev := range Lessons() {
		if prev.N >= l.N {
			break
		}
		if failed := firstFailure(e, prev); failed != "" {
			fmt.Fprintf(w, "! §%d %s isn't done yet (%s): dbc tutorial step %d --run\n\n",
				prev.N, prev.Title, failed, prev.N)
			break
		}
	}

	if len(l.Checks) > 0 && !force && firstFailure(e, l) == "" {
		fmt.Fprintf(w, "§%d %s is done already: every check passes. --force runs it again.\n", l.N, l.Title)
		return true
	}

	if len(l.Files) > 0 {
		fmt.Fprintf(w, "§%d %s: files\n", l.N, l.Title)
		for _, f := range l.Files {
			was, err := WriteFile(e.Dir, f, force)
			if err != nil {
				fmt.Fprintf(w, "  ✗ %s: %v\n", f, err)
				return false
			}
			fmt.Fprintln(w, writtenLine(was, f))
		}
	}

	for _, c := range l.Commands {
		if e.Ctx.Err() != nil {
			fmt.Fprintln(w, "\ninterrupted")
			return false
		}
		fmt.Fprintf(w, "\n$ %s\n", CommandLine(c.Args))
		if c.Note != "" {
			fmt.Fprintf(w, "# %s\n", c.Note)
		}
		code, err := e.Dbc(e.Ctx, e.Dir, c.Args, w, errw)
		switch {
		case err != nil:
			fmt.Fprintf(w, "✗ couldn't run dbc: %v\n", err)
			return false
		case c.ExpectFail && code == 0:
			// Not a stop: the state the lesson checks may still be right.
			fmt.Fprintln(w, "! this was meant to fail and didn't: was the table empty?")
		case c.ExpectFail:
			fmt.Fprintf(w, "↳ exit %d, as expected\n", code)
		case code != 0:
			fmt.Fprintf(w, "✗ exit %d: the lesson stops here. Fix what the error names, then run it again.\n", code)
			return false
		}
	}

	if len(l.Checks) == 0 {
		return true
	}
	fmt.Fprintln(w)
	return CheckLesson(e, l, w)
}

// CheckLesson runs l's checks and prints each one, ✓ or ✗ with what was
// found and the hint. A lesson with no checks (the UIs, the pitfalls) says
// so and passes.
func CheckLesson(e *Env, l Lesson, w io.Writer) bool {
	fmt.Fprintf(w, "§%d %s\n", l.N, l.Title)
	if len(l.Checks) == 0 {
		fmt.Fprintln(w, "  · nothing to check: this lesson is reading")
		return true
	}
	ok := true
	for _, c := range l.Checks {
		if err := c.Test(e); err != nil {
			ok = false
			fmt.Fprintf(w, "  ✗ %s\n      found: %v\n      hint:  %s\n", c.What, err, c.Hint)
			continue
		}
		fmt.Fprintf(w, "  ✓ %s\n", c.What)
	}
	return ok
}

// Status lists every lesson. In a workspace each gets a mark: ✓ every
// check passes, ✗ one doesn't, · nothing to check. It reports whether
// every lesson passed.
func Status(e *Env, inWorkspace bool, w io.Writer) bool {
	ok := true
	for _, l := range Lessons() {
		mark := " "
		if inWorkspace {
			switch {
			case len(l.Checks) == 0:
				mark = "·"
			case firstFailure(e, l) == "":
				mark = "✓"
			default:
				mark, ok = "✗", false
			}
		}
		fmt.Fprintf(w, "  %s §%-2d %s\n", mark, l.N, l.Title)
	}
	return ok
}

// firstFailure runs l's checks quietly and returns what the first failing
// one checks, or "" when every one passes.
func firstFailure(e *Env, l Lesson) string {
	for _, c := range l.Checks {
		if c.Test(e) != nil {
			return c.What
		}
	}
	return ""
}

// next prints one "Next" line, the commands in a column.
func next(w io.Writer, cmd, what string) {
	fmt.Fprintf(w, "    %-30s %s\n", cmd, what)
}

// indent prefixes every non-empty line of s.
func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = prefix + l
		}
	}
	return strings.Join(lines, "\n")
}
