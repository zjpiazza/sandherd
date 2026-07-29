// Command control-plane exposes Sandherd's client-neutral API.
package main

import (
	"os"

	"github.com/zjpiazza/sandherd/internal/cliapp"
)

func main() {
	os.Exit(cliapp.Run("control-plane", os.Args[1:], os.Stdout, os.Stderr))
}
