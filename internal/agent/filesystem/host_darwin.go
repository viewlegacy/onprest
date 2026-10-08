//go:build darwin

package filesystem

import (
	"golang.org/x/sys/unix"
	"io/fs"
)

func hostNativeOpen(fd int, p string, f int, perm fs.FileMode) (int, error) {
	return unix.Openat(fd, p, f|unix.O_CLOEXEC|unix.O_NOFOLLOW_ANY|unix.O_NONBLOCK, uint32(perm))
}
