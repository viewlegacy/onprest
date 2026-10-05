package filesystem

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/viewlegacy/onprest/internal/contract"
)

// Identity is the durable OS identity of a pinned directory or regular file.
// It is local cleanup material, never public metadata.
type Identity struct {
	Device, Inode, Extra uint64
	Mount                string
}

func exported(i identity) Identity { return Identity{i.device, i.inode, i.extra, i.mount} }

// Directory pins a verified directory below a configured Root. All mutations
// use this descriptor; a renamed directory never redirects them to a new path.
type Directory struct {
	mu       sync.Mutex
	file     *os.File
	id       Identity
	absolute string
}

func (r *Root) Directory(relativePath, absoluteRoot string) (*Directory, error) {
	f, s, err := r.open(relativePath)
	if err != nil {
		return nil, err
	}
	if !s.info.IsDir() {
		f.Close()
		return nil, failure("PERMISSION_DENIED", fs.ErrPermission)
	}
	return &Directory{file: f, id: exported(s.identity), absolute: filepath.Join(absoluteRoot, filepath.FromSlash(path.Clean(relativePath)))}, nil
}

// DirectoryFromFile takes ownership of an already pinned directory descriptor.
// Its absolute name is recovery material; mutations never reopen that path.
func DirectoryFromFile(f *os.File, absolute string) (*Directory, error) {
	s, e := inspect(f)
	if e != nil || !s.info.IsDir() {
		return nil, errors.Join(e, fs.ErrPermission, f.Close())
	}
	return &Directory{file: f, id: exported(s.identity), absolute: absolute}, nil
}
func OpenDirectory(absolute string) (*Directory, error) {
	r, e := OpenRoot(absolute)
	if e != nil {
		return nil, e
	}
	defer r.Close()
	return r.Directory(".", absolute)
}
func (d *Directory) Identity() Identity { return d.id }
func (d *Directory) Path() string       { return d.absolute }
func (d *Directory) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.file == nil {
		return nil
	}
	e := d.file.Close()
	d.file = nil
	return e
}
func (d *Directory) Sync() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.file == nil {
		return fs.ErrClosed
	}
	return syncDirectoryHandle(d.file)
}

func SaveName(name string) error {
	if e := contract.ValidateBasename(name); e != nil {
		return failure("VALIDATION_FAILED", e)
	}
	if strings.HasPrefix(strings.ToLower(name), ".onprest-") || strings.ContainsAny(name, ":<>\"|?*") || strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return failure("VALIDATION_FAILED", fs.ErrInvalid)
	}
	base := strings.ToUpper(strings.SplitN(name, ".", 2)[0])
	if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || base == "CONIN$" || base == "CONOUT$" {
		return failure("VALIDATION_FAILED", fs.ErrInvalid)
	}
	for _, p := range []string{"COM", "LPT"} {
		for _, n := range []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "¹", "²", "³"} {
			if base == p+n {
				return failure("VALIDATION_FAILED", fs.ErrInvalid)
			}
		}
	}
	return nil
}

// CollisionKey accounts for the native filesystem's case behavior. macOS and
// Windows names are conservatively folded; Unix case-sensitive names remain distinct.
func CollisionKey(d *Directory, name string) string { return directoryCollisionKey(d.id, name) }

type FileIdentity struct {
	Identity                     Identity
	Size                         int64
	Owner                        string
	Mode                         uint32
	Modified                     int64
	ChangedSeconds, ChangedNanos int64
}

// MatchesDisplaced compares the original business identity and content stats.
// Exchange changes ctime, which is deliberately excluded.
func (v FileIdentity) MatchesDisplaced(i FileIdentity) bool {
	return v.Identity == i.Identity && v.Owner == i.Owner && v.Mode == i.Mode && v.Size == i.Size && v.Modified == i.Modified
}

