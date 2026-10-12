package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/rohanthewiz/logger"
	"github.com/rohanthewiz/serr"
	"github.com/urfave/cli/v3"

	"github.com/rohanthewiz/dbc/tutorial"
)

// The tutorial command: docs/scripting-and-etl-tutorial.md as something you
// do rather than read. The lessons, their files and their checks are in
// package tutorial; this is the command line around them.
//
//	dbc tutorial                         the lessons (with ✓ ✗ · in a workspace)
//	dbc tutorial init [DIR] [--force]    make a workspace (default ./dbc-tutorial)
//	dbc tutorial step N [--run] [--files] [--force] [--dir D]
//	                                     show lesson N; --run writes its files and runs its commands
//	dbc tutorial check [N] [--dir D]     check lesson N, or all of them; exit 1 if one fails
//
// Every lesson runs this same binary as a child process in the workspace
// (tutorial.ExecRunner), so the workspace's ./dbc.toml is the config and a
// lesson does exactly what the learner would type there. Nothing here
// opens a database itself.

var (
	flagTutDir   string
	flagTutRun   bool
	flagTutFiles bool
	flagTutForce bool
)

// defaultTutorialDir is where init makes the workspace when no DIR is given.
const defaultTutorialDir = "dbc-tutorial"

func tutorialCommand() *cli.Command {
	dir := &cli.StringFlag{Name: "dir", Value: ".", Usage: "the tutorial workspace `DIR`", Destination: &flagTutDir}
	return &cli.Command{
		Name:   "tutorial",
		Usage:  "the scripting and ETL tutorial, interactively (dbc tutorial init to start)",
		Flags:  []cli.Flag{dir},
		Action: tutorialListAction,
		Commands: []*cli.Command{
			{
				Name:      "init",
				Usage:     "make a tutorial workspace: dbc.toml, seed.sql and its directories (default ./" + defaultTutorialDir + ")",
				ArgsUsage: "[DIR]",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "force", Usage: "replace files already there", Destination: &flagTutForce},
				},
				Action: tutorialInitAction,
			},
			{
				Name:      "step",
				Usage:     "show lesson N: what it teaches, its files and the commands to type",
				ArgsUsage: "<N>",
				Flags: []cli.Flag{
					dir,
					&cli.BoolFlag{Name: "run", Usage: "write the lesson's files and run its commands, then check it",
						Destination: &flagTutRun},
					&cli.BoolFlag{Name: "files", Usage: "print the lesson's files, to type them in yourself",
						Destination: &flagTutFiles},
					&cli.BoolFlag{Name: "force", Usage: "with --run: run a lesson that is done already, and replace its files",
						Destination: &flagTutForce},
				},
				Action: tutorialStepAction,
			},
			{
				Name:      "check",
				Usage:     "check lesson N (or every lesson) worked; exit 1 if a check fails",
				ArgsUsage: "[N]",
				Flags:     []cli.Flag{dir},
				Action:    tutorialCheckAction,
			},
		},
	}
}

// tutorialListAction lists the lessons. In a workspace each is marked with
// its state, which runs every check (a few dbc runs per lesson).
func tutorialListAction(ctx context.Context, cmd *cli.Command) error {
	refuseQueryFlags("tutorial")
	if cmd.Args().Present() {
		usage("usage: dbc tutorial [init|step|check] (dbc tutorial help)")
	}
	inWorkspace := tutorial.CheckWorkspace(flagTutDir) == nil
	env, stop := tutorialEnv()
	defer stop()
	fmt.Printf("The scripting and ETL tutorial: %s\n\n", tutorial.DocURL)
	tutorial.Status(env, inWorkspace, os.Stdout)
	if inWorkspace {
		fmt.Println("\n  ✓ done   ✗ not yet   · nothing to check\n\nNext: dbc tutorial step N")
	} else {
		fmt.Printf("\nStart with: dbc tutorial init, then cd %s and dbc tutorial step 0\n", defaultTutorialDir)
	}
	return nil
}

