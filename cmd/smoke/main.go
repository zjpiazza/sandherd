// Command smoke verifies an installed Sandherd lifecycle and terminal gateway.
package main

import (
	"os"

	"github.com/zjpiazza/sandherd/internal/smoke"
)

func main() {
	os.Exit(smoke.Run(os.Args[1:], os.Stdout, os.Stderr))
}
