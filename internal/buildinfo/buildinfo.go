// Package buildinfo reports which build of plugyard is running.
package buildinfo

import "runtime/debug"

// version is set at release build time with
// -ldflags "-X github.com/Amraa1/plugyard/internal/buildinfo.version=v1.2.3".
var version string

// OCPPVersions lists the OCPP versions this build can speak.
var OCPPVersions = []string{"1.6J"}

// Version returns the release version, the module version when installed
// with "go install", or "dev" for a local build.
func Version() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return "dev"
}