func tutorialInitAction(ctx context.Context, cmd *cli.Command) error {
	refuseQueryFlags("tutorial init")
	if cmd.Args().Len() > 1 {
		usage("usage: dbc tutorial init [DIR]")
	}
	dir := defaultTutorialDir
	if cmd.Args().Present() {
		dir = cmd.Args().First()
	}
	// A dbc.toml that isn't the tutorial's is someone's config: init must
	// not mix the tutorial into it, even with --force.
	if err := tutorial.CheckWorkspace(dir); errors.Is(err, tutorial.ErrNotWorkspace) && fileExists(filepath.Join(dir, "dbc.toml")) {
		usage(fmt.Sprintf("%s has a dbc.toml that isn't the tutorial's (no shop and warehouse connections): pick another DIR", dir))
	}
	fmt.Printf("tutorial workspace %s\n", dir)
	if err := tutorial.Init(dir, flagTutForce, os.Stdout); err != nil {
		fail(err, "could not make the tutorial workspace")
	}
	fmt.Printf("\nNext:\n  cd %s\n  dbc tutorial step 0\n", dir)
	return nil
}

func tutorialStepAction(ctx context.Context, cmd *cli.Command) error {
	refuseQueryFlags("tutorial step")
	if cmd.Args().Len() != 1 {
		usage("usage: dbc tutorial step <N> [--run] [--files] [--force] [--dir DIR]")
	}
	l := lessonArg(cmd.Args().First())
	if flagTutForce && !flagTutRun {
		usage("--force goes with --run")
	}
	requireWorkspace()
	if !flagTutRun {
		tutorial.Show(os.Stdout, flagTutDir, l, flagTutFiles)
		return nil
	}
	env, stop := tutorialEnv()
	defer stop()
	if !tutorial.Run(env, l, flagTutForce, os.Stdout, os.Stderr) {
		exitTutorial(env.Ctx, 1)
	}
	return nil
}

func tutorialCheckAction(ctx context.Context, cmd *cli.Command) error {
	refuseQueryFlags("tutorial check")
	if cmd.Args().Len() > 1 {
		usage("usage: dbc tutorial check [N] [--dir DIR]")
	}
	requireWorkspace()
	lessons := tutorial.Lessons()
	if cmd.Args().Present() {
		lessons = []tutorial.Lesson{lessonArg(cmd.Args().First())}
	}
	env, stop := tutorialEnv()
	defer stop()
	ok := true
	for i, l := range lessons {
		if i > 0 {
			fmt.Println()
		}
		// every lesson is checked, not just up to the first failure, so
		// the whole picture prints in one go
		if !tutorial.CheckLesson(env, l, os.Stdout) {
			ok = false
		}
	}
	if !ok {
		exitTutorial(env.Ctx, 1)
	}
	return nil
}

// tutorialEnv is the workspace and a Runner that runs this same binary.
// Its context ends on Ctrl+C; stop releases the signal handler.
func tutorialEnv() (*tutorial.Env, func()) {
	bin, err := os.Executable()
	if err != nil {
		fail(serr.Wrap(err, "op", "find the dbc binary"), "could not run the tutorial")
	}
	ctx, stop := interruptible()
	return &tutorial.Env{Ctx: ctx, Dir: flagTutDir, Dbc: tutorial.ExecRunner(bin, nil)}, stop
}

// requireWorkspace exits 2 when --dir isn't a tutorial workspace, saying
// how to make one.
func requireWorkspace() {
	err := tutorial.CheckWorkspace(flagTutDir)
	if errors.Is(err, tutorial.ErrNotWorkspace) {
		usage(fmt.Sprintf("%s is not a tutorial workspace (%v).\nMake one with `dbc tutorial init`, then cd into it or pass --dir.",
			flagTutDir, serr.StringFromErr(err)))
	}
	if err != nil {
		fail(err, "could not read the tutorial workspace")
	}
}

// lessonArg parses a lesson number, exiting 2 on a bad one. "§6" and "6"
// both work, since the lessons print as §N.
func lessonArg(s string) tutorial.Lesson {
	n, err := strconv.Atoi(trimSection(s))
	if err == nil {
		if l, ok := tutorial.Find(n); ok {
			return l
		}
	}
	ls := tutorial.Lessons()
	usage(fmt.Sprintf("no lesson %q: the lessons are %d to %d (dbc tutorial lists them)", s, ls[0].N, ls[len(ls)-1].N))
	return tutorial.Lesson{}
}

func trimSection(s string) string {
	if len(s) > len("§") && s[:len("§")] == "§" {
		return s[len("§"):]
	}
	return s
}

// exitTutorial exits 130 when the run was interrupted, as the other
// headless commands do, else code.
func exitTutorial(ctx context.Context, code int) {
	if ctx.Err() != nil {
		code = 130
	}
	logger.CloseLog()
	os.Exit(code)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
