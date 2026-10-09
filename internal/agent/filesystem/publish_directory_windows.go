//go:build windows

package filesystem

import (
	"golang.org/x/sys/windows"
	"os"
	"unsafe"
)

func renameDirectoryExclusive(d *os.File, a, b string) error {
	n, e := windows.NewNTUnicodeString(a)
	if e != nil {
		return e
	}
	oa := windows.OBJECT_ATTRIBUTES{RootDirectory: windows.Handle(d.Fd()), ObjectName: n, Attributes: windows.OBJ_DONT_REPARSE}
	oa.Length = uint32(unsafe.Sizeof(oa))
	var h windows.Handle
	var ios windows.IO_STATUS_BLOCK
	e = windows.NtCreateFile(&h, windows.GENERIC_READ|windows.DELETE|windows.SYNCHRONIZE, &oa, &ios, nil, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN, windows.FILE_OPEN_REPARSE_POINT|windows.FILE_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	if e != nil {
		return e
	}
	defer windows.CloseHandle(h)
	chars, e := windows.UTF16FromString(b)
	if e != nil {
		return e
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
	v.Root = windows.Handle(d.Fd())
	v.Length = uint32(2 * len(chars))
	copy(unsafe.Slice((*uint16)(unsafe.Pointer(&buf[off])), len(chars)), chars)
	return windows.NtSetInformationFile(h, &ios, &buf[0], uint32(len(buf)), windows.FileRenameInformation)
}

func createPrivateDirectory(d *os.File, name string) (*os.File, error) {
	user, e := windows.GetCurrentProcessToken().GetTokenUser()
	if e != nil {
		return nil, e
	}
	sd, e := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + user.User.Sid.String() + ")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)")
	if e != nil {
		return nil, e
	}
	n, e := windows.NewNTUnicodeString(name)
	if e != nil {
		return nil, e
	}
	oa := windows.OBJECT_ATTRIBUTES{RootDirectory: windows.Handle(d.Fd()), ObjectName: n, Attributes: windows.OBJ_DONT_REPARSE, SecurityDescriptor: sd}
	oa.Length = uint32(unsafe.Sizeof(oa))
	var h windows.Handle
	var ios windows.IO_STATUS_BLOCK
	e = windows.NtCreateFile(&h, windows.GENERIC_READ|windows.GENERIC_WRITE|windows.DELETE|windows.READ_CONTROL|windows.SYNCHRONIZE, &oa, &ios, nil, windows.FILE_ATTRIBUTE_DIRECTORY, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_CREATE, windows.FILE_OPEN_REPARSE_POINT|windows.FILE_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	if e != nil {
		return nil, e
	}
	return os.NewFile(uintptr(h), name), nil
}
