//go:build windows

package providers

import (
	"errors"
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"unsafe"
)

func securityAttributes() (*windows.SecurityAttributes, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + user.User.Sid.String() + ")(A;;FA;;;SY)(A;;FA;;;BA)")
	if err != nil {
		return nil, err
	}
	return &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}, nil
}
func privateHandle(h windows.Handle, private bool) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("configuration path is a reparse point")
	}
	if !private {
		return nil
	}
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	control, _, err := sd.Control()
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return errors.New("configuration path has no private DACL")
	}
	var aces []privateACE
	if dacl != nil {
		for i := uint32(0); i < uint32(dacl.AceCount); i++ {
			var ace *windows.ACCESS_ALLOWED_ACE
			if err := windows.GetAce(dacl, i, &ace); err != nil {
				return err
			}
			aces = append(aces, privateACE{Type: ace.Header.AceType, SID: (*windows.SID)(unsafe.Pointer(&ace.SidStart))})
		}
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	return privateSecurity(control&windows.SE_DACL_PROTECTED != 0, owner, dacl != nil, aces, user.User.Sid)
}

// privateACE is one DACL entry reduced to what the protection check needs.
type privateACE struct {
	Type byte
	SID  *windows.SID
}

func trustedPrincipal(sid, user *windows.SID) bool {
	return sid != nil && (sid.Equals(user) || sid.IsWellKnown(windows.WinLocalSystemSid) || sid.IsWellKnown(windows.WinBuiltinAdministratorsSid))
}

// privateSecurity decides whether a secret, identity or spool object is
// private: like the Unix owner check, the owner must be the agent user,
// SYSTEM or Administrators; the DACL must be protected from inheritance and
// grant access only to those principals (deny entries are allowed).
func privateSecurity(protected bool, owner *windows.SID, hasDACL bool, aces []privateACE, user *windows.SID) error {
	if !trustedPrincipal(owner, user) {
		return errors.New("configuration path is owned by an unprivileged principal")
	}
	if !protected {
		return errors.New("configuration path DACL is not protected")
	}
	if !hasDACL {
		return errors.New("configuration path has no private DACL")
	}
	for _, ace := range aces {
		if ace.Type == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if ace.Type != windows.ACCESS_ALLOWED_ACE_TYPE {
			return errors.New("unsupported configuration DACL ACE")
		}
		if !trustedPrincipal(ace.SID, user) {
			return errors.New("configuration path permits an unprivileged principal")
		}
	}
	return nil
}
func openFile(path string, private bool, disposition, access uint32) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	var sa *windows.SecurityAttributes
	if disposition == windows.CREATE_NEW {
		sa, err = securityAttributes()
		if err != nil {
			return nil, err
		}
	}
	h, err := windows.CreateFile(name, access|windows.READ_CONTROL, windows.FILE_SHARE_READ|windows.FILE_SHARE_DELETE, sa, disposition, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(h), path)
	if err := privateHandle(h, private); err != nil {
		_ = f.Close()
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, errors.New("configuration material is not a regular file")
	}
	return f, nil
}
func openRegular(path string, private bool) (*os.File, error) {
	return openFile(path, private, windows.OPEN_EXISTING, windows.GENERIC_READ)
}
func createPrivate(path string) (*os.File, error) {
	return openFile(path, true, windows.CREATE_NEW, windows.GENERIC_READ|windows.GENERIC_WRITE)
}
func privateDirectory(path string) error {
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		sa, err := securityAttributes()
		if err != nil {
			return err
		}
		name, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return err
		}
		if err := windows.CreateDirectory(name, sa); err != nil && err != windows.ERROR_ALREADY_EXISTS {
			return err
		}
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ|windows.READ_CONTROL, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	if err := privateHandle(h, true); err != nil {
		return err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return errors.New("configuration identity path is not a directory")
	}
	return nil
}
func syncDirectory(string) error { return nil } // Key contents are FlushFileBuffers-synced before publication.
