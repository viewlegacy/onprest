//go:build linux || darwin

package agent

import (
	"golang.org/x/sys/unix"
	"os"
)

func codeOpenInputNative(path string) (*os.File, error) {
	fd, e := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
	if e != nil {
		return nil, e
	}
	return os.NewFile(uintptr(fd), path), nil
}
