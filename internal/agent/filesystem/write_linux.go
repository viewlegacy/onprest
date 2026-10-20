//go:build linux

package filesystem

import (
	"golang.org/x/sys/unix"
	"os"
)

func renameExclusive(d *os.File, a, b string) error {
	return unix.Renameat2(int(d.Fd()), a, int(d.Fd()), b, unix.RENAME_NOREPLACE)
}
func renameExchange(d *os.File, a, b string) error {
	return unix.Renameat2(int(d.Fd()), a, int(d.Fd()), b, unix.RENAME_EXCHANGE)
}
func directoryCollisionKey(_ Identity, s string) string { return s }
