// Command codex-launcher starts Codex and resumes the latest durable session when one exists.
package main

import (
	"fmt"
	"os"
	"syscall"

	"github.com/zjpiazza/sandherd/internal/buildinfo"
	"github.com/zjpiazza/sandherd/internal/codexlauncher"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		buildinfo.Write(os.Stdout, "codex-launcher")
		return
	}
	command, err := codexlauncher.Command("/usr/local/bin/codex", os.Getenv("CODEX_HOME"), os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "codex-launcher: durable session state is unavailable")
		os.Exit(1)
	}
	if err := syscall.Exec(command[0], command, os.Environ()); err != nil {
		fmt.Fprintln(os.Stderr, "codex-launcher: Codex could not be started")
		os.Exit(1)
	}
}
