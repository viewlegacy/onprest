//go:build !linux && !darwin && !windows

package filesystem

import (
	"errors"
	"io/fs"
	"os"
)

func hostOpen(*os.File, string, int, fs.FileMode) (*os.File, error) {
	return nil, errors.ErrUnsupported
}
func hostMkdir(*os.File, string, fs.FileMode) error                 { return errors.ErrUnsupported }
func hostRemove(*os.File, string, *os.File, bool) error             { return errors.ErrUnsupported }
func hostRename(*os.File, string, *os.File, string, *os.File) error { return errors.ErrUnsupported }

func OpenOrdinary(p string, flags int, perm fs.FileMode) (*os.File, error) {
	return nil, errors.ErrUnsupported
}

func hostOrdinaryChild(*os.File, string) (*os.File, error) { return nil, errors.ErrUnsupported }

func ordinaryInspectionError(e error) error { return errors.ErrUnsupported }

func hostEntryOrdinary(*os.File, string) error { return errors.ErrUnsupported }
