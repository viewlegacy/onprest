//go:build !linux && !darwin && !windows

package wasmruntime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"time"
)

func attachPipes(*exec.Cmd, *os.File, *os.File) error  { return errors.ErrUnsupported }
func workerPipes([]string) (*os.File, *os.File, error) { return nil, nil, errors.ErrUnsupported }
func setCPU(context.Context) error                     { return errors.ErrUnsupported }
func restrictProcess(*exec.Cmd, uint64, time.Duration) (func(), error) {
	return nil, errors.ErrUnsupported
}