func inspectRegular(f *os.File, private bool) (FileIdentity, error) {
	s, e := inspect(f)
	if e != nil {
		return FileIdentity{}, e
	}
	if !s.info.Mode().IsRegular() || s.links != 1 {
		return FileIdentity{}, fs.ErrPermission
	}
	if private {
		if e := privateTemp(f); e != nil {
			return FileIdentity{}, e
		}
	}
	owner, e := fileOwner(f)
	if e != nil {
		return FileIdentity{}, e
	}
	return FileIdentity{Identity: exported(s.identity), Size: s.info.Size(), Owner: owner, Mode: uint32(s.info.Mode()), Modified: s.info.ModTime().UnixNano(), ChangedSeconds: s.changeSeconds, ChangedNanos: s.changeNanos}, nil
}
func (d *Directory) Inspect(name string, private bool) (FileIdentity, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.file == nil {
		return FileIdentity{}, fs.ErrClosed
	}
	f, e := openBelow(d.file, name)
	if e != nil {
		return FileIdentity{}, e
	}
	defer f.Close()
	v, e := inspectRegular(f, private)
	if e == nil && (v.Identity.Device != d.id.Device || v.Identity.Extra != d.id.Extra || v.Identity.Mount != d.id.Mount) {
		e = fs.ErrPermission
	}
	return v, e
}
func (d *Directory) CreateTemp(name string) (*os.File, FileIdentity, error) {
	if !validTempName(name) {
		return nil, FileIdentity{}, fs.ErrInvalid
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.file == nil {
		return nil, FileIdentity{}, fs.ErrClosed
	}
	f, e := createBelow(d.file, name)
	if e != nil {
		return nil, FileIdentity{}, e
	}
	v, e := inspectRegular(f, true)
	if e != nil {
		f.Close()
		return nil, FileIdentity{}, e
	}
	return f, v, nil
}
func validTempName(s string) bool {
	if !strings.HasPrefix(s, ".onprest-") || len(s) != len(".onprest-")+32+len(".tmp") || !strings.HasSuffix(s, ".tmp") {
		return false
	}
	for _, c := range s[len(".onprest-") : len(s)-4] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func ValidTempName(s string) bool { return validTempName(s) }

// Publish returns whether the new business file was atomically installed. On
// exchange, the prior verified regular file remains under the recorded temp
// name until cleanup. A changed displaced identity is never automatically rolled back.
func (d *Directory) Publish(ctx context.Context, temp, name string, overwrite bool, temporary FileIdentity, expected *FileIdentity) (bool, error) {
	if e := ctx.Err(); e != nil {
		return false, e
	}
	if !validTempName(temp) {
		return false, fs.ErrInvalid
	}
	if e := SaveName(name); e != nil {
		return false, e
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.file == nil {
		return false, fs.ErrClosed
	}
	f, e := openBelow(d.file, temp)
	if e != nil {
		return false, changed(e)
	}
	actual, e := inspectRegular(f, true)
	e = errors.Join(e, f.Close())
	if e != nil || actual.Identity != temporary.Identity || actual.Owner != temporary.Owner || actual.Size != temporary.Size {
		return false, changed(errors.Join(e, fs.ErrPermission))
	}
	installed, e := publishBelow(d.file, temp, name, overwrite, expected)
	if e != nil {
		return installed, e
	}
	f, e = openBelow(d.file, name)
	if e != nil {
		return installed, changed(e)
	}
	actual, e = inspectRegular(f, true)
	e = errors.Join(e, f.Close())
	if e != nil || actual.Identity != temporary.Identity || actual.Owner != temporary.Owner || actual.Size != temporary.Size {
		return installed, changed(errors.Join(e, fs.ErrPermission))
	}
	if e = syncDirectoryHandle(d.file); e != nil {
		return installed, e
	}
	return installed, nil
}

// RemoveTemp checks identity/ownership through a no-follow descriptor before
// unlinking only the exact recorded basename. Unidentified files must be empty.
func (d *Directory) RemoveTemp(name string, identities []FileIdentity, emptyOnly bool) error {
	if !validTempName(name) {
		return fs.ErrInvalid
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.file == nil {
		return fs.ErrClosed
	}
	f, e := openBelow(d.file, name)
	if errors.Is(e, fs.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	v, e := inspectRegular(f, emptyOnly || len(identities) == 0)
	if e == nil && emptyOnly && v.Size != 0 {
		e = fs.ErrPermission
	}
	if e == nil && len(identities) > 0 {
		match := false
		for _, i := range identities {
			if v.Identity == i.Identity && v.Owner == i.Owner && (i.Identity == identities[0].Identity || v.MatchesDisplaced(i)) {
				match = true
			}
		}
		if !match {
			e = fs.ErrPermission
		}
	}
	if e == nil && len(identities) > 0 { // Original temp must remain private; displaced business file retains its original ACL.
		if v.Identity == identities[0].Identity {
			e = privateTemp(f)
		}
	}
	if e == nil {
		e = unlinkBelow(d.file, name, f)
	}
	return errors.Join(e, f.Close())
}
