// Package version reports the implementation version (not the protocol
// version, which is wire.Version).
package version

import (
	"runtime/debug"
	"strings"
)

// Version is set at release time with
// -ldflags "-X github.com/agentwireprotocol/awp/internal/version.Version=0.1.0".
var Version string

// String returns the version: the stamped one for release builds, the
// module version for `go install ...@vX.Y.Z` builds, and "dev" otherwise.
func String() string {
	if Version != "" {
		return Version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return strings.TrimPrefix(bi.Main.Version, "v")
	}
	return "dev"
}
