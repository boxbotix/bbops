package main

import (
	"flag"
	"fmt"
	"io"
	"os"
)

var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, version, commit))
}

func run(args []string, stdout, stderr io.Writer, ver, sha string) int {
	fs := flag.NewFlagSet("bbops", flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print bbops version and commit")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "unexpected arguments: %v\n", fs.Args())
		return 2
	}
	if *showVersion {
		_, _ = fmt.Fprintf(stdout, "bbops %s (%s)\n", ver, sha)
		return 0
	}
	// shortcut: M0.1 entrypoint exits 0 after flag validation, wire embedded NATS + SemEngine host runtime in M0.2 (#3)
	return 0
}
