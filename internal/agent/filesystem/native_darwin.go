//go:build darwin

package filesystem

import (
	"golang.org/x/sys/unix"
	"os"
)

func nativeOpen(fd int, p string) (int, error) {
	return unix.Openat(fd, p, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW_ANY|unix.O_NONBLOCK, 0)
}
func changeTime(s *unix.Stat_t) (int64, int64) { return s.Ctim.Sec, s.Ctim.Nsec }
func filesystemIdentity(f *os.File, i *identity) error {
	var s unix.Statfs_t
	if err := unix.Fstatfs(int(f.Fd()), &s); err != nil {
		return err
	}
	i.extra = uint64(uint32(s.Fsid.Val[0]))<<32 | uint64(uint32(s.Fsid.Val[1]))
	// Include the mount's attachment, rather than treating device identity
	// alone as proof that two descriptors did not cross a mount boundary.
	i.mount = unix.ByteSliceToString(s.Mntonname[:])
	return nil
}
