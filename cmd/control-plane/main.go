// Command control-plane exposes Sandherd's client-neutral API.
package main

import (
	"os"

	"github.com/zjpiazza/sandherd/internal/controlplane"
)

func main() {
	os.Exit(controlplane.Run(os.Args[1:], os.Stdout, os.Stderr))
}
