package filesystem

import (
	"errors"
	"io/fs"
	"os"
	"path"
)

// HostOpen opens an authorized descriptor without following links. Truncation
// happens only after the descriptor's type, filesystem and link count pass.
func (r *Root) HostOpen(p string, flags int, perm fs.FileMode) (*os.File, error) {
	f, _, e := r.HostOpenObserved(p, flags, perm)
	return f, e
}

// HostOpenObserved keeps the authorized native descriptor path, reporting only
// atomic creation or successful truncation, never a path snapshot inference.
func (r *Root) HostOpenObserved(p string, flags int, perm fs.FileMode) (*os.File, bool, error) {
	if err := relative(p); err != nil {
		return nil, false, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.dir == nil {
		return nil, false, fs.ErrClosed
	}
	f, mutated, e := observedCreate(path.Clean(p), flags&^os.O_TRUNC, func(n string, f int) (*os.File, error) { return hostOpen(r.dir, n, f, perm) }, false)
	if e != nil {
		return nil, mutated, e
	}
	s, e := inspect(f)
	if e == nil && (!sameFilesystem(r.identity, s.identity) || (!s.info.IsDir() && (!s.info.Mode().IsRegular() || s.links != 1))) {
		e = fs.ErrPermission
	}
	if e == nil && flags&os.O_TRUNC != 0 {
		if s.info.IsDir() {
			e = fs.ErrInvalid
		} else {
			e = f.Truncate(0)
			if e == nil {
				mutated = true
			}
		}
	}
	if e != nil {
		f.Close()
		return nil, mutated, e
	}
	return f, mutated, nil
}
func (r *Root) hostParent(p string) (*os.File, string, error) {
	if e := relative(p); e != nil {
		return nil, "", e
	}
	p = path.Clean(p)
	if p == "." {
		return nil, "", fs.ErrPermission
	}
	f, e := r.HostOpen(path.Dir(p), os.O_RDONLY, 0)
	return f, path.Base(p), e
}
func (r *Root) HostMkdir(p string, perm fs.FileMode) error {
	d, n, e := r.hostParent(p)
	if e != nil {
		return e
	}
	defer d.Close()
	return hostMkdir(d, n, perm)
}
func (r *Root) HostRemove(p string) error {
	d, n, e := r.hostParent(p)
	if e != nil {
		return e
	}
	defer d.Close()
	f, e := r.HostOpen(p, os.O_RDONLY, 0)
	if e != nil {
		return e
	}
	defer f.Close()
	i, e := f.Stat()
	if e != nil {
		return e
	}
	return hostRemove(d, n, f, i.IsDir())
}
func (r *Root) HostRename(old string, dst *Root, new string) error {
	a, n, e := r.hostParent(old)
	if e != nil {
		return e
	}
	defer a.Close()
	b, m, e := dst.hostParent(new)
	if e != nil {
		return e
	}
	defer b.Close()
	f, e := r.HostOpen(old, os.O_RDONLY, 0)
	if e != nil {
		return e
	}
	defer f.Close()
	g, e := dst.HostOpen(new, os.O_RDONLY, 0)
	if e == nil {
		g.Close()
	} else if !errors.Is(e, fs.ErrNotExist) {
		return e
	}
	return hostRename(a, n, b, m, f)
}

// HostDirectoryChild opens only a single name from an already authorized
// directory descriptor. Renames of the directory never rebind this operation.
func HostDirectoryChild(directory *os.File, name string, strict bool) (*os.File, error) {
	if e := relative(name); e != nil {
		return nil, e
	}
	if path.Base(name) != name {
		return nil, fs.ErrInvalid
	}
	parent, e := inspect(directory)
	if e != nil {
		return nil, e
	}
	if !parent.info.IsDir() {
		return nil, fs.ErrInvalid
	}
	var f *os.File
	if strict {
		f, e = hostOpen(directory, name, os.O_RDONLY, 0)
	} else {
		f, e = hostOrdinaryChild(directory, name)
	}
	if e != nil {
		return nil, e
	}
	child, e := inspect(f)
	if e == nil && (!child.info.IsDir() && !child.info.Mode().IsRegular() || strict && (!sameFilesystem(parent.identity, child.identity) || !child.info.IsDir() && child.links != 1)) {
		e = fs.ErrPermission
	}
	if e != nil {
		f.Close()
		return nil, e
	}
	return f, nil
}

// Re-opening "." relative to the held descriptor resets OS enumeration state
// on every supported OS without resolving the original path again.
func HostRewindDirectory(directory *os.File, strict bool) (*os.File, error) {
	return HostDirectoryChild(directory, ".", strict)
}
