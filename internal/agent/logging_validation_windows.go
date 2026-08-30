//go:build windows

package agent

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

type validationNativeLock struct {
	file       *os.File
	overlapped windows.Overlapped
}

var validationLockOpenedHook = func(string) {}

func privateSecurityAttributes() (*windows.SecurityAttributes, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	sddl := "D:P(A;;FA;;;" + user.User.Sid.String() + ")(A;;FA;;;SY)(A;;FA;;;BA)"
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return nil, err
	}
	return &windows.SecurityAttributes{
		Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd,
	}, nil
}

func openPrivateValidationFile(path string, disposition uint32, access uint32) (*os.File, error) {
	sa, err := privateSecurityAttributes()
	if err != nil {
		return nil, err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(name, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, sa, disposition,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(h), path)
	if err := validatePrivateWindowsHandle(h); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func acquireValidationLock(path string) (*validationNativeLock, bool, error) {
	file, err := openPrivateValidationFile(path, windows.OPEN_ALWAYS, windows.GENERIC_READ|windows.GENERIC_WRITE)
	if err != nil {
		return nil, false, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != 0 {
		_ = file.Close()
		if err == nil {
			err = fmt.Errorf("validation lock path is not an empty regular file")
		}
		return nil, false, err
	}
	validationLockOpenedHook(path)
	lock := &validationNativeLock{file: file}
	err = windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &lock.overlapped)
	if err == windows.ERROR_LOCK_VIOLATION {
		_ = file.Close()
		return nil, true, nil
	}
	if err != nil {
		_ = file.Close()
		return nil, false, err
	}
	return lock, false, nil
}

func (l *validationNativeLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := windows.UnlockFileEx(windows.Handle(l.file.Fd()), 0, 1, 0, &l.overlapped)
	closeErr := l.file.Close()
	l.file = nil
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

func createPrivateValidationFile(path string) (*os.File, error) {
	return openPrivateValidationFile(path, windows.CREATE_NEW, windows.GENERIC_READ|windows.GENERIC_WRITE)
}

func validateExistingPrivateFile(path string) error {
	file, err := openPrivateValidationFile(path, windows.OPEN_EXISTING, windows.GENERIC_READ|windows.READ_CONTROL)
	if err == windows.ERROR_FILE_NOT_FOUND || err == windows.ERROR_PATH_NOT_FOUND {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > validationDetailLogMaxBytes {
		return fmt.Errorf("validation detail path is not a bounded regular file")
	}
	return nil
}

func validatePrivateWindowsHandle(handle windows.Handle) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("validation path is a reparse point")
	}
	sd, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		return fmt.Errorf("validation path DACL is not protected")
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return fmt.Errorf("validation path has no private DACL")
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return err
		}
		if ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return fmt.Errorf("validation path DACL contains an unsupported allow-capable ACE")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.Equals(user.User.Sid) && !sid.IsWellKnown(windows.WinLocalSystemSid) && !sid.IsWellKnown(windows.WinBuiltinAdministratorsSid) {
			return fmt.Errorf("validation path DACL permits an unprivileged principal")
		}
	}
	return nil
}

func atomicReplaceValidationFile(source, target string) error {
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		return err
	}
	if err := validationPostCommitDurabilityHook(); err != nil {
		return err
	}
	return validateExistingPrivateFile(target)
}

func syncValidationDirectory(string) error { return nil }
