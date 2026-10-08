package filesystem

import (
	"io/fs"
	"path/filepath"
	"runtime"
	"strings"
)

// WASIPath maps the fixed native base to an absolute WASI guest path. Native
// drive and UNC authority remains in the parent bridge, outside guest paths.
func WASIPath(native string) string {
	if runtime.GOOS != "windows" {
		return filepath.ToSlash(native)
	}
	volume := filepath.VolumeName(native)
	if len(volume) == 2 && volume[1] == ':' {
		return "/volumes/" + strings.ToUpper(volume[:1]) + "/" + strings.TrimLeft(filepath.ToSlash(native[len(volume):]), "/")
	}
	return "/unc/" + strings.TrimLeft(filepath.ToSlash(native), "/")
}

func NativeWASIPath(guest string) (string, error) {
	if runtime.GOOS != "windows" {
		return guest, nil
	}
	if strings.HasPrefix(guest, "/volumes/") {
		rest := strings.TrimPrefix(guest, "/volumes/")
		if len(rest) < 1 || rest[0] < 'A' || rest[0] > 'Z' || len(rest) > 1 && rest[1] != '/' {
			return "", fs.ErrInvalid
		}
		return rest[:1] + ":" + "/" + strings.TrimPrefix(rest[1:], "/"), nil
	}
	if strings.HasPrefix(guest, "/unc/") {
		rest := strings.TrimPrefix(guest, "/unc/")
		parts := strings.Split(rest, "/")
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
			return "", fs.ErrInvalid
		}
		return "//" + rest, nil
	}
	return "", fs.ErrInvalid
}
