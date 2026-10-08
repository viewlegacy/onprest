//go:build windows

package filesystem

import (
	"errors"
	"golang.org/x/sys/windows"
	"io/fs"
	"os"
	"testing"
)

// Native execution of this table and the host ABI mutation fixtures is P20.
func TestHostWindowsErrorCategories(t *testing.T) {
	cases := []struct{ err, errorClass error }{
		{nil, nil}, {windows.STATUS_OBJECT_NAME_NOT_FOUND, fs.ErrNotExist}, {windows.STATUS_OBJECT_PATH_NOT_FOUND, fs.ErrNotExist},
		{windows.STATUS_OBJECT_NAME_COLLISION, fs.ErrExist}, {windows.STATUS_ACCESS_DENIED, fs.ErrPermission},
		{windows.STATUS_REPARSE_POINT_ENCOUNTERED, fs.ErrPermission}, {windows.STATUS_INVALID_PARAMETER, fs.ErrInvalid},
		{windows.STATUS_OBJECT_NAME_INVALID, fs.ErrInvalid}, {windows.STATUS_NOT_SUPPORTED, errors.ErrUnsupported},
		{&os.PathError{Op: "openRoot", Path: "private", Err: windows.ERROR_PATH_NOT_FOUND}, fs.ErrNotExist},
		{&os.PathError{Op: "openBelow", Path: "private", Err: windows.ERROR_FILE_NOT_FOUND}, fs.ErrNotExist},
		{&os.PathError{Op: "inspect", Path: "private", Err: windows.STATUS_INVALID_HANDLE}, fs.ErrInvalid},
		{windows.ERROR_ALREADY_EXISTS, fs.ErrExist}, {windows.ERROR_FILE_EXISTS, fs.ErrExist},
		{windows.ERROR_INVALID_NAME, fs.ErrInvalid}, {windows.ERROR_BAD_PATHNAME, fs.ErrInvalid}, {windows.ERROR_FILENAME_EXCED_RANGE, fs.ErrInvalid},
		{windows.ERROR_NOT_SUPPORTED, errors.ErrUnsupported}, {windows.ERROR_INVALID_FUNCTION, errors.ErrUnsupported},
		{windows.STATUS_PRIVILEGE_NOT_HELD, fs.ErrPermission}, {windows.ERROR_PRIVILEGE_NOT_HELD, fs.ErrPermission}, {windows.ERROR_INVALID_HANDLE, fs.ErrInvalid},
		{windows.ERROR_INVALID_PARAMETER, fs.ErrInvalid}, {windows.ERROR_ACCESS_DENIED, fs.ErrPermission},
	}
	for _, c := range cases {
		got := hostWindowsError(c.err)
		if !errors.Is(got, c.errorClass) {
			t.Fatalf("%v => %v want %v", c.err, got, c.errorClass)
		}
	}
	if e := hostWindowsError(&os.PathError{Op: "inspect", Path: "private", Err: windows.STATUS_DISK_FULL}); e == nil || errors.Is(e, fs.ErrPermission) || errors.Is(e, fs.ErrInvalid) {
		t.Fatal(e)
	}
}
