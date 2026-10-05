//go:build !linux && !darwin && !windows

package filesystem

import (
	"errors"
	"os"
)

func createBelow(*os.File, string) (*os.File, error) { return nil, errors.ErrUnsupported }
func privateTemp(*os.File) error                     { return errors.ErrUnsupported }
func fileOwner(*os.File) (string, error)             { return "", errors.ErrUnsupported }
func syncDirectoryHandle(*os.File) error             { return errors.ErrUnsupported }
func unlinkBelow(*os.File, string, *os.File) error   { return errors.ErrUnsupported }
func publishBelow(*os.File, string, string, bool, *FileIdentity) (bool, error) {
	return false, errors.ErrUnsupported
}
func directoryCollisionKey(_ Identity, s string) string { return s }
