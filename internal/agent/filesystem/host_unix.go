//go:build linux || darwin

package filesystem

import (
	"golang.org/x/sys/unix"
	"io/fs"
	"os"
	"time"
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

// hostChildInfo reads an entry's metadata relative to the held directory
// without opening it: FIFOs, devices, write-only files and dangling links are
// described instead of being opened, and a final link is never followed.
func hostChildInfo(dir *os.File, name string) (fs.FileInfo, error) {
	var st unix.Stat_t
	if e := unix.Fstatat(int(dir.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); e != nil {
		return nil, hostPathError(nativeError(e))
	}
	mode := fs.FileMode(uint32(st.Mode) & 0o777)
	switch uint32(st.Mode) & unix.S_IFMT {
	case unix.S_IFDIR:
		mode |= fs.ModeDir
	case unix.S_IFLNK:
		mode |= fs.ModeSymlink
	case unix.S_IFIFO:
		mode |= fs.ModeNamedPipe
	case unix.S_IFSOCK:
		mode |= fs.ModeSocket
	case unix.S_IFCHR:
		mode |= fs.ModeDevice | fs.ModeCharDevice
	case unix.S_IFBLK:
		mode |= fs.ModeDevice
	}
	if uint32(st.Mode)&unix.S_ISUID != 0 {
		mode |= fs.ModeSetuid
	}
	if uint32(st.Mode)&unix.S_ISGID != 0 {
		mode |= fs.ModeSetgid
	}
	if uint32(st.Mode)&unix.S_ISVTX != 0 {
		mode |= fs.ModeSticky
	}
	return entryInfo{name: name, size: st.Size, mode: mode, modified: time.Unix(st.Mtim.Sec, st.Mtim.Nsec), sys: &st}, nil
}

// SameEntry reports whether an opened file is the entry described by info,
// as returned by HostStat or HostDirectoryEntryInfo.
func SameEntry(info fs.FileInfo, f *os.File) bool {
	st, ok := info.Sys().(*unix.Stat_t)
	if !ok {
		return false
	}
	var actual unix.Stat_t
	if unix.Fstat(int(f.Fd()), &actual) != nil {
		return false
	}
	return st.Dev == actual.Dev && st.Ino == actual.Ino
}
