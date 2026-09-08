// Package buildinfo exposes the version embedded in a released binary.
package buildinfo

import "strings"

// Version is replaced by the release build with the repository version.
// Development builds intentionally retain the safe, stable default.
var Version = "dev"

// Current returns the normalized version shown by both binaries and the
// gateway MCP initialize response.
func Current() string {
	if version := strings.TrimSpace(Version); version != "" {
		return version
	}
	return "dev"
}
