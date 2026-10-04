//go:build !linux && !darwin && !windows

package filesystem

import (
	"errors"
	"os"
)

func platformName(string) error                    { return errors.ErrUnsupported }
func openRoot(string) (*os.File, error)            { return nil, errors.ErrUnsupported }
func openBelow(*os.File, string) (*os.File, error) { return nil, errors.ErrUnsupported }
func inspect(*os.File) (stamp, error)              { return stamp{}, errors.ErrUnsupported }
