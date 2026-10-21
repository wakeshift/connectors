// Package version reports the build's version, set at link time:
//
//	-ldflags "-X github.com/wakeshift/connectors/internal/version.Version=1.2.3"
package version

// Version is the build's version; "dev" for local builds.
var Version = "dev"
