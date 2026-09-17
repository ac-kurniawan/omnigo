// Package version is the single source of truth for the OmniGo build version.
package version

import "runtime/debug"

// Default is reported when no version can be resolved (a plain `go build` or
// `go run` from a checkout).
const Default = "dev"

// Value is the version reported by the dashboard badge, `/health`, the
// `-version` flag, and the startup log.
//
// Release builds stamp it from the Git tag, which requires a constant
// initializer:
//
//	-ldflags "-X github.com/ac-kurniawan/omnigo/internal/version.Value=v1.2.3"
//
// Binaries installed straight from the module proxy (`go install
// github.com/ac-kurniawan/omnigo@v1.2.3`) carry no ldflags; those fall back to
// the module version recorded in the build info, then to Default.
var Value = Default

func init() { Value = resolve(Value, moduleVersion()) }

// resolve prefers an explicit build stamp, then the module version recorded by
// `go install <module>@v1.2.3`, then Default.
func resolve(stamped, module string) string {
	if stamped != "" && stamped != Default {
		return stamped
	}
	if module != "" && module != "(devel)" {
		return module
	}
	return Default
}

func moduleVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	return bi.Main.Version
}
