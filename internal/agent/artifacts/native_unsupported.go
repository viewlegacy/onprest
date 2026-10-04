//go:build !linux && !darwin && !windows

package artifacts

import (
	"errors"
	"os"
)

func privateDirectory(string) error         { return errors.ErrUnsupported }
func protect(*os.File, bool) error          { return errors.ErrUnsupported }
func protectDirectory(*os.File, bool) error { return errors.ErrUnsupported }
func lockFile(*os.File) error               { return errors.ErrUnsupported }
