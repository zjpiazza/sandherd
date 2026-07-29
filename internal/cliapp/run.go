// Package cliapp supplies the behavior shared by command scaffolds.
package cliapp

import (
	"flag"
	"fmt"
	"io"

	"github.com/zjpiazza/sandherd/internal/buildinfo"
)

// Run parses the common command flags and returns a process exit code.
// Feature commands will replace the scaffold message as they are implemented.
func Run(component string, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(component, flag.ContinueOnError)
	flags.SetOutput(stderr)
	showVersion := flags.Bool("version", false, "print version information and exit")
	flags.Usage = func() {
		fmt.Fprintf(stderr, "Usage: %s [--version]\n", component)
	}

	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "%s: unexpected arguments: %v\n", component, flags.Args())
		flags.Usage()
		return 2
	}
	if *showVersion {
		buildinfo.Write(stdout, component)
		return 0
	}

	fmt.Fprintf(stderr, "%s: no feature behavior is implemented yet\n", component)
	flags.Usage()
	return 2
}
