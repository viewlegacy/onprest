//go:build linux

package filesystem

import (
	"golang.org/x/sys/unix"
	"os"
)

func nativeOpen(fd int, p string) (int, error) {
	return unix.Openat2(fd, p, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV})
}
func changeTime(s *unix.Stat_t) (int64, int64)     { return s.Ctim.Sec, s.Ctim.Nsec }
func filesystemIdentity(*os.File, *identity) error { return nil }
