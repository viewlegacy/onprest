package providers

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// The spool and identity directories hold other requests' inputs and outputs
// and the definition key. A folder source or a Wasm filesystem grant that is,
// contains, or lies inside either of them would expose that private state
// through ordinary capability reads and writes, so such a layout is refused.
var errPrivateDirectoryOverlap = errors.New("folder root or code filesystem grant overlaps the agent spool_dir or identity_dir")

func checkPrivateDirectories(roots, private []string) error {
	for _, root := range roots {
		for _, p := range private {
			if directoriesOverlap(root, p) {
				return errPrivateDirectoryOverlap
			}
		}
	}
	return nil
}

func directoriesOverlap(a, b string) bool {
	ra, rb := resolveExistingPath(a), resolveExistingPath(b)
	if pathWithin(ra, rb) || pathWithin(rb, ra) {
		return true
	}
	return sameDirectoryOnPath(ra, rb) || sameDirectoryOnPath(rb, ra)
}

// resolveExistingPath evaluates symbolic links in the longest existing prefix
// and keeps the not-yet-created remainder, so a spool created later is checked
// at the location it will occupy.
func resolveExistingPath(p string) string {
	p = filepath.Clean(p)
	var rest []string
	for {
		if resolved, e := filepath.EvalSymlinks(p); e == nil {
			return filepath.Join(append([]string{resolved}, rest...)...)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return filepath.Join(append([]string{p}, rest...)...)
		}
		rest = append([]string{filepath.Base(p)}, rest...)
		p = parent
	}
}

// pathWithin reports whether child is parent or below it. macOS and Windows
// folders are usually case-insensitive, so names are compared case-folded there.
func pathWithin(parent, child string) bool {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		parent, child = strings.ToLower(parent), strings.ToLower(child)
	}
	rel, e := filepath.Rel(parent, child)
	return e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// sameDirectoryOnPath reports whether an existing ancestor of child (or child
// itself) is the same directory as parent by OS identity, which catches bind
// mounts and other aliases that path comparison cannot see.
func sameDirectoryOnPath(parent, child string) bool {
	target, e := os.Stat(parent)
	if e != nil || !target.IsDir() {
		return false
	}
	for p := child; ; {
		if info, e := os.Stat(p); e == nil && os.SameFile(info, target) {
			return true
		}
		next := filepath.Dir(p)
		if next == p {
			return false
		}
		p = next
	}
}
