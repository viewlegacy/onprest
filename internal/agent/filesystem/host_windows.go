//go:build windows

package filesystem

import (
	"errors"
	"fmt"
	"golang.org/x/sys/windows"
	"io/fs"
	"os"
	"strings"
	"unsafe"
)

func hostOpen(root *os.File, p string, flags int, perm fs.FileMode) (*os.File, error) {
	return hostOpenMode(root, p, flags, perm, true)
}
func hostOrdinaryChild(directory *os.File, name string) (*os.File, error) {
	return hostOpenMode(directory, name, os.O_RDONLY, 0, false)
}
func hostOpenMode(root *os.File, p string, flags int, perm fs.FileMode, strict bool) (*os.File, error) {
	if p == "." {
		p = ""
	}
	n, e := windows.NewNTUnicodeString(strings.ReplaceAll(p, "/", `\`))
	if e != nil {
		return nil, hostWindowsError(e)
	}
	attributes := uint32(0)
	openOptions := uint32(windows.FILE_SYNCHRONOUS_IO_NONALERT | windows.FILE_OPEN_FOR_BACKUP_INTENT)
	if strict {
		attributes = windows.OBJ_DONT_REPARSE
		openOptions |= windows.FILE_OPEN_REPARSE_POINT
	}
	oa := windows.OBJECT_ATTRIBUTES{RootDirectory: windows.Handle(root.Fd()), ObjectName: n, Attributes: attributes}
	oa.Length = uint32(unsafe.Sizeof(oa))
	access := uint32(windows.GENERIC_READ | windows.SYNCHRONIZE)
	if flags&(os.O_WRONLY|os.O_RDWR) != 0 {
		access |= windows.GENERIC_WRITE
	}
	disposition := uint32(windows.FILE_OPEN)
	if flags&os.O_CREATE != 0 {
		disposition = windows.FILE_OPEN_IF
		if flags&os.O_EXCL != 0 {
			disposition = windows.FILE_CREATE
		}
	}
	var h windows.Handle
	var ios windows.IO_STATUS_BLOCK
	e = windows.NtCreateFile(&h, access, &oa, &ios, nil, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, disposition, openOptions, 0, 0)
	if e != nil {
		return nil, hostWindowsError(e)
	}
	return os.NewFile(uintptr(h), p), nil
}
func hostMkdir(d *os.File, n string, _ fs.FileMode) error {
	name, e := windows.NewNTUnicodeString(n)
	if e != nil {
		return hostWindowsError(e)
	}
	oa := windows.OBJECT_ATTRIBUTES{RootDirectory: windows.Handle(d.Fd()), ObjectName: name, Attributes: windows.OBJ_DONT_REPARSE}
	oa.Length = uint32(unsafe.Sizeof(oa))
	var h windows.Handle
	var ios windows.IO_STATUS_BLOCK
	e = windows.NtCreateFile(&h, windows.GENERIC_READ|windows.SYNCHRONIZE, &oa, &ios, nil, windows.FILE_ATTRIBUTE_DIRECTORY, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_CREATE, windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	if e == nil {
		windows.CloseHandle(h)
	}
	return hostWindowsError(e)
}
func hostMutation(d *os.File, n string) (windows.Handle, error) {
	name, e := windows.NewNTUnicodeString(n)
	if e != nil {
		return 0, hostWindowsError(e)
	}
	oa := windows.OBJECT_ATTRIBUTES{RootDirectory: windows.Handle(d.Fd()), ObjectName: name, Attributes: windows.OBJ_DONT_REPARSE}
	oa.Length = uint32(unsafe.Sizeof(oa))
	var h windows.Handle
	var ios windows.IO_STATUS_BLOCK
	e = windows.NtCreateFile(&h, windows.DELETE|windows.GENERIC_READ|windows.SYNCHRONIZE, &oa, &ios, nil, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN, windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	return h, hostWindowsError(e)
}
func hostRemove(d *os.File, n string, f *os.File, _ bool) error {
	h, e := hostMutation(d, n)
	if e != nil {
		return hostWindowsError(e)
	}
	g := os.NewFile(uintptr(h), n)
	defer g.Close()
	a, e := inspect(f)
	b, e2 := inspect(g)
	if e != nil {
		return hostWindowsError(e)
	}
	if e2 != nil {
		return hostWindowsError(e2)
	}
	if a.identity != b.identity {
		return fs.ErrPermission
	}
	var ios windows.IO_STATUS_BLOCK
	v := byte(1)
	return hostWindowsError(windows.NtSetInformationFile(h, &ios, &v, 1, windows.FileDispositionInformation))
}
func hostRename(a *os.File, n string, b *os.File, m string, f *os.File) error {
	h, e := hostMutation(a, n)
	if e != nil {
		return hostWindowsError(e)
	}
	g := os.NewFile(uintptr(h), n)
	defer g.Close()
	x, e := inspect(f)
	y, e2 := inspect(g)
	if e != nil {
		return hostWindowsError(e)
	}
	if e2 != nil {
		return hostWindowsError(e2)
	}
	if x.identity != y.identity {
		return fs.ErrPermission
	}
	chars, e := windows.UTF16FromString(m)
	if e != nil {
		return hostWindowsError(e)
	}
	chars = chars[:len(chars)-1]
	type info struct {
		Replace byte
		_       [7]byte
		Root    windows.Handle
		Length  uint32
		Name    uint16
	}
	off := int(unsafe.Offsetof(info{}.Name))
	buf := make([]byte, off+2*len(chars))
	v := (*info)(unsafe.Pointer(&buf[0]))
	v.Replace = 1
	v.Root = windows.Handle(b.Fd())
	v.Length = uint32(2 * len(chars))
	copy(unsafe.Slice((*uint16)(unsafe.Pointer(&buf[off])), len(chars)), chars)
	var ios windows.IO_STATUS_BLOCK
	return hostWindowsError(windows.NtSetInformationFile(h, &ios, &buf[0], uint32(len(buf)), windows.FileRenameInformation))
}

func OpenOrdinary(p string, flags int, perm fs.FileMode) (*os.File, error) {
	return os.OpenFile(p, flags, perm)
}

// Native NT operations must have the same fixed host categories as Go file IO.
// Unknown statuses remain a local I/O diagnostic, never a public OS message.
func hostWindowsError(err error) error {
	if err == nil {
		return nil
	}
	var status windows.NTStatus
	if errors.As(err, &status) {
		switch status {
		case windows.STATUS_OBJECT_NAME_NOT_FOUND, windows.STATUS_OBJECT_PATH_NOT_FOUND, windows.STATUS_NO_SUCH_FILE:
			return fs.ErrNotExist
		case windows.STATUS_OBJECT_NAME_COLLISION:
			return fs.ErrExist
		case windows.STATUS_ACCESS_DENIED, windows.STATUS_PRIVILEGE_NOT_HELD, windows.STATUS_SHARING_VIOLATION, windows.STATUS_REPARSE_POINT_ENCOUNTERED, windows.STATUS_REPARSE_POINT_NOT_RESOLVED:
			return fs.ErrPermission
		case windows.STATUS_INVALID_HANDLE, windows.STATUS_INVALID_PARAMETER, windows.STATUS_INVALID_PARAMETER_MIX, windows.STATUS_OBJECT_NAME_INVALID, windows.STATUS_OBJECT_PATH_SYNTAX_BAD, windows.STATUS_NOT_A_DIRECTORY, windows.STATUS_FILE_IS_A_DIRECTORY:
			return fs.ErrInvalid
		case windows.STATUS_NOT_SUPPORTED, windows.STATUS_INVALID_DEVICE_REQUEST, windows.STATUS_NOT_IMPLEMENTED:
			return errors.ErrUnsupported
		}
		err = status.Errno()
	}
	for _, sentinel := range []error{fs.ErrNotExist, fs.ErrPermission, fs.ErrExist, fs.ErrInvalid} {
		if errors.Is(err, sentinel) {
			return sentinel
		}
	}
	if errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) {
		return fs.ErrPermission
	}
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) || errors.Is(err, windows.ERROR_INVALID_NAME) || errors.Is(err, windows.ERROR_BAD_PATHNAME) || errors.Is(err, windows.ERROR_FILENAME_EXCED_RANGE) || errors.Is(err, windows.ERROR_INVALID_HANDLE) {
		return fs.ErrInvalid
	}
	if errors.Is(err, windows.ERROR_NOT_SUPPORTED) || errors.Is(err, windows.ERROR_CALL_NOT_IMPLEMENTED) || errors.Is(err, windows.ERROR_INVALID_FUNCTION) {
		return errors.ErrUnsupported
	}
	return fmt.Errorf("filesystem operation failed: %w", err)
}

func ordinaryInspectionError(e error) error { return hostWindowsError(e) }

func hostEntryOrdinary(parent *os.File, name string) error {
	f, e := hostOpen(parent, name, os.O_RDONLY, 0)
	if e != nil {
		return e
	}
	stamp, e := inspect(f)
	closed := f.Close()
	if e != nil {
		return e
	}
	if closed != nil {
		return hostWindowsError(closed)
	}
	if !stamp.info.Mode().IsRegular() && !stamp.info.IsDir() {
		return fs.ErrPermission
	}
	return nil
}
