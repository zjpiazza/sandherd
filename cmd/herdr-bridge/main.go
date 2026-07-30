// Command herdr-bridge connects Herdr to the Sandherd API.
package main

import (
	"os"

	"github.com/zjpiazza/sandherd/internal/herdrbridge"
)

func main() {
	os.Exit(herdrbridge.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
