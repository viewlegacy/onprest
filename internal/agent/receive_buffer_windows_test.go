//go:build windows

package agent

import "syscall"

func setReceiveBuffer(fd uintptr, bytes int) error {
	return syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, bytes)
}

// Winsock may accept a large send beyond a nonzero SO_SNDBUF; zero completes it only as it is sent.
const slowSendBuffer = 0
