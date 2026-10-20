//go:build linux

package filesystem

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
)

func HostAllocate(f *os.File, offset, length int64) error {
	err := unix.Fallocate(int(f.Fd()), 0, offset, length)
	if err == unix.ENOSYS || err == unix.EOPNOTSUPP {
		return errors.ErrUnsupported
	}
	return err
}
