// Command herdr-bridge connects Herdr to the Sandherd API.
package main

import (
	"os"

	"github.com/zjpiazza/sandherd/internal/cliapp"
)

func main() {
	os.Exit(cliapp.Run("herdr-bridge", os.Args[1:], os.Stdout, os.Stderr))
}
