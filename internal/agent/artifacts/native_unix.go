//go:build linux || darwin

package artifacts

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
)

func privateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Uid != uint32(os.Geteuid()) || st.Mode&0o077 != 0 {
		return errors.New("configuration identity directory is not private")
	}
	return nil
}

func protect(f *os.File, newly bool) error {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0077 != 0 || !info.Mode().IsRegular() || st.Nlink != 1 {
		return errors.New("spool file is not private regular single-link storage")
	}
	return nil
}
func lockFile(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) }
func protectDirectory(f *os.File, newly bool) error {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.IsDir() || st.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0077 != 0 {
		return errors.New("spool directory is not private")
	}
	return nil
}
