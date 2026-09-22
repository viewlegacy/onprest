// Package buildinfo exposes the version embedded in a released binary.
package buildinfo

import "strings"

// Version is replaced by the release build with the repository version.
// Development builds intentionally retain the safe, stable default.
var Version = "dev"

// ReleaseMarker gives packaging checks an unambiguous cross-platform byte
// marker without adding another public CLI field. Release builds replace it
// together with Version.
var ReleaseMarker = "onprest-release-version:dev"

// Current returns the normalized version shown by both binaries and the
// gateway MCP initialize response.
func Current() string {
	if strings.TrimSpace(ReleaseMarker) == "" {
		return "dev"
	}
	if version := strings.TrimSpace(Version); version != "" {
		return version
	}
	return "dev"
}
