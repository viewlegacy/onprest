// Package artifacts owns private, temporary Agent snapshots. It has no public
// transport, URI, provider registration, or durable execution result contract.
package artifacts

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"io/fs"
	"mime"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/viewlegacy/onprest/internal/contract"
)

// Owner is compared in full on every lookup. Its values come from trusted Agent
// dispatch, never from a file ID or self-asserted external descriptor.
type Owner struct{ RequestID, CallerBinding, Capability, DefinitionHash, AuthGeneration string }

func (o Owner) valid() bool {
	for _, s := range []string{o.RequestID, o.CallerBinding, o.Capability, o.DefinitionHash, o.AuthGeneration} {
		if s == "" || len(s) > 256 || !utf8.ValidString(s) || strings.ContainsRune(s, 0) {
			return false
		}
	}
	return true
}

type Error struct {
	Code  string
	cause error
}

func (e *Error) Error() string { return "private artifact operation failed: " + e.Code }
func (e *Error) Unwrap() error {
	if e.Code == "LIMIT_EXCEEDED" {
		return errors.Join(e.cause, &contract.OutputError{Code: e.Code})
	}
	return e.cause
}
func fail(code string, cause error) error { return &Error{code, cause} }
func safe(err error) error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		// A joined error may also contain raw OS cleanup details. Preserve the
		// category, but always keep the entire join behind one safe summary.
		return fail(e.Code, err)
	}
	return fail("EXECUTION_FAILED", err)
}

type Options struct {
	Directory              string // Absolute configuration-relative path, resolved by prepare.
	QuotaBytes             int64
	MaxPending, MaxObjects int
	TransferTimeout        time.Duration
}
type Store struct {
	mu               sync.Mutex
	root, objects    *os.Root
	lock             *os.File
	options          Options
	used             int64
	readers          int
	closed, poisoned bool
	closeErr         error
	stages           map[string]*Stage
	requests         map[string]*Request
	reservations     map[*Reservation]bool
	journal          *saveJournal
	saveInit         sync.Mutex
	// Fault injection belongs to package tests; production always uses disk I/O.
	write    func(*os.File, []byte) (int, error)
	remove   func(string) error
	syncFile func(*os.File) error
}

var objectName = regexp.MustCompile(`^[ioh]-[0-9a-f]{32}\.spool$`)

