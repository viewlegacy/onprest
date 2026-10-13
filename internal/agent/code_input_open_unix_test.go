//go:build linux || darwin

package agent

import (
	"golang.org/x/sys/unix"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func codeUnsafeNativeInputs(t *testing.T, directory string) map[string]string {
	t.Helper()
	fifo := filepath.Join(directory, "input.fifo")
	if e := unix.Mkfifo(fifo, 0600); e != nil {
		t.Fatal(e)
	}
	socketDirectory, e := os.MkdirTemp("/tmp", "code-input-")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.RemoveAll(socketDirectory) })
	socket := filepath.Join(socketDirectory, "input.socket")
	l, e := net.Listen("unix", socket)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { l.Close() })
	if _, e := os.Stat("/dev/null"); e != nil {
		t.Fatal(e)
	}
	return map[string]string{"device": "/dev/null", "fifo": fifo, "socket": socket}
}
