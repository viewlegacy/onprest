//go:build darwin

package filesystem

import (
	"golang.org/x/sys/unix"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
	"os"
)

func renameExclusive(d *os.File, a, b string) error {
	return unix.RenameatxNp(int(d.Fd()), a, int(d.Fd()), b, unix.RENAME_EXCL)
}
func renameExchange(d *os.File, a, b string) error {
	return unix.RenameatxNp(int(d.Fd()), a, int(d.Fd()), b, unix.RENAME_SWAP)
}
func directoryCollisionKey(_ Identity, s string) string {
	return cases.Fold().String(norm.NFD.String(s))
}
