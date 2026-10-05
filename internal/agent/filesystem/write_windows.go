//go:build windows

package filesystem

import (
	"errors"
	"golang.org/x/sys/windows"
	"io/fs"
	"os"
	"strings"
	"unsafe"
)

func directoryCollisionKey(_ Identity, s string) string { return strings.ToLower(s) }
func fileOwner(f *os.File) (string, error) {
	sd, e := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if e != nil {
		return "", e
	}
	owner, _, e := sd.Owner()
	if e != nil {
		return "", e
	}
	return owner.String(), nil
}
func createBelow(dir *os.File, name string) (*os.File, error) {
	user, e := windows.GetCurrentProcessToken().GetTokenUser()
	if e != nil {
		return nil, e
	}
	sd, e := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + user.User.Sid.String() + ")(A;;FA;;;SY)(A;;FA;;;BA)")
	if e != nil {
		return nil, e
	}
	n, e := windows.NewNTUnicodeString(name)
	if e != nil {
		return nil, e
	}
	oa := windows.OBJECT_ATTRIBUTES{RootDirectory: windows.Handle(dir.Fd()), ObjectName: n, Attributes: windows.OBJ_DONT_REPARSE, SecurityDescriptor: sd}
	oa.Length = uint32(unsafe.Sizeof(oa))
	var h windows.Handle
	var ios windows.IO_STATUS_BLOCK
	e = windows.NtCreateFile(&h, windows.GENERIC_READ|windows.GENERIC_WRITE|windows.DELETE|windows.READ_CONTROL|windows.SYNCHRONIZE, &oa, &ios, nil, windows.FILE_ATTRIBUTE_NORMAL, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_CREATE, windows.FILE_OPEN_REPARSE_POINT|windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	if e != nil {
		return nil, e
	}
	return os.NewFile(uintptr(h), name), nil
}
func privateTemp(f *os.File) error {
	h := windows.Handle(f.Fd())
	sd, e := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION)
	if e != nil {
		return e
	}
	u, e := windows.GetCurrentProcessToken().GetTokenUser()
	if e != nil {
		return e
	}
	owner, _, e := sd.Owner()
	if e != nil || owner == nil || !owner.Equals(u.User.Sid) {
		return fs.ErrPermission
	}
	acl, _, e := sd.DACL()
	if e != nil || acl == nil {
		return fs.ErrPermission
	}
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var a *windows.ACCESS_ALLOWED_ACE
		if e = windows.GetAce(acl, i, &a); e != nil {
			return e
		}
		if a.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if a.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return fs.ErrPermission
		}
		sid := (*windows.SID)(unsafe.Pointer(&a.SidStart))
		if !sid.Equals(u.User.Sid) && !sid.IsWellKnown(windows.WinLocalSystemSid) && !sid.IsWellKnown(windows.WinBuiltinAdministratorsSid) {
			return fs.ErrPermission
		}
	}
	return nil
}
func syncDirectoryHandle(f *os.File) error { return windows.FlushFileBuffers(windows.Handle(f.Fd())) }
func unlinkBelow(dir *os.File, name string, f *os.File) error {
	// A DELETE-access relative handle binds removal to the inspected file, not
	// to a subsequently substituted name.
	h, e := mutationHandle(dir, name)
	if e != nil {
		return e
	}
	g := os.NewFile(uintptr(h), name)
	defer g.Close()
	a, e := inspect(f)
	b, e2 := inspect(g)
	if e != nil || e2 != nil {
		return errors.Join(e, e2)
	}
	if a.identity != b.identity {
		return fs.ErrPermission
	}
	var ios windows.IO_STATUS_BLOCK
	value := byte(1)
	return windows.NtSetInformationFile(h, &ios, &value, 1, windows.FileDispositionInformation)
}
func mutationHandle(dir *os.File, name string) (windows.Handle, error) {
	n, e := windows.NewNTUnicodeString(name)
	if e != nil {
		return 0, e
	}
	oa := windows.OBJECT_ATTRIBUTES{RootDirectory: windows.Handle(dir.Fd()), ObjectName: n, Attributes: windows.OBJ_DONT_REPARSE}
	oa.Length = uint32(unsafe.Sizeof(oa))
	var h windows.Handle
	var ios windows.IO_STATUS_BLOCK
	e = windows.NtCreateFile(&h, windows.GENERIC_READ|windows.DELETE|windows.SYNCHRONIZE, &oa, &ios, nil, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN, windows.FILE_OPEN_REPARSE_POINT|windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	return h, e
}
func publishBelow(dir *os.File, temp, name string, overwrite bool, expected *FileIdentity) (bool, error) {
	// Windows has no atomic exchange/CAS of a verified destination. Refuse
	// replacement of an existing destination rather than weakening the race
	// contract. Exclusive publication remains supported.
	if overwrite && expected != nil {
		return false, errors.ErrUnsupported
	}
	h, e := mutationHandle(dir, temp)
	if e != nil {
		return false, e
	}
	defer windows.CloseHandle(h)
	chars, e := windows.UTF16FromString(name)
	if e != nil {
		return false, e
	}
	chars = chars[:len(chars)-1]
	type renameInfo struct {
		Replace byte
		_       [7]byte
		Root    windows.Handle
		Length  uint32
		Name    uint16
	}
	off := int(unsafe.Offsetof(renameInfo{}.Name))
	buffer := make([]byte, off+2*len(chars))
	v := (*renameInfo)(unsafe.Pointer(&buffer[0]))
	v.Root = windows.Handle(dir.Fd())
	v.Length = uint32(len(chars) * 2)
	dst := unsafe.Slice((*uint16)(unsafe.Pointer(&buffer[off])), len(chars))
	copy(dst, chars)
	var ios windows.IO_STATUS_BLOCK
	e = windows.NtSetInformationFile(h, &ios, &buffer[0], uint32(len(buffer)), windows.FileRenameInformation)
	if e == windows.STATUS_OBJECT_NAME_COLLISION {
		return false, failure("CONFLICT", e)
	}
	return e == nil, e
}
