//go:build linux || darwin

package filesystem

import (
	"golang.org/x/sys/unix"
	"io/fs"
	"os"
)

func platformName(string) error { return nil }
func openRoot(p string) (*os.File, error) {
	fd, err := unix.Open(p, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, nativeError(err)
	}
	return os.NewFile(uintptr(fd), p), nil
}
func openBelow(root *os.File, p string) (*os.File, error) {
	fd, err := nativeOpen(int(root.Fd()), p)
	if err != nil {
		return nil, nativeError(err)
	}
	return os.NewFile(uintptr(fd), p), nil
}
func nativeError(err error) error {
	if err == unix.ELOOP || err == unix.EXDEV || err == unix.ENOTDIR {
		return fs.ErrPermission
	}
	return err
}
func inspect(f *os.File) (stamp, error) {
	info, err := f.Stat()
	if err != nil {
		return stamp{}, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return stamp{}, err
	}
	var s stamp
	s.info = info
	s.identity = identity{device: uint64(st.Dev), inode: uint64(st.Ino)}
	s.links = uint64(st.Nlink)
	s.changeSeconds, s.changeNanos = changeTime(&st)
	if err := filesystemIdentity(f, &s.identity); err != nil {
		return stamp{}, err
	}
	return s, nil
}
