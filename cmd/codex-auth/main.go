// Command codex-auth coordinates and distributes Codex ChatGPT subscription credentials.
package main

import (
	"os"

	"github.com/zjpiazza/sandherd/internal/codexauth"
)

func main() {
	os.Exit(codexauth.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
