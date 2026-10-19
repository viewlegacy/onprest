//go:build !windows

package agent

import "syscall"

func setReceiveBuffer(fd uintptr, bytes int) error {
	return syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, bytes)
}

const slowSendBuffer = 1024