func Open(options Options) (_ *Store, err error) {
	if !filepath.IsAbs(options.Directory) || !utf8.ValidString(options.Directory) || strings.ContainsRune(options.Directory, 0) || options.QuotaBytes < 1 || options.MaxPending < 1 || options.MaxPending > (int(^uint(0)>>1)-3)/2 || options.MaxObjects < 1 || options.MaxObjects == int(^uint(0)>>1) || options.TransferTimeout <= 0 || options.TransferTimeout > 300*time.Second {
		return nil, fail("VALIDATION_FAILED", fs.ErrInvalid)
	}
	if err = privateDirectory(options.Directory); err != nil {
		return nil, safe(err)
	}
	r, err := os.OpenRoot(options.Directory)
	if err != nil {
		return nil, safe(err)
	}
	s := &Store{root: r, options: options, stages: map[string]*Stage{}, requests: map[string]*Request{}, reservations: map[*Reservation]bool{}, write: func(f *os.File, p []byte) (int, error) { return f.Write(p) }, syncFile: func(f *os.File) error { return f.Sync() }}
	defer func() {
		if err != nil {
			_ = s.closeHandles()
		}
	}()
	if info, e := r.Lstat(".lock"); e == nil && !info.Mode().IsRegular() {
		return nil, fail("PERMISSION_DENIED", fs.ErrPermission)
	} else if e != nil && !errors.Is(e, fs.ErrNotExist) {
		return nil, safe(e)
	}
	s.lock, err = r.OpenFile(".lock", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	newLock := err == nil
	if errors.Is(err, fs.ErrExist) {
		s.lock, err = r.OpenFile(".lock", os.O_RDWR, 0)
	}
	if err != nil {
		return nil, safe(err)
	}
	if err = protect(s.lock, newLock); err != nil {
		return nil, safe(err)
	}
	if i, e := s.lock.Stat(); e != nil || i.Size() != 0 {
		return nil, fail("PERMISSION_DENIED", e)
	}
	if err = lockFile(s.lock); err != nil {
		return nil, fail("BUSY", err)
	}
	// Durable business-temp recovery precedes ephemeral deletion and all
	// request admission, regardless of the current capability/source settings.
	if err = s.openSaveJournal(); err != nil {
		return nil, safe(err)
	}
	// Only the dedicated ephemeral namespace is recovered. P08's cleanup
	// manifests and business directories are never scanned or deleted here.
	err = r.Mkdir("objects", 0700)
	newDirectory := err == nil
	if err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, safe(err)
	}
	i, e := r.Lstat("objects")
	if e != nil || !i.IsDir() {
		return nil, fail("PERMISSION_DENIED", e)
	}
	directory, e := r.Open("objects")
	if e != nil {
		return nil, safe(e)
	}
	err = errors.Join(protectDirectory(directory, newDirectory), directory.Close())
	if err != nil {
		return nil, safe(err)
	}
	s.objects, err = r.OpenRoot("objects")
	if err != nil {
		return nil, safe(err)
	}
	s.remove = s.objects.Remove
	d, err := s.objects.Open(".")
	if err != nil {
		return nil, safe(err)
	}
	entries, readErr := d.ReadDir(options.MaxObjects + 1)
	closeErr := d.Close()
	if readErr != nil && readErr != io.EOF {
		return nil, safe(readErr)
	}
	if closeErr != nil {
		return nil, safe(closeErr)
	}
	if len(entries) > options.MaxObjects {
		return nil, fail("LIMIT_EXCEEDED", fs.ErrInvalid)
	}
	// Validate every entry before deleting any. Unknown names, links and unsafe
	// permissions require operator intervention, not a broad prefix cleanup.
	for _, entry := range entries {
		if !objectName.MatchString(entry.Name()) {
			return nil, fail("PERMISSION_DENIED", fs.ErrPermission)
		}
		f, e := s.openObject(entry.Name(), nil)
		if e != nil {
			return nil, e
		}
		if e = f.Close(); e != nil {
			return nil, safe(e)
		}
	}
	for _, entry := range entries {
		if err = s.remove(entry.Name()); err != nil {
			return nil, safe(err)
		}
	}
	return s, nil
}
func (s *Store) openObject(name string, expected os.FileInfo) (*os.File, error) {
	i, e := s.objects.Lstat(name)
	if e != nil {
		return nil, safe(e)
	}
	if !i.Mode().IsRegular() {
		return nil, fail("PERMISSION_DENIED", fs.ErrPermission)
	}
	f, e := s.objects.Open(name)
	if e != nil {
		return nil, safe(e)
	}
	actual, e := f.Stat()
	if e == nil && (!os.SameFile(i, actual) || expected != nil && !os.SameFile(expected, actual)) {
		e = fs.ErrPermission
	}
	if e == nil {
		e = protect(f, false)
	}
	if e != nil {
		_ = f.Close()
		return nil, safe(e)
	}
	return f, nil
}
func (s *Store) available() error {
	if s.closed {
		return fail("NOT_FOUND", fs.ErrClosed)
	}
	if s.poisoned {
		return fail("DEPENDENCY_UNAVAILABLE", fs.ErrPermission)
	}
	return nil
}
func (s *Store) reserveLocked(n int64) error {
	if e := s.available(); e != nil {
		return e
	}
	if n < 0 || n > s.options.QuotaBytes-s.used {
		return fail("LIMIT_EXCEEDED", nil)
	}
	s.used += n
	return nil
}

// Reservation is a common quota lease for future HTTP spools or business temps.
// The owning service must release it only after the represented bytes are gone
// or have become completed business data. This store never deletes those files.
type Reservation struct {
	store    *Store
	bytes    int64
	released bool
}

func (s *Store) Reserve(n int64) (*Reservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n < 1 {
		return nil, fail("VALIDATION_FAILED", fs.ErrInvalid)
	}
	if len(s.reservations)+len(s.stages) >= s.options.MaxObjects {
		return nil, fail("BUSY", nil)
	}
	if e := s.reserveLocked(n); e != nil {
		return nil, e
	}
	r := &Reservation{store: s, bytes: n}
	s.reservations[r] = true
	return r, nil
}
func (r *Reservation) Release() {
	s := r.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if !r.released {
		s.used -= r.bytes
		r.released = true
		delete(s.reservations, r)
	}
}
func (s *Store) Usage() int64 { s.mu.Lock(); defer s.mu.Unlock(); return s.used }
func (s *Store) closeHandles() error {
	var errs []error
	if s.journal != nil {
		errs = append(errs, s.journal.close())
	}
	if s.objects != nil {
		errs = append(errs, s.objects.Close())
	}
	if s.lock != nil {
		errs = append(errs, s.lock.Close())
	}
	if s.root != nil {
		errs = append(errs, s.root.Close())
	}
	return errors.Join(errs...)
}

