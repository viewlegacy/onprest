//go:build !windows

package providers

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
)

func openRegular(path string, private bool) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = errors.New("configuration material is not a regular file")
	}
	if err == nil && private {
		var st unix.Stat_t
		err = unix.Fstat(fd, &st)
		if err == nil && (st.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0o077 != 0) {
			err = errors.New("configuration material is not private")
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}
func createPrivate(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}
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
func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
