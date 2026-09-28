package cli

import (
	"fmt"
	"io"
)

var Version = "dev"

const (
	exitOK    = 0
	exitError = 1
	exitNo    = 2
)

type env struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

type command struct {
	name    string
	summary string
	run     func(e *env, args []string) int
}

func commands() []command {
	return []command{
		{"version", "print the version", runVersion},
	}
}

func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	e := &env{stdin: stdin, stdout: stdout, stderr: stderr}
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		usage(stdout)
		return exitOK
	}
	for _, c := range commands() {
		if c.name == args[0] {
			return c.run(e, args[1:])
		}
	}
	fmt.Fprintf(stderr, "dibs: unknown command %q\n\n", args[0])
	usage(stderr)
	return exitError
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "dibs: share local Docker containers between git worktrees, one holder at a time")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage: dibs <command> [flags] [args]")
	fmt.Fprintln(w)
	for _, c := range commands() {
		fmt.Fprintf(w, "  %-12s %s\n", c.name, c.summary)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Exit codes: 0 yes, 2 no, 1 error.")
}

func runVersion(e *env, args []string) int {
	fmt.Fprintln(e.stdout, Version)
	return exitOK
}
