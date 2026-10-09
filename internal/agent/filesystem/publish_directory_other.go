//go:build !linux && !darwin && !windows

package filesystem

import (
	"errors"
	"os"
)

func renameDirectoryExclusive(*os.File, string, string) error { return errors.ErrUnsupported }

func createPrivateDirectory(*os.File, string) (*os.File, error) { return nil, errors.ErrUnsupported }
