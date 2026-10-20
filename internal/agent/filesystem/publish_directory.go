package filesystem

import (
	"context"
	"errors"
	"github.com/viewlegacy/onprest/internal/contract"
	"io/fs"
	"path/filepath"
)

// PublishDirectory moves a pinned private directory within its pinned parent.
// Native exclusive rename never replaces a completed file or directory.
func (d *Directory) PublishDirectory(ctx context.Context, temp, name string, expected Identity) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.file == nil {
		return false, fs.ErrClosed
	}
	if SaveName(name) != nil || contract.ValidateBasename(temp) != nil || temp == name {
		return false, fs.ErrInvalid
	}
	if e := ctx.Err(); e != nil {
		return false, e
	}
	f, e := openBelow(d.file, temp)
	if e != nil {
		return false, e
	}
	st, e := inspect(f)
	e = errors.Join(e, f.Close())
	if e != nil {
		return false, e
	}
	if !st.info.IsDir() || exported(st.identity) != expected {
		return false, fs.ErrPermission
	}
	e = renameDirectoryExclusive(d.file, temp, name)
	if e != nil {
		return false, e
	}
	completed, e := openBelow(d.file, name)
	if e != nil {
		return true, e
	}
	actual, e := inspect(completed)
	e = errors.Join(e, completed.Close())
	if e == nil && (!actual.info.IsDir() || exported(actual.identity) != expected) {
		e = fs.ErrPermission
	}
	return true, e
}

// CreatePrivateDirectory creates a fresh run directory relative to this handle.
func (d *Directory) CreatePrivateDirectory(name string) (*Directory, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.file == nil {
		return nil, fs.ErrClosed
	}
	if contract.ValidateBasename(name) != nil {
		return nil, fs.ErrInvalid
	}
	f, e := createPrivateDirectory(d.file, name)
	if e != nil {
		return nil, e
	}
	return DirectoryFromFile(f, filepath.Join(d.absolute, name))
}
