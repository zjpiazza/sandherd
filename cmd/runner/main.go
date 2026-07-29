// Command runner hosts an agent process inside a sandbox.
package main

import (
	"os"

	"github.com/zjpiazza/sandherd/internal/runner"
)

func main() {
	os.Exit(runner.Run(os.Args[1:], os.Stdout, os.Stderr))
}
