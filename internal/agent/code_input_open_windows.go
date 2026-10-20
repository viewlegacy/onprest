//go:build windows

package agent

import (
	"errors"
	"io/fs"
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

func codeOpenInputNative(path string) (*os.File, error) {
	if strings.HasPrefix(path, `\\?\`) || strings.HasPrefix(path, `\\.\`) {
		return nil, fs.ErrPermission
	}
	name, e := windows.UTF16PtrFromString(path)
	if e != nil {
		return nil, e
	}
	h, e := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if e != nil {
		return nil, e
	}
	f := os.NewFile(uintptr(h), path)
	var info windows.ByHandleFileInformation
	e = windows.GetFileInformationByHandle(h, &info)
	kind, e2 := windows.GetFileType(h)
	if e != nil || e2 != nil || kind != windows.FILE_TYPE_DISK || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return nil, errors.Join(e, e2, fs.ErrPermission, f.Close())
	}
	return f, nil
}
