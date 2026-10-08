package filesystem

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// observedCreate records creation only when an atomic exclusive native open
// succeeds. An existing-object open never includes O_CREATE, so a race cannot
// silently create an object without that observation. Retries/link traversal
// are bounded; the returned descriptor, rather than a path stat, owns the result.
func observedCreate(p string, flags int, open func(string, int) (*os.File, error), followLinks bool) (*os.File, bool, error) {
	if flags&os.O_CREATE == 0 {
		f, e := open(p, flags)
		return f, false, e
	}
	if flags&os.O_EXCL != 0 {
		f, e := open(p, flags)
		return f, e == nil, e
	}
	for attempts := 0; attempts < 64; attempts++ {
		f, e := open(p, flags|os.O_EXCL)
		if e == nil {
			return f, true, nil
		}
		if !errors.Is(e, fs.ErrExist) {
			return nil, false, e
		}
		f, e = open(p, flags&^os.O_CREATE)
		if e == nil {
			return f, false, nil
		}
		if !errors.Is(e, fs.ErrNotExist) {
			return nil, false, e
		}
		if followLinks {
			// Existing symlinks are valid under ordinary OS-user semantics, including
			// a dangling final link. Resolve its target only to attempt another atomic
			// open; reading this name never establishes whether creation occurred.
			target, err := os.Readlink(p)
			if err == nil {
				if !filepath.IsAbs(target) {
					target = filepath.Join(filepath.Dir(p), target)
				}
				p = target
			} else if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, fs.ErrInvalid) {
				// EINVAL means a concurrent replacement is no longer a symlink; retry.
				if !errors.Is(err, syscall.EINVAL) {
					return nil, false, err
				}
			}
		}
	}
	return nil, false, errors.New("filesystem open race or link traversal limit")
}

// OpenOrdinaryObserved preserves ordinary OS-user flags and link semantics,
// while exposing the result of an atomic create to the internal host bridge.
func OpenOrdinaryObserved(p string, flags int, perm fs.FileMode) (*os.File, bool, error) {
	return observedCreate(p, flags, func(n string, f int) (*os.File, error) { return OpenOrdinary(n, f, perm) }, true)
}
