//go:build linux || darwin

package wasmruntime

import (
	"context"
	"errors"
	"golang.org/x/sys/unix"
	"math"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func attachPipes(cmd *exec.Cmd, in, out *os.File) error {
	cmd.ExtraFiles = []*os.File{in, out}
	cmd.Args = append(cmd.Args, workerCommand)
	return nil
}
func workerPipes(args []string) (*os.File, *os.File, error) {
	if len(args) != 0 {
		return nil, nil, errors.New("invalid private worker arguments")
	}
	in, out := os.NewFile(3, "worker-input"), os.NewFile(4, "worker-output")
	for _, f := range []*os.File{in, out} {
		v, e := f.Stat()
		if e != nil || v.Mode()&os.ModeNamedPipe == 0 {
			in.Close()
			out.Close()
			return nil, nil, errors.New("worker requires inherited pipes")
		}
	}
	return in, out, nil
}
func setCPU(ctx context.Context) error {
	d, ok := ctx.Deadline()
	if !ok {
		return errors.New("worker deadline required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	secs := uint64(math.Ceil(time.Until(d).Seconds()))
	if secs < 1 {
		secs = 1
	}
	return unix.Setrlimit(unix.RLIMIT_CPU, &unix.Rlimit{Cur: secs, Max: secs})
}
func restrictProcess(*exec.Cmd, uint64, time.Duration) (func(), error) { return func() {}, nil }

func cpuLimitKilled(state *os.ProcessState, limit float64) bool {
	if state == nil {
		return false
	}
	status, ok := state.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || (status.Signal() != unix.SIGKILL && status.Signal() != unix.SIGXCPU) {
		return false
	}
	// wait4 rusage can trail the kernel's RLIMIT_CPU clock by a few milliseconds.
	return (state.UserTime() + state.SystemTime()).Seconds() >= limit-0.05
}
