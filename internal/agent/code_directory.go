package agent

import (
	"context"
	"errors"
	"github.com/viewlegacy/onprest/internal/agent/filesystem"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// codeDirectory binds an author-chosen directory by native descriptors. The
// descriptor root shares delete access (FILE_SHARE_DELETE on Windows), so
// holding it never blocks publishing or renaming a child directory.
type codeDirectory struct {
	root      *filesystem.Root
	directory *filesystem.Directory
	path      string
}

func openCodeDirectory(path string) (*codeDirectory, error) {
	absolute, e := filepath.Abs(path)
	if e != nil {
		return nil, e
	}
	d, e := filesystem.OpenDirectory(absolute)
	if e != nil {
		return nil, e
	}
	r, e := filesystem.OpenRoot(absolute)
	if e != nil {
		d.Close()
		return nil, e
	}
	f, e := r.HostOpen(".", os.O_RDONLY, 0)
	if e != nil {
		r.Close()
		d.Close()
		return nil, e
	}
	pinned, e := filesystem.DirectoryFromFile(f, absolute)
	if e != nil || pinned.Identity() != d.Identity() {
		if pinned != nil {
			pinned.Close()
		}
		r.Close()
		d.Close()
		return nil, errors.New("directory binding changed")
	}
	pinned.Close()
	verify, e := filesystem.OpenDirectory(absolute)
	if e != nil || verify.Identity() != d.Identity() {
		if verify != nil {
			verify.Close()
		}
		r.Close()
		d.Close()
		return nil, errors.New("directory binding changed")
	}
	verify.Close()
	return &codeDirectory{r, d, absolute}, nil
}
func (d *codeDirectory) Close() error { return errors.Join(d.root.Close(), d.directory.Close()) }
func (d *codeDirectory) bound() error {
	current, e := filesystem.OpenDirectory(d.path)
	if e != nil {
		return e
	}
	defer current.Close()
	if current.Identity() != d.directory.Identity() {
		return errors.New("directory binding changed")
	}
	return nil
}
func (d *codeDirectory) readRegular(name string, limit int64) ([]byte, bool, error) {
	info, e := d.root.HostStat(name)
	if errors.Is(e, fs.ErrNotExist) {
		return nil, false, nil
	}
	if e != nil {
		return nil, false, e
	}
	if !info.Mode().IsRegular() {
		return nil, true, errors.New("artifact is not a regular file")
	}
	f, e := d.root.HostOpen(name, os.O_RDONLY, 0)
	if e != nil {
		return nil, true, e
	}
	if !filesystem.SameEntry(info, f) {
		f.Close()
		return nil, true, errors.New("artifact binding changed")
	}
	b, e := io.ReadAll(io.LimitReader(f, limit+1))
	e = errors.Join(e, f.Close())
	if int64(len(b)) > limit {
		e = errors.Join(e, errors.New("artifact exceeds byte limit"))
	}
	return b, true, e
}
func (d *codeDirectory) writePrivate(name string, b []byte) error {
	f, _, e := d.directory.CreateTemp(name)
	if e != nil {
		return e
	}
	e = checkedWrite(f, b)
	if e == nil {
		e = f.Sync()
	}
	return errors.Join(e, f.Close())
}

// A fault hook is passed explicitly by tests; production never supplies one.
func (d *codeDirectory) publishPair(ctx context.Context, name, manifest string, b, mb []byte, old map[string][]byte) error {
	return d.publishPairWithFault(ctx, name, manifest, b, mb, old, nil)
}
func (d *codeDirectory) publishPairWithFault(ctx context.Context, name, manifest string, b, mb []byte, old map[string][]byte, fault func(string) error) (err error) {
	prefix := ".onprest-code-build-" + randomTransportID()
	moduleTemp := ".onprest-" + randomTransportID() + ".tmp"
	manifestTemp := ".onprest-" + randomTransportID() + ".tmp"
	backups := map[string]string{}
	published := map[string]bool{}
	defer func() {
		if err != nil {
			for _, n := range []string{name, manifest} {
				if published[n] {
					err = errors.Join(err, d.root.HostRemove(n))
				}
				if backup, ok := backups[n]; ok {
					err = errors.Join(err, d.root.HostRename(backup, d.root, n))
				}
			}
		}
		for _, n := range []string{moduleTemp, manifestTemp} {
			if e := d.root.HostRemove(n); e != nil && !errors.Is(e, fs.ErrNotExist) {
				err = errors.Join(err, e)
			}
		}
		if err == nil {
			for _, n := range backups {
				err = errors.Join(err, d.root.HostRemove(n))
			}
		}
	}()
	if err = d.writePrivate(moduleTemp, b); err != nil {
		return err
	}
	if err = d.writePrivate(manifestTemp, mb); err != nil {
		return err
	}
	if err = d.bound(); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	for _, n := range []string{manifest, name} {
		current, exists, e := d.readRegular(n, 32<<20)
		if e != nil {
			return e
		}
		previous, was := old[n]
		if exists != was || exists && string(current) != string(previous) {
			return errors.New("existing build pair changed")
		}
		if exists {
			backup := prefix + ".old-" + codeHash([]byte(n))[:8]
			if err = d.root.HostRename(n, d.root, backup); err != nil {
				return err
			}
			backups[n] = backup
		}
	}
	for _, entry := range []struct{ temp, dest, phase string }{{moduleTemp, name, "module"}, {manifestTemp, manifest, "manifest"}} {
		if fault != nil {
			if err = fault(entry.phase); err != nil {
				return err
			}
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = d.bound(); err != nil {
			return err
		}
		identity, e := d.directory.Inspect(entry.temp, true)
		if e != nil {
			return e
		}
		var moved bool
		moved, err = d.directory.Publish(ctx, entry.temp, entry.dest, false, identity, nil)
		published[entry.dest] = moved
		if err != nil {
			return err
		}
	}
	return d.directory.Sync()
}

// removeFlat removes a run directory that holds only regular files, through
// the same no-follow descriptors; any other entry makes removal fail.
func (d *codeDirectory) removeFlat(name string) error {
	dir, e := d.root.HostOpen(name, os.O_RDONLY, 0)
	if e != nil {
		return e
	}
	entries, e := dir.ReadDir(-1)
	e = errors.Join(e, dir.Close())
	if e != nil {
		return e
	}
	for _, entry := range entries {
		if e := d.root.HostRemove(name + "/" + entry.Name()); e != nil {
			return e
		}
	}
	return d.root.HostRemove(name)
}
