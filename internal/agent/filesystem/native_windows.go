//go:build windows

package filesystem

import (
	"golang.org/x/sys/windows"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unsafe"
)

func platformName(s string) error {
	if s == "" || s == "." {
		return nil
	}
	if strings.ContainsAny(s, ":<>\"|?*") || strings.HasSuffix(s, ".") || strings.HasSuffix(s, " ") {
		return fs.ErrInvalid
	}
	base := strings.ToUpper(strings.SplitN(s, ".", 2)[0])
	if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || base == "CONIN$" || base == "CONOUT$" || len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && (base[3] >= '1' && base[3] <= '9' || base[3] == 0xb9) {
		return fs.ErrInvalid
	}
	// Windows also treats the superscript digits as DOS device aliases.
	for _, prefix := range []string{"COM", "LPT"} {
		for _, digit := range []string{"¹", "²", "³"} {
			if base == prefix+digit {
				return fs.ErrInvalid
			}
		}
	}
	return nil
}
func openRoot(p string) (*os.File, error) {
	if strings.HasPrefix(p, `\\?\`) || strings.HasPrefix(p, `\\.\`) {
		return nil, fs.ErrPermission
	}
	rest := strings.TrimPrefix(p, filepath.VolumeName(p))
	for _, part := range strings.Split(rest, `\`) {
		if err := platformName(part); err != nil {
			return nil, err
		}
	}
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(h), p)
	if _, err := inspect(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}
func openBelow(root *os.File, p string) (*os.File, error) {
	// NT relative opens use an empty object name to re-open the directory
	// itself with independent enumeration state (the Go runtime does likewise).
	if p == "." {
		p = ""
	}
	name, err := windows.NewNTUnicodeString(strings.ReplaceAll(p, "/", `\`))
	if err != nil {
		return nil, err
	}
	oa := windows.OBJECT_ATTRIBUTES{RootDirectory: windows.Handle(root.Fd()), ObjectName: name, Attributes: windows.OBJ_DONT_REPARSE}
	oa.Length = uint32(unsafe.Sizeof(oa))
	var h windows.Handle
	var ios windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(&h, windows.GENERIC_READ|windows.SYNCHRONIZE, &oa, &ios, nil, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN, windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_FOR_BACKUP_INTENT, 0, 0)
	if err != nil {
		if status, ok := err.(windows.NTStatus); ok {
			switch status {
			case windows.STATUS_OBJECT_NAME_NOT_FOUND, windows.STATUS_OBJECT_PATH_NOT_FOUND:
				return nil, fs.ErrNotExist
			}
			return nil, fs.ErrPermission
		}
		return nil, err
	}
	return os.NewFile(uintptr(h), p), nil
}
func inspect(f *os.File) (stamp, error) {
	h := windows.Handle(f.Fd())
	typ, err := windows.GetFileType(h)
	if err != nil {
		return stamp{}, err
	}
	if typ != windows.FILE_TYPE_DISK {
		return stamp{}, fs.ErrPermission
	}
	var st windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &st); err != nil {
		return stamp{}, err
	}
	if st.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return stamp{}, fs.ErrPermission
	}
	info, err := f.Stat()
	if err != nil {
		return stamp{}, err
	}
	var basic struct {
		CreationTime, LastAccessTime, LastWriteTime, ChangeTime int64
		FileAttributes                                          uint32
		padding                                                 uint32
	}
	var ios windows.IO_STATUS_BLOCK
	if err := windows.NtQueryInformationFile(h, &ios, (*byte)(unsafe.Pointer(&basic)), uint32(unsafe.Sizeof(basic)), windows.FileBasicInformation); err != nil {
		return stamp{}, err
	}
	return stamp{info: info, identity: identity{device: uint64(st.VolumeSerialNumber), inode: uint64(st.FileIndexHigh)<<32 | uint64(st.FileIndexLow)}, links: uint64(st.NumberOfLinks), changeSeconds: basic.ChangeTime}, nil
}
