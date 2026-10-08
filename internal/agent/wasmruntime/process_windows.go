//go:build windows

package wasmruntime

import (
	"context"
	"errors"
	"golang.org/x/sys/windows"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
	"unsafe"
)

func attachPipes(cmd *exec.Cmd, in, out *os.File) error {
	for _, f := range []*os.File{in, out} {
		if err := windows.SetHandleInformation(windows.Handle(f.Fd()), windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
			return err
		}
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{AdditionalInheritedHandles: []syscall.Handle{syscall.Handle(in.Fd()), syscall.Handle(out.Fd())}}
	cmd.Args = append(cmd.Args, workerCommand, strconv.FormatUint(uint64(in.Fd()), 10), strconv.FormatUint(uint64(out.Fd()), 10))
	return nil
}
func workerPipes(args []string) (*os.File, *os.File, error) {
	if len(args) != 2 {
		return nil, nil, errors.New("worker requires inherited pipes")
	}
	handles := make([]*os.File, 2)
	valid := false
	defer func() {
		if !valid {
			for _, file := range handles {
				if file != nil {
					file.Close()
				}
			}
		}
	}()
	for i, s := range args {
		n, e := strconv.ParseUint(s, 10, 64)
		if e != nil || n == 0 {
			return nil, nil, errors.New("invalid inherited handle")
		}
		h := windows.Handle(n)
		kind, e := windows.GetFileType(h)
		if e != nil || kind != windows.FILE_TYPE_PIPE {
			return nil, nil, errors.New("worker requires pipe handles")
		}
		handles[i] = os.NewFile(uintptr(n), "worker-pipe")
	}
	valid = true
	return handles[0], handles[1], nil
}
func setCPU(context.Context) error { return nil }
func restrictProcess(cmd *exec.Cmd, memory uint64, budget time.Duration) (func(), error) {
	if budget <= 0 || uint64(uintptr(memory)) != memory {
		return nil, errors.New("invalid Windows worker resource limits")
	}
	job, e := windows.CreateJobObject(nil, nil)
	if e != nil {
		return nil, e
	}
	closeJob := func() { windows.CloseHandle(job) }
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE | windows.JOB_OBJECT_LIMIT_PROCESS_MEMORY | windows.JOB_OBJECT_LIMIT_PROCESS_TIME
	info.ProcessMemoryLimit = uintptr(memory)
	info.BasicLimitInformation.PerProcessUserTimeLimit = int64(budget / 100)
	_, e = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
	if e != nil {
		closeJob()
		return nil, e
	}
	p, e := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if e != nil {
		closeJob()
		return nil, e
	}
	defer windows.CloseHandle(p)
	if e = windows.AssignProcessToJobObject(job, p); e != nil {
		closeJob()
		return nil, e
	}
	return closeJob, nil
}
