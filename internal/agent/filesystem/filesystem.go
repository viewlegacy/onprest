// Package filesystem provides descriptor-based folder access and safe native
// file publication shared by CSV/file providers and the strict host bridge. It
// does not register a provider, own a spool, or publish external artifacts.
package filesystem

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/viewlegacy/onprest/internal/contract"
)

// Error exposes a fixed category and safe text. Unwrap is for local diagnostics;
// callers must never publish the underlying OS error or root-relative path.
type Error struct {
	Code  string
	cause error
}

func (e *Error) Error() string { return "folder operation failed: " + e.Code }
func (e *Error) Unwrap() error { return e.cause }

type contextError struct{ cause error }

func (e *contextError) Error() string {
	if errors.Is(e.cause, context.Canceled) {
		return "folder operation canceled"
	}
	return "folder operation exceeded deadline"
}
func (e *contextError) Unwrap() error { return e.cause }
func failure(code string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return &contextError{cause: err}
	}
	return &Error{Code: code, cause: err}
}
func joinFailures(errs ...error) error {
	err := errors.Join(errs...)
	if err == nil {
		return nil
	}
	var typed *Error
	if errors.As(err, &typed) {
		return failure(typed.Code, err)
	}
	return osFailure(err)
}
func osFailure(err error) error {
	var output *contract.OutputError
	if errors.As(err, &output) && output.Code == "LIMIT_EXCEEDED" {
		return failure("LIMIT_EXCEEDED", err)
	}
	if errors.Is(err, fs.ErrNotExist) {
		return failure("NOT_FOUND", err)
	}
	if errors.Is(err, fs.ErrPermission) {
		return failure("PERMISSION_DENIED", err)
	}
	return failure("EXECUTION_FAILED", err)
}
func changed(err error) error { return failure("SOURCE_CHANGED", err) }

// Root pins the directory identity obtained at OpenRoot. Trusted ancestors of
// the configured absolute path belong to the operator; its final component and
// all accesses below it reject links, special files, and filesystem crossings.
// Close may run concurrently with operations and never closes a consumer's
// File. Consumers keep Root alive until their Files have closed, so final
// verification can check the root-relative binding.
type Root struct {
	mu       sync.RWMutex
	dir      *os.File
	identity identity
}

func OpenRoot(absolute string) (*Root, error) {
	if !filepath.IsAbs(absolute) || !utf8.ValidString(absolute) || strings.ContainsRune(absolute, 0) {
		return nil, failure("VALIDATION_FAILED", fs.ErrInvalid)
	}
	f, err := openRoot(filepath.Clean(absolute))
	if err != nil {
		return nil, osFailure(err)
	}
	stamp, err := inspect(f)
	if err != nil {
		_ = f.Close()
		return nil, osFailure(err)
	}
	if !stamp.info.IsDir() {
		_ = f.Close()
		return nil, failure("PERMISSION_DENIED", fs.ErrPermission)
	}
	return &Root{dir: f, identity: stamp.identity}, nil
}
func (r *Root) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dir == nil {
		return nil
	}
	f := r.dir
	r.dir = nil
	if err := f.Close(); err != nil {
		return osFailure(err)
	}
	return nil
}
func relative(s string) error {
	if !utf8.ValidString(s) || s == "" || strings.ContainsAny(s, "\x00\\") || strings.HasPrefix(s, "/") || filepath.IsAbs(s) {
		return fs.ErrInvalid
	}
	for _, part := range strings.Split(s, "/") {
		if part == ".." {
			return fs.ErrInvalid
		}
		if err := platformName(part); err != nil {
			return err
		}
	}
	return nil
}
func reserved(s string) bool { return strings.HasPrefix(s, ".onprest-") }
func allowedRelative(s string) error {
	if err := relative(s); err != nil {
		return err
	}
	for _, part := range strings.Split(s, "/") {
		if reserved(part) {
			return fs.ErrPermission
		}
	}
	return nil
}
func (r *Root) open(relativePath string) (*os.File, stamp, error) {
	if err := allowedRelative(relativePath); err != nil {
		return nil, stamp{}, failure("PERMISSION_DENIED", err)
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.dir == nil {
		return nil, stamp{}, osFailure(fs.ErrClosed)
	}
	f, err := openBelow(r.dir, path.Clean(relativePath))
	if err != nil {
		return nil, stamp{}, osFailure(err)
	}
	s, err := inspect(f)
	if err == nil && (!sameFilesystem(r.identity, s.identity) || (!s.info.IsDir() && (!s.info.Mode().IsRegular() || s.links != 1))) {
		err = fs.ErrPermission
	}
	if err != nil {
		_ = f.Close()
		return nil, stamp{}, osFailure(err)
	}
	return f, s, nil
}

// File reads only the descriptor authorized at open. Verify checks the original
// identity/size/modification state and the current root-relative binding. EOF
// and Close also verify. A detected change is never silently re-opened/retried.
// A producer must write a temporary file and atomically rename its complete
// contents into place; these checks cannot guarantee arbitrary-writer coherence.
// A File is owned by one consumer, like an ordinary io.ReadCloser.
type File struct {
	root     *Root
	file     *os.File
	relative string
	before   stamp
	ctx      context.Context
	closed   bool
}

func (r *Root) Open(ctx context.Context, relativePath string) (*File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, s, err := r.open(relativePath)
	if err != nil {
		return nil, err
	}
	if !s.info.Mode().IsRegular() {
		_ = f.Close()
		return nil, failure("PERMISSION_DENIED", fs.ErrPermission)
	}
	return &File{root: r, file: f, relative: path.Clean(relativePath), before: s, ctx: ctx}, nil
}
func (f *File) Read(b []byte) (int, error) {
	if f.closed {
		return 0, fs.ErrClosed
	}
	if err := f.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := f.file.Read(b)
	if err == io.EOF {
		if e := f.Verify(); e != nil {
			return n, e
		}
	} else if err != nil {
		return n, osFailure(err)
	}
	return n, err
}
func (f *File) Verify() error {
	if f.closed {
		return fs.ErrClosed
	}
	if err := f.ctx.Err(); err != nil {
		return err
	}
	after, err := inspect(f.file)
	if err != nil {
		return changed(err)
	}
	if !sameStamp(f.before, after) {
		return changed(errors.New("opened file changed"))
	}
	current, s, err := f.root.open(f.relative)
	if err != nil {
		return changed(err)
	}
	closeErr := current.Close()
	if !sameStamp(f.before, s) {
		return changed(errors.New("selected path changed"))
	}
	if closeErr != nil {
		return osFailure(closeErr)
	}
	return nil
}
func (f *File) Close() error {
	if f.closed {
		return nil
	}
	err := f.Verify()
	f.closed = true
	return joinFailures(err, f.file.Close())
}

type identity struct {
	device uint64
	inode  uint64
	extra  uint64
	mount  string
}
type stamp struct {
	info          fs.FileInfo
	identity      identity
	links         uint64
	changeSeconds int64
	changeNanos   int64
}

func sameStamp(a, b stamp) bool {
	return a.identity == b.identity && a.links == b.links && a.changeSeconds == b.changeSeconds && a.changeNanos == b.changeNanos && a.info.Size() == b.info.Size() && a.info.Mode() == b.info.Mode() && a.info.ModTime().Equal(b.info.ModTime())
}
func sameFilesystem(a, b identity) bool {
	return a.device == b.device && a.extra == b.extra && a.mount == b.mount
}
