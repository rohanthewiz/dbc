package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/x/term"
	"github.com/urfave/cli/v3"

	"github.com/rohanthewiz/dbc/config"
)

// The encrypt subcommand: an encrypted dsn for config.toml or dbc.toml.
//
//	dbc encrypt                          prompt for the DSN (not echoed), print its envelope
//	pbpaste | dbc encrypt                … or read it from stdin
//
// connections.toml encrypts its own DSNs (config/secret.go); a config file
// is the user's and dbc never writes it, so an inline password there stays
// plain unless the user pastes in an envelope this command made. Load opens
// it with the same key, ~/.config/dbc/connections.key, made here on first
// use if no saved connection has made it yet.
//
// The DSN is never an argument: one on the command line lands in the
// shell's history and, while dbc runs, in every other user's ps — the two
// places a password is least wanted. On a terminal it is read without echo,
// so it is not left in the scrollback either.
//
// It reads no config and opens nothing, like `dbc version`: it needs only
// the key, and it should work before a config file exists.
//
// Exit status: 0 printed, 1 the key could not be read or made, 2 bad usage
// (an argument, nothing read, more than one line).

func encryptCommand() *cli.Command {
	return &cli.Command{
		Name:  "encrypt",
		Usage: "print an encrypted DSN (enc:v1:…) to paste into config.toml or dbc.toml",
		Description: "Reads one DSN — typed at the prompt, which does not echo it, or piped to stdin — and " +
			"prints it encrypted with ~/.config/dbc/connections.key (made if there is none), the key that " +
			"already protects connections.toml. Paste the line as a connection's dsn; dbc decrypts it when " +
			"it loads the config. The DSN is not taken as an argument, so it stays out of the shell's history.",
		Action: encryptAction,
	}
}

func encryptAction(ctx context.Context, cmd *cli.Command) error {
	refuseQueryFlags("encrypt")
	if cmd.Args().Present() {
		usage("usage: dbc encrypt — type the DSN at the prompt or pipe it in; " +
			"as an argument it would be left in the shell's history")
	}
	tty := term.IsTerminal(os.Stdin.Fd())
	var (
		dsn string
		err error
	)
	if tty {
		dsn, err = promptDSN()
	} else {
		dsn, err = readDSN(os.Stdin)
	}
	if err != nil {
		usage(err.Error())
	}

	saved := config.SavedFile()
	env, err := config.SealDSN(saved, dsn)
	if err != nil {
		fail(err, "could not encrypt the DSN")
	}
	// The envelope alone on stdout, so `dbc encrypt | pbcopy` copies just
	// it. The how-to goes to stderr, and only when a person is reading.
	fmt.Println(env)
	if term.IsTerminal(os.Stderr.Fd()) {
		fmt.Fprintf(os.Stderr, "paste it as the connection's dsn:  dsn = \"%s\"\n"+
			"it opens only where %s is: keep that key out of anything you share\n",
			env, config.KeyFile(saved))
	}
	return nil
}

// promptDSN reads the DSN from the terminal without echo. The prompt goes
// to stderr, so stdout still carries only the envelope.
func promptDSN() (string, error) {
	fmt.Fprint(os.Stderr, "DSN (not shown): ")
	bs, err := term.ReadPassword(os.Stdin.Fd())
	fmt.Fprintln(os.Stderr) // the Enter the user typed was not echoed either
	if err != nil {
		return "", fmt.Errorf("reading the DSN: %w", err)
	}
	return checkDSN(string(bs))
}

// readDSN reads the DSN piped to stdin: one line, as `echo` or a password
// manager's copy leaves it.
func readDSN(r io.Reader) (string, error) {
	bs, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("reading the DSN from stdin: %w", err)
	}
	return checkDSN(string(bs))
}

// checkDSN trims what surrounds the DSN and refuses input that is not one.
//
// Trimmed because no DSN starts or ends in white space, while a pipe
// nearly always brings a trailing newline — encrypted along with the DSN,
// it would come back out as part of the last value (a database named
// "app\n"). More than one line is refused rather than encrypted whole: it
// is a file piped by mistake, or several DSNs where one is expected.
func checkDSN(s string) (string, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return "", errors.New("no DSN was given: type it at the prompt or pipe it in")
	case strings.ContainsAny(s, "\r\n"):
		return "", errors.New("the input is more than one line: give one DSN per run")
	}
	return s, nil
}
