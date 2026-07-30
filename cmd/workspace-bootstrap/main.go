// Command workspace-bootstrap safely prepares a durable agent workspace.
package main

import (
	"os"

	"github.com/zjpiazza/sandherd/internal/bootstrap"
)

func main() {
	os.Exit(bootstrap.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
