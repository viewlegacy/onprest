//go:build linux || darwin

package filesystem

import (
	"golang.org/x/sys/unix"
	"io/fs"
	"os"
)

func hostOpen(root *os.File, p string, flags int, perm fs.FileMode) (*os.File, error) {
	fd, e := hostNativeOpen(int(root.Fd()), p, flags, perm)
	if e != nil {
		return nil, hostPathError(nativeError(e))
	}
	return os.NewFile(uintptr(fd), p), nil
}
func hostMkdir(d *os.File, n string, p fs.FileMode) error {
	return unix.Mkdirat(int(d.Fd()), n, uint32(p))
}
func hostRemove(d *os.File, n string, _ *os.File, dir bool) error {
	flag := 0
	if dir {
		flag = unix.AT_REMOVEDIR
	}
	return unix.Unlinkat(int(d.Fd()), n, flag)
}
func hostRename(a *os.File, n string, b *os.File, m string, _ *os.File) error {
	return unix.Renameat(int(a.Fd()), n, int(b.Fd()), m)
}

func OpenOrdinary(p string, flags int, perm fs.FileMode) (*os.File, error) {
	fd, e := unix.Open(p, flags|unix.O_NONBLOCK|unix.O_CLOEXEC, uint32(perm))
	if e != nil {
		return nil, hostPathError(e)
	}
	return os.NewFile(uintptr(fd), p), nil
}

func hostPathError(e error) error {
	if e == unix.ENAMETOOLONG || e == unix.EINVAL {
		return fs.ErrInvalid
	}
	return e
}

func hostOrdinaryChild(directory *os.File, name string) (*os.File, error) {
	fd, e := unix.Openat(int(directory.Fd()), name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if e != nil {
		return nil, hostPathError(e)
	}
	return os.NewFile(uintptr(fd), name), nil
}

func ordinaryInspectionError(e error) error { return hostPathError(e) }

func hostEntryOrdinary(parent *os.File, name string) error {
	var st unix.Stat_t
	if e := unix.Fstatat(int(parent.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); e != nil {
		return hostPathError(nativeError(e))
	}
	kind := uint32(st.Mode) & unix.S_IFMT
	if kind != unix.S_IFREG && kind != unix.S_IFDIR {
		return fs.ErrPermission
	}
	return nil
}
