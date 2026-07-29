// Command runner hosts an agent process inside a sandbox.
package main

import (
	"os"

	"github.com/zjpiazza/sandherd/internal/cliapp"
)

func main() {
	os.Exit(cliapp.Run("runner", os.Args[1:], os.Stdout, os.Stderr))
}
