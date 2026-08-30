//go:build !windows

package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

type validationNativeLock struct {
	file *os.File
}

var validationLockOpenedHook = func(string) {}

func acquireValidationLock(path string) (*validationNativeLock, bool, error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, false, err
	}
	file := os.NewFile(uintptr(fd), path)
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = file.Close()
		}
	}()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != 0 || info.Mode().Perm() != 0o600 {
		if err == nil {
			err = fmt.Errorf("validation lock path is not a private empty regular file")
		}
		return nil, false, err
	}
	validationLockOpenedHook(path)
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, true, nil
		}
		return nil, false, err
	}
	closeOnError = false
	return &validationNativeLock{file: file}, false, nil
}

func (l *validationNativeLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	fd := int(l.file.Fd())
	unlockErr := unix.Flock(fd, unix.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

func createPrivateValidationFile(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func validateExistingPrivateFile(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
		return fmt.Errorf("validation detail path is not a private regular file")
	}
	if info.Size() > validationDetailLogMaxBytes {
		return fmt.Errorf("validation detail path exceeds size limit")
	}
	return nil
}

func atomicReplaceValidationFile(source, target string) error {
	if err := os.Rename(source, target); err != nil {
		return err
	}
	if err := syncValidationDirectory(filepath.Dir(target)); err != nil {
		return err
	}
	return validationPostCommitDurabilityHook()
}

func syncValidationDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return err
	}
	return dir.Close()
}
