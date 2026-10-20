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
	buf, e := renameInformation(d, b)
	if e != nil {
		return e
	}
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
