//go:build !linux && !darwin && !windows

package agent

import (
	"errors"
	"os"
)

func codeOpenInputNative(string) (*os.File, error) { return nil, errors.ErrUnsupported }
