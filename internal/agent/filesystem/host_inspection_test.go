package filesystem

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

type inspectionFault struct {
	info              fs.FileInfo
	statErr, closeErr error
	closed            int
}

func (f *inspectionFault) Stat() (fs.FileInfo, error) { return f.info, f.statErr }
func (f *inspectionFault) Close() error               { f.closed++; return f.closeErr }
func TestHostDirectoryInspectionCloseAndFailure(t *testing.T) {
	base := t.TempDir()
	dir, _ := os.Stat(base)
	os.WriteFile(filepath.Join(base, "f"), nil, 0600)
	file, _ := os.Stat(filepath.Join(base, "f"))
	for _, tc := range []struct {
		info               fs.FileInfo
		stat, closed, want error
	}{{dir, nil, nil, nil}, {file, nil, nil, fs.ErrExist}, {dir, nil, syscall.EIO, syscall.EIO}, {nil, fs.ErrNotExist, nil, fs.ErrNotExist}, {nil, fs.ErrNotExist, syscall.EIO, fs.ErrNotExist}} {
		f := &inspectionFault{info: tc.info, statErr: tc.stat, closeErr: tc.closed}
		e := inspectDirectoryFile(f)
		if !errors.Is(e, tc.want) || f.closed != 1 {
			t.Fatal(e, tc.want, f.closed)
		}
	}
	r, e := OpenRoot(base)
	if e != nil {
		t.Fatal(e)
	}
	if e = r.HostInspectDirectory("."); e != nil {
		t.Fatal(e)
	}
	if e = r.HostInspectDirectory("f"); !errors.Is(e, fs.ErrExist) {
		t.Fatal(e)
	}
	if e = r.HostInspectMutation("f"); e != nil {
		t.Fatal(e)
	}
	r.Close()
	if e = r.HostInspectMutation("f"); !errors.Is(e, fs.ErrClosed) {
		t.Fatal(e)
	}
}
