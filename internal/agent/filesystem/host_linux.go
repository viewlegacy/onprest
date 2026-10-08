//go:build linux

package filesystem

import (
	"golang.org/x/sys/unix"
	"io/fs"
)

func hostNativeOpen(fd int, p string, f int, perm fs.FileMode) (int, error) {
	return unix.Openat2(fd, p, &unix.OpenHow{Flags: uint64(f | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK), Mode: func() uint64 {
		if f&unix.O_CREAT != 0 {
			return uint64(perm)
		}
		return 0
	}(), Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV})
}
