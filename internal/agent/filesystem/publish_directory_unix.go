//go:build linux || darwin

package filesystem

import (
	"golang.org/x/sys/unix"
	"os"
)

func renameDirectoryExclusive(d *os.File, a, b string) error { return renameExclusive(d, a, b) }

func createPrivateDirectory(d *os.File, name string) (*os.File, error) {
	if e := unix.Mkdirat(int(d.Fd()), name, 0700); e != nil {
		return nil, e
	}
	return openBelow(d, name)
}
