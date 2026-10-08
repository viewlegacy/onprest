//go:build !linux

package filesystem

import (
	"errors"
	"os"
)

func HostAllocate(_ *os.File, _, _ int64) error { return errors.ErrUnsupported }
