package filesystem

import (
	"io/fs"
	"os"
)

// InspectOrdinaryMutation classifies the final pathname object without opening
// devices or streams. Ordinary unlink/rename act on a final symlink itself,
// including a dangling link. This guard adds no atomic pathname-race guarantee.
func InspectOrdinaryMutation(p string) error {
	info, e := os.Lstat(p)
	if e != nil {
		return ordinaryInspectionError(e)
	}
	if !info.Mode().IsRegular() && !info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
		return fs.ErrPermission
	}
	return nil
}

// Directory inspection follows ordinary directory symlinks, then verifies a
// nonblocking held descriptor. A raced FIFO cannot block the host process.
func InspectOrdinaryDirectory(p string) error {
	info, e := os.Stat(p)
	if e != nil {
		return ordinaryInspectionError(e)
	}
	if !info.IsDir() {
		if !info.Mode().IsRegular() {
			return fs.ErrPermission
		}
		return fs.ErrExist
	}
	f, e := OpenOrdinary(p, os.O_RDONLY, 0)
	if e != nil {
		return e
	}
	return ordinaryInspectionError(inspectDirectoryFile(f))
}

func (r *Root) HostInspectMutation(p string) error {
	if e := r.inspectHostType(p); e != nil {
		return e
	}
	f, e := r.HostOpen(p, os.O_RDONLY, 0)
	if e != nil {
		return e
	}
	return ordinaryInspectionError(f.Close())
}
func (r *Root) HostInspectDirectory(p string) error {
	if e := r.inspectHostType(p); e != nil {
		return e
	}
	f, e := r.HostOpen(p, os.O_RDONLY, 0)
	if e != nil {
		return e
	}
	return ordinaryInspectionError(inspectDirectoryFile(f))
}

type directoryInspection interface {
	Stat() (fs.FileInfo, error)
	Close() error
}

func inspectDirectoryFile(f directoryInspection) error {
	info, e := f.Stat()
	closed := f.Close()
	if e != nil {
		return e
	}
	if closed != nil {
		return closed
	}
	if !info.IsDir() {
		if !info.Mode().IsRegular() {
			return fs.ErrPermission
		}
		return fs.ErrExist
	}
	return nil
}

// Classify the final native entry relative to a no-follow held parent before
// opening it. In particular an AF_UNIX socket need not be opened as a file.
// The subsequent HostOpen retains the root/filesystem/link-count checks.
func (r *Root) inspectHostType(p string) error {
	if p == "." {
		return nil
	}
	parent, name, e := r.hostParent(p)
	if e != nil {
		return e
	}
	e = hostEntryOrdinary(parent, name)
	closed := parent.Close()
	if e != nil {
		return e
	}
	return ordinaryInspectionError(closed)
}
