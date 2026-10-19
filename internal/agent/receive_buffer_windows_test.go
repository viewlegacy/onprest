//go:build windows

package agent

import "syscall"

func setReceiveBuffer(fd uintptr, bytes int) error {
	return syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, bytes)
}
