// Package version carries the build metadata injected by the release script.
package version

import (
	"fmt"
	"runtime"
)

// Set with -ldflags -X at build time.
var (
	Version   = "dev"
	Commit    = "none"
	BuildTime = "unknown"
)

// String returns a one line description of the build.
func String() string {
	return fmt.Sprintf("%s (commit %s, built %s, %s/%s, %s)",
		Version, Commit, BuildTime, runtime.GOOS, runtime.GOARCH, runtime.Version())
}
