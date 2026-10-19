//go:build linux || darwin

package filesystem

import (
	"errors"
	"golang.org/x/sys/unix"
	"io/fs"
	"os"
	"strconv"
)

func createBelow(dir *os.File, name string) (*os.File, error) {
	fd, e := unix.Openat(int(dir.Fd()), name, unix.O_CREAT|unix.O_EXCL|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
	if e != nil {
		return nil, nativeError(e)
	}
	return os.NewFile(uintptr(fd), name), nil
}

func pinTemp(dir *os.File, name string, created *os.File) (*os.File, error) {
	pin, e := openBelow(dir, name)
	if e != nil {
		return nil, e
	}
	a, e := inspect(created)
	b, other := inspect(pin)
	if e != nil || other != nil || a.identity != b.identity {
		return nil, errors.Join(e, other, fs.ErrPermission, pin.Close())
	}
	return pin, nil
}
func privateTemp(f *os.File) error {
	var s unix.Stat_t
	if e := unix.Fstat(int(f.Fd()), &s); e != nil {
		return e
	}
	if s.Uid != uint32(os.Geteuid()) || s.Mode&0077 != 0 {
		return fs.ErrPermission
	}
	return nil
}
func fileOwner(f *os.File) (string, error) {
	var s unix.Stat_t
	e := unix.Fstat(int(f.Fd()), &s)
	return strconv.FormatUint(uint64(s.Uid), 10), e
}
func sameBusinessFile(a, b FileIdentity) bool {
	return a.MatchesDisplaced(b)
}
func syncDirectoryHandle(f *os.File) error { return f.Sync() }
func unlinkBelow(dir *os.File, name string, f *os.File) error {
	current, e := openBelow(dir, name)
	if e != nil {
		return e
	}
	a, e := inspect(f)
	b, other := inspect(current)
	closeErr := current.Close()
	if e != nil || other != nil || closeErr != nil {
		return errors.Join(e, other, closeErr)
	}
	if a.identity != b.identity {
		return fs.ErrPermission
	}
	return unix.Unlinkat(int(dir.Fd()), name, 0)
}

// Set by package tests to exercise an independent writer at the exchange gap.
var exchangeBarrier func()

func publishBelow(dir *os.File, temp, name string, overwrite bool, expected *FileIdentity) (bool, error) {
	if !overwrite || expected == nil {
		e := renameExclusive(dir, temp, name)
		if errors.Is(e, fs.ErrExist) {
			return false, failure("CONFLICT", e)
		}
		return e == nil, e
	}
	existing, e := openBelow(dir, name)
	if e != nil {
		return false, changed(e)
	}
	v, e := inspectRegular(existing, false)
	e = errors.Join(e, existing.Close())
	if e != nil {
		return false, changed(e)
	}
	if v != *expected {
		return false, changed(fs.ErrPermission)
	}
	if exchangeBarrier != nil {
		exchangeBarrier()
	}
	if e = renameExchange(dir, temp, name); e != nil {
		return false, e
	}
	displaced, e := openBelow(dir, temp)
	if e != nil {
		return true, changed(e)
	}
	v, e = inspectRegular(displaced, false)
	e = errors.Join(e, displaced.Close())
	if e != nil || !sameBusinessFile(v, *expected) {
		return true, changed(errors.Join(e, fs.ErrPermission))
	}
	return true, nil
}

// ReplaceSupported reports whether an existing destination can be replaced
// atomically (Linux RENAME_EXCHANGE, macOS RENAME_SWAP).
func ReplaceSupported() bool { return true }