// Close is shutdown after dispatch/workers have stopped. It closes owned readers
// and removes only this store's temporary objects; failed cleanup retains usage.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.closeErr
	}
	s.closed = true
	var errs []error
	for _, r := range s.requests {
		r.cancel()
		if r.stopExecution != nil {
			r.stopExecution()
		}
		r.state = "released"
	}
	for _, st := range s.stages {
		for r := range st.readers {
			errs = append(errs, r.closeLocked())
		}
		st.retired = true
		errs = append(errs, st.cleanupLocked())
	}
	errs = append(errs, s.closeHandles())
	s.closeErr = safe(errors.Join(errs...))
	return s.closeErr
}

// Stage writes at most 64KiB per call and reserves bytes before the disk write.
// Close syncs and seals; Finish verifies the caller's claims against actual bytes.
type Stage struct {
	store                            *Store
	owner                            Owner
	kind, id, path, name, mime       string
	ctx                              context.Context
	file                             *os.File
	identity                         os.FileInfo
	hash                             hash.Hash
	size                             int64
	reserved                         int64
	sealed, ready, retained, retired bool
	writeErr                         error
	readers                          map[*Reader]bool
	request                          *Request
	scope                            *OutputScope
	expires                          time.Time
}

func (s *Store) newStage(ctx context.Context, o Owner, kind, name, media string) (*Stage, error) {
	if e := s.available(); e != nil {
		return nil, e
	}
	if !o.valid() || ctx == nil {
		return nil, fail("VALIDATION_FAILED", fs.ErrInvalid)
	}
	if e := ctx.Err(); e != nil {
		return nil, safe(e)
	}
	parsed, p, e := mime.ParseMediaType(media)
	if contract.ValidateBasename(name) != nil || e != nil || parsed != media || len(p) != 0 || strings.ToLower(media) != media || !strings.Contains(media, "/") || strings.Contains(media, "*") {
		return nil, fail("VALIDATION_FAILED", fs.ErrInvalid)
	}
	if len(s.stages)+len(s.reservations) >= s.options.MaxObjects {
		return nil, fail("BUSY", nil)
	}
	b := make([]byte, 16)
	if _, e = rand.Read(b); e != nil {
		return nil, safe(e)
	}
	id := hex.EncodeToString(b)
	path := kind + "-" + id + ".spool"
	f, e := s.objects.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if e != nil {
		return nil, safe(e)
	}
	if e = protect(f, true); e != nil {
		_ = f.Close()
		if cleanup := s.remove(path); cleanup != nil {
			s.poisoned = true
		}
		return nil, safe(e)
	}
	info, e := f.Stat()
	if e != nil {
		_ = f.Close()
		if cleanup := s.remove(path); cleanup != nil {
			s.poisoned = true
		}
		return nil, safe(e)
	}
	st := &Stage{store: s, owner: o, kind: kind, id: id, path: path, name: name, mime: media, ctx: ctx, file: f, identity: info, hash: sha256.New(), readers: map[*Reader]bool{}}
	s.stages[id] = st
	return st, nil
}
func (st *Stage) Write(p []byte) (int, error) {
	s := st.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if st.retired || st.sealed || st.writeErr != nil {
		return 0, fail("CONFLICT", fs.ErrClosed)
	}
	if e := st.ctx.Err(); e != nil {
		return 0, safe(e)
	}
	if len(p) > 64*1024 {
		return 0, fail("LIMIT_EXCEEDED", nil)
	}
	n64 := int64(len(p))
	if st.request != nil {
		r := st.request
		leafMax := r.limits.MaxFileBytes
		for pointer, candidate := range r.files {
			if candidate == st {
				leafMax = min(leafMax, schemaAt(r.capability.Input, pointer).File.MaxBytes)
				break
			}
		}
		if r.state != "receiving" || n64 > leafMax-st.size || n64 > r.limits.MaxTotalFileBytes-r.bytes {
			return 0, fail("LIMIT_EXCEEDED", nil)
		}
	}
	if st.scope != nil {
		v := st.scope
		if n64 > v.limits.MaxFileBytes-st.size || n64 > v.limits.MaxTotalBytes-v.bytes {
			return 0, fail("LIMIT_EXCEEDED", nil)
		}
	}
	prepaid := min(n64, st.reserved)
	if e := s.reserveLocked(n64 - prepaid); e != nil {
		return 0, e
	}
	st.reserved -= prepaid
	n, e := s.write(st.file, p)
	if n < 0 || n > len(p) {
		n = 0
		e = io.ErrShortWrite
	}
	s.used -= n64 - int64(n)
	st.size += int64(n)
	if st.request != nil {
		st.request.bytes += int64(n)
	}
	if st.scope != nil {
		st.scope.bytes += int64(n)
	}
	_, _ = st.hash.Write(p[:n])
	if e == nil && n != len(p) {
		e = io.ErrShortWrite
	}
	if e != nil {
		st.writeErr = e
	}
	return n, safe(e)
}
func (st *Stage) Close() error {
	s := st.store
	s.mu.Lock()
	defer s.mu.Unlock()
	return st.sealLocked()
}
func (st *Stage) sealLocked() error {
	if st.retired {
		return fail("NOT_FOUND", fs.ErrClosed)
	}
	if st.sealed {
		return safe(st.writeErr)
	}
	st.sealed = true
	st.store.used -= st.reserved
	st.reserved = 0
	err := errors.Join(st.writeErr, st.ctx.Err(), st.store.syncFile(st.file), st.file.Close())
	st.file = nil
	st.writeErr = err
	return safe(err)
}
func (st *Stage) metadata() contract.ArtifactMetadata {
	return contract.ArtifactMetadata{Name: st.name, MIMEType: st.mime, SizeBytes: st.size, SHA256: hex.EncodeToString(st.hash.Sum(nil))}
}
func (st *Stage) Finish(m contract.ArtifactMetadata) (io.ReadCloser, error) {
	s := st.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if st.kind != "o" || !st.sealed || st.ready || st.retired || st.writeErr != nil || st.ctx.Err() != nil {
		return nil, fail("OUTPUT_INVALID", st.writeErr)
	}
	if m != st.metadata() {
		return nil, fail("OUTPUT_INVALID", nil)
	}
	st.ready = true
	// Completed bytes outlive the execution context. The publication owner
	// releases this initial lease; retained downloads have their own contexts.
	return st.openLocked(context.Background())
}
func (st *Stage) Abort() error {
	s := st.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := st.request; r != nil {
		// Losing one input invalidates the complete request, including ready
		// requests. Executing workers must stop before any input is retired.
		r.abortLocked()
		if r.started && r.state != "released" {
			return nil
		}
		return r.releaseStoppedLocked()
	}
	if st.retained {
		return nil
	}
	st.retired = true
	return st.cleanupLocked()
}
func (st *Stage) cleanupLocked() error {
	if !st.retired || len(st.readers) > 0 {
		return nil
	}
	if st.file != nil {
		if e := st.file.Close(); e != nil {
			s := st.store
			s.poisoned = true
			return safe(e)
		}
		st.file = nil
	}
	if _, ok := st.store.stages[st.id]; !ok {
		return nil
	}
	if e := st.store.remove(st.path); e != nil {
		st.store.poisoned = true
		return safe(e)
	}
	st.store.used -= st.size + st.reserved
	st.reserved = 0
	delete(st.store.stages, st.id)
	if st.request != nil {
		st.request.releaseSlotLocked()
	}
	return nil
}
func (st *Stage) openLocked(ctx context.Context) (*Reader, error) {
	if !st.ready || st.retired {
		return nil, fail("NOT_FOUND", nil)
	}
	if e := ctx.Err(); e != nil {
		return nil, safe(e)
	}
	if st.store.readers >= st.store.options.MaxObjects {
		return nil, fail("BUSY", nil)
	}
	f, e := st.store.openObject(st.path, st.identity)
	if e != nil {
		return nil, e
	}
	readerContext, cancel := context.WithCancel(ctx)
	r := &Reader{stage: st, file: f, ctx: readerContext, cancel: cancel}
	st.readers[r] = true
	st.store.readers++
	return r, nil
}

// Reader is a counted read-only lease. Retirement denies new readers but keeps
// bytes/quota until existing readers close; cancellation never unlinks under I/O.
type Reader struct {
	stage  *Stage
	file   *os.File
	ctx    context.Context
	cancel context.CancelFunc
	closed bool
}

func (r *Reader) Read(p []byte) (int, error) {
	s := r.stage.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.closed {
		return 0, fs.ErrClosed
	}
	if e := r.ctx.Err(); e != nil {
		return 0, safe(e)
	}
	if len(p) > 64*1024 {
		p = p[:64*1024]
	}
	n, e := r.file.Read(p)
	if e != nil && e != io.EOF {
		return n, safe(e)
	}
	return n, e
}
func (r *Reader) Close() error {
	s := r.stage.store
	s.mu.Lock()
	defer s.mu.Unlock()
	return r.closeLocked()
}
func (r *Reader) closeLocked() error {
	if r.closed {
		return nil
	}
	r.cancel()
	e := r.file.Close()
	if e != nil {
		r.stage.store.poisoned = true
		return safe(e)
	}
	r.closed = true
	delete(r.stage.readers, r)
	r.stage.store.readers--
	return r.stage.cleanupLocked()
}
