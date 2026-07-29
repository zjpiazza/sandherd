// Package buildinfo reports metadata injected into Sandherd binaries at build time.
package buildinfo

import (
	"fmt"
	"io"
	"runtime"
)

// These values are overridden with -ldflags for release and container builds.
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

// Info is the build metadata shared by every Sandherd command.
type Info struct {
	Version string
	Commit  string
	Date    string
	Go      string
	OS      string
	Arch    string
}

// Current returns the metadata compiled into the running binary.
func Current() Info {
	return Info{
		Version: Version,
		Commit:  Commit,
		Date:    Date,
		Go:      runtime.Version(),
		OS:      runtime.GOOS,
		Arch:    runtime.GOARCH,
	}
}

// Write prints one stable, human-readable version line.
func Write(w io.Writer, component string) {
	info := Current()
	fmt.Fprintf(
		w,
		"%s %s (commit=%s, built=%s, go=%s, platform=%s/%s)\n",
		component,
		info.Version,
		info.Commit,
		info.Date,
		info.Go,
		info.OS,
		info.Arch,
	)
}
