package artifacts

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/viewlegacy/onprest/internal/agent/filesystem"
	"github.com/viewlegacy/onprest/internal/contract"
)

const manifestLimit int64 = 1 << 20

// Updating a manifest briefly owns both the previous record and its draft.
const manifestReservation int64 = 2 * manifestLimit

var manifestName = regexp.MustCompile(`^m-[0-9a-f]{32}\.json$`)
var draftName = regexp.MustCompile(`^\.draft-[0-9a-f]{32}$`)

// Set only by package tests before opening a store.
var saveRecoveryHook func(string)

var errDraftCleanup = errors.New("cleanup manifest draft could not be removed")

type saveJournal struct {
	mu        sync.Mutex
	root      *os.Root
	directory *filesystem.Directory
	lock      *os.File
	agent     string
	active    int
	stopped   bool
	// Deterministic barriers/faults for real-process crash and cleanup tests.
	hook   func(string)
	remove func(string) error
}
type saveManifest struct {
	Version int         `json:"version"`
	Agent   string      `json:"agent"`
	Request string      `json:"request"`
	Entries []saveEntry `json:"entries"`
}
type saveEntry struct {
	Directory   string                   `json:"directory"`
	DirectoryID filesystem.Identity      `json:"directory_id"`
	Temp        string                   `json:"temp"`
	Reserved    int64                    `json:"reserved"`
	Identity    *filesystem.FileIdentity `json:"identity,omitempty"`
	Replaced    *filesystem.FileIdentity `json:"replaced,omitempty"`
}
type SaveFile struct {
	Directory *filesystem.Directory
	Name      string
	Overwrite bool
	Value     contract.FileValue
}

func randomName(prefix, suffix string) (string, error) {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		return "", e
	}
	return prefix + hex.EncodeToString(b[:]) + suffix, nil
}

func (s *Store) openSaveJournal() error {
	const namespace = "file-write-cleanup"
	if _, e := s.root.Lstat(namespace); errors.Is(e, fs.ErrNotExist) {
		s.journal = &saveJournal{}
		return nil
	} else if e != nil {
		return e
	}
	e := s.root.Mkdir(namespace, 0700)
	newDir := e == nil
	if e != nil && !errors.Is(e, fs.ErrExist) {
		return e
	}
	info, e := s.root.Lstat(namespace)
	if e != nil || !info.IsDir() {
		return fs.ErrPermission
	}
	f, e := s.root.Open(namespace)
	if e != nil {
		return e
	}
	e = errors.Join(protectDirectory(f, newDir), f.Close())
	if e != nil {
		return e
	}
	root, e := s.root.OpenRoot(namespace)
	if e != nil {
		return e
	}
	j := &saveJournal{root: root, hook: saveRecoveryHook}
	s.journal = j
	pinned, e := root.Open(".")
	if e != nil {
		return e
	}
	j.directory, e = filesystem.DirectoryFromFile(pinned, filepath.Join(s.options.Directory, namespace))
	if e != nil {
		return e
	}
	j.lock, e = j.openPrivate(".lock", true)
	if e != nil {
		return e
	}
	if info, e := j.lock.Stat(); e != nil || info.Size() != 0 {
		return fs.ErrPermission
	}
	if e = lockFile(j.lock); e != nil {
		return fail("BUSY", e)
	}
	j.remove = j.root.Remove
	identity, e := j.openPrivate("identity", false)
	if errors.Is(e, fs.ErrNotExist) {
		id, e := randomName("", "")
		if e != nil {
			return e
		}
		identity, e = j.root.OpenFile("identity", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		if e != nil {
			return e
		}
		if _, e = io.WriteString(identity, id); e == nil {
			e = identity.Sync()
		}
		e = errors.Join(e, identity.Close(), j.directory.Sync())
		if e != nil {
			return e
		}
		j.agent = id
	} else if e != nil {
		return e
	} else {
		b, e := io.ReadAll(io.LimitReader(identity, 33))
		e = errors.Join(e, identity.Close())
		if e != nil || len(b) != 32 || !regexp.MustCompile(`^[0-9a-f]{32}$`).Match(b) {
			return fs.ErrPermission
		}
		j.agent = string(b)
	}
	return s.recoverSaves()
}
func (j *saveJournal) openPrivate(name string, create bool) (*os.File, error) {
	info, e := j.root.Lstat(name)
	if e != nil && !errors.Is(e, fs.ErrNotExist) {
		return nil, e
	}
	if e == nil && !info.Mode().IsRegular() {
		return nil, fs.ErrPermission
	}
	flags := os.O_RDWR
	if create {
		flags |= os.O_CREATE
	}
	f, e := j.root.OpenFile(name, flags, 0600)
	if e != nil {
		return nil, e
	}
	actual, e := f.Stat()
	if e == nil && info != nil && !os.SameFile(info, actual) {
		e = fs.ErrPermission
	}
	if e == nil {
		e = protect(f, false)
	}
	if e != nil {
		f.Close()
		return nil, e
	}
	return f, nil
}
func (j *saveJournal) close() error {
	var a, b, c error
	if j.lock != nil {
		a = j.lock.Close()
	}
	if j.root != nil {
		b = j.root.Close()
	}
	if j.directory != nil {
		c = j.directory.Close()
	}
	return errors.Join(a, b, c)
}
func (j *saveJournal) event(name string) {
	if j.hook != nil {
		j.hook(name)
	}
}
func (j *saveJournal) persist(name string, m saveManifest) (err error) {
	b, e := json.Marshal(m)
	if e != nil {
		return e
	}
	if int64(len(b)) > manifestLimit {
		return fail("LIMIT_EXCEEDED", nil)
	}
	draft := ".draft-" + strings.TrimSuffix(strings.TrimPrefix(name, "m-"), ".json")
	f, e := j.root.OpenFile(draft, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	defer func() {
		if e := j.root.Remove(draft); e != nil && !errors.Is(e, fs.ErrNotExist) {
			err = errors.Join(err, errDraftCleanup, e)
		}
	}()
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	e = errors.Join(e, f.Close())
	if e != nil {
		return e
	}
	j.event("manifest_draft")
	if e = j.root.Rename(draft, name); e != nil {
		return e
	}
	return j.directory.Sync()
}

// Save atomically publishes each file independently. It never deletes a
// completed business name, retries publication, or rolls back prior files.
// installed observes the first potentially effectful native publication.
func (s *Store) Save(ctx context.Context, request string, files []SaveFile, open func(context.Context, contract.FileValue) (io.ReadCloser, error), installed func(bool)) (err error) {
	defer func() {
		var a *Error
		var f *filesystem.Error
		switch {
		case errors.As(err, &a):
			err = safe(err)
		case errors.As(err, &f):
			err = fail(f.Code, err)
		case errors.Is(err, fs.ErrPermission):
			err = fail("PERMISSION_DENIED", err)
		case errors.Is(err, fs.ErrNotExist):
			err = fail("NOT_FOUND", err)
		case errors.Is(err, fs.ErrExist):
			err = fail("CONFLICT", err)
		default:
			err = safe(err)
		}
	}()
	s.saveInit.Lock()
	if s.journal != nil {
		s.journal.mu.Lock()
		stopped := s.journal.stopped
		s.journal.mu.Unlock()
		if stopped {
			s.saveInit.Unlock()
			return fail("DEPENDENCY_UNAVAILABLE", nil)
		}
	}
	if s.journal != nil && s.journal.root == nil {
		e := s.root.Mkdir("file-write-cleanup", 0700)
		if e == nil {
			parentFile, pe := s.root.Open(".")
			if pe == nil {
				parent, ce := filesystem.DirectoryFromFile(parentFile, s.options.Directory)
				pe = ce
				if pe == nil {
					pe = errors.Join(parent.Sync(), parent.Close())
				}
			}
			e = pe
		}
		if e == nil || errors.Is(e, fs.ErrExist) {
			e = s.openSaveJournal()
		}
		if e != nil {
			if s.journal != nil {
				s.journal.stopped = true
			}
			s.saveInit.Unlock()
			return safe(e)
		}
	}
	s.saveInit.Unlock()
	j := s.journal
	if j == nil || open == nil || len(files) == 0 || request == "" || len(request) > 256 || !utf8.ValidString(request) || strings.ContainsRune(request, 0) {
		return fail("VALIDATION_FAILED", fs.ErrInvalid)
	}
	j.mu.Lock()
	if j.stopped {
		j.mu.Unlock()
		return fail("DEPENDENCY_UNAVAILABLE", nil)
	}
	if j.active >= s.options.MaxPending {
		j.mu.Unlock()
		return fail("BUSY", nil)
	}
	j.active++
	j.mu.Unlock()
	slot := true
	defer func() {
		if slot {
			j.mu.Lock()
			j.active--
			j.mu.Unlock()
		}
	}()
	m := saveManifest{Version: 1, Agent: j.agent, Request: request, Entries: []saveEntry{}}
	total := manifestReservation
	seen := map[string]bool{}
	// Inspect every destination and reserve all bytes before any business temp.
	for _, v := range files {
		if e := ctx.Err(); e != nil {
			return e
		}
		if v.Directory == nil || v.Value.SizeBytes < 0 {
			return fail("VALIDATION_FAILED", fs.ErrInvalid)
		}
		if e := filesystem.SaveName(v.Name); e != nil {
			return e
		}
		key := fmt.Sprint(v.Directory.Identity()) + "/" + filesystem.CollisionKey(v.Directory, v.Name)
		if seen[key] {
			return fail("CONFLICT", nil)
		}
		seen[key] = true
		var old *filesystem.FileIdentity
		value, e := v.Directory.Inspect(v.Name, false)
		if e == nil {
			if !v.Overwrite {
				return fail("CONFLICT", nil)
			}
			old = &value
		} else if !errors.Is(e, fs.ErrNotExist) {
			return e
		}
		temp, e := randomName(".onprest-", ".tmp")
		if e != nil {
			return e
		}
		n := v.Value.SizeBytes
		if old != nil && old.Size > n {
			n = old.Size
		}
		if n > math.MaxInt64-total {
			return fail("LIMIT_EXCEEDED", nil)
		}
		total += n
		m.Entries = append(m.Entries, saveEntry{Directory: v.Directory.Path(), DirectoryID: v.Directory.Identity(), Temp: temp, Reserved: n, Replaced: old})
	}
	reservation, e := s.Reserve(total)
	if e != nil {
		return e
	}
	name, e := randomName("m-", ".json")
	if e != nil {
		reservation.Release()
		return e
	}
	durable := false
	defer func() {
		if !durable {
			reservation.Release()
			return
		}
		cleanup := func() error {
			for i, v := range m.Entries {
				ids := []filesystem.FileIdentity{}
				if v.Identity != nil {
					ids = append(ids, *v.Identity)
				}
				if v.Identity != nil && v.Replaced != nil {
					ids = append(ids, *v.Replaced)
				}
				if e := files[i].Directory.RemoveTemp(v.Temp, ids, v.Identity == nil); e != nil {
					return e
				}
				if e := files[i].Directory.Sync(); e != nil {
					return e
				}
				j.event("cleanup")
			}
			if errors.Is(err, errDraftCleanup) {
				return errDraftCleanup
			}
			if e := j.remove(name); e != nil && !errors.Is(e, fs.ErrNotExist) {
				return e
			}
			return j.directory.Sync()
		}()
		if cleanup != nil {
			// A native exchange can expose an independently changed old file under
			// the recorded temp name. Charge measured growth, retain the record, and
			// poison shared admission when ownership/cleanup remains uncertain.
			actualTotal := manifestReservation
			for i, v := range m.Entries {
				n := v.Reserved
				if actual, e := files[i].Directory.Inspect(v.Temp, false); e == nil && actual.Size > n {
					n = actual.Size
				}
				if n > math.MaxInt64-actualTotal {
					actualTotal = math.MaxInt64
					break
				}
				actualTotal += n
			}
			s.mu.Lock()
			if actualTotal > reservation.bytes && actualTotal-reservation.bytes <= math.MaxInt64-s.used {
				s.used += actualTotal - reservation.bytes
				reservation.bytes = actualTotal
			}
			s.poisoned = true
			s.mu.Unlock()
			j.mu.Lock()
			j.stopped = true
			j.mu.Unlock()
			slot = false
			err = errors.Join(err, fail("EXECUTION_FAILED", cleanup))
			return
		}
		reservation.Release()
	}()
	j.event("manifest_before")
	// A failed directory sync is ambiguous: retain reservation and attempt exact
	// cleanup even if persist reported failure after its atomic installation.
	durable = true
	if e = j.persist(name, m); e != nil {
		return e
	}
	j.event("manifest")
	buffer := make([]byte, 64*1024)
	for i, v := range files {
		if e := ctx.Err(); e != nil {
			return e
		}
		f, id, e := v.Directory.CreateTemp(m.Entries[i].Temp)
		if e != nil {
			return e
		}
		j.event("create")
		m.Entries[i].Identity = &id
		e = j.persist(name, m)
		if e != nil {
			f.Close()
			return e
		}
		j.event("identity")
		r, e := open(ctx, v.Value)
		if e != nil {
			f.Close()
			return e
		}
		h := sha256.New()
		var copied int64
		copyErr := func() error {
			for {
				if e := ctx.Err(); e != nil {
					return e
				}
				n, re := r.Read(buffer)
				if n > 0 {
					if int64(n) > v.Value.SizeBytes-copied {
						return fail("VALIDATION_FAILED", nil)
					}
					wn, we := s.write(f, buffer[:n])
					if wn < 0 || wn > n {
						return io.ErrShortWrite
					}
					copied += int64(wn)
					_, _ = h.Write(buffer[:wn])
					if we != nil {
						return we
					}
					if wn != n {
						return io.ErrShortWrite
					}
					j.event("copy")
				}
				if re == io.EOF {
					break
				}
				if re != nil {
					return re
				}
				if n == 0 {
					return io.ErrNoProgress
				}
			}
			if copied != v.Value.SizeBytes || hex.EncodeToString(h.Sum(nil)) != v.Value.SHA256 {
				return fail("VALIDATION_FAILED", nil)
			}
			return s.syncFile(f)
		}()
		e = errors.Join(copyErr, r.Close(), f.Close())
		if e != nil {
			return e
		}
	}
	for i, v := range files {
		if e := ctx.Err(); e != nil {
			return e
		}
		temporary := *m.Entries[i].Identity
		temporary.Size = v.Value.SizeBytes
		did, e := v.Directory.Publish(ctx, m.Entries[i].Temp, v.Name, v.Overwrite, temporary, m.Entries[i].Replaced)
		if did && installed != nil {
			installed(e == nil)
		}
		if e != nil {
			return e
		}
		j.event("rename")
	}
	j.event("manifest_delete_before")
	return nil
}

func (s *Store) recoverSaves() (err error) {
	var diagnosticManifest, diagnosticDirectory, diagnosticTemp string
	defer func() {
		if err != nil {
			err = fmt.Errorf("file cleanup recovery manifest %q directory %q temp %q: %w", diagnosticManifest, diagnosticDirectory, diagnosticTemp, err)
		}
	}()
	j := s.journal
	dir, e := j.root.Open(".")
	if e != nil {
		return e
	}
	entries, re := dir.ReadDir(s.options.MaxPending*2 + 3)
	closeErr := dir.Close()
	if re == io.EOF {
		re = nil
	}
	e = errors.Join(closeErr, re)
	if e != nil {
		return e
	}
	if len(entries) > s.options.MaxPending*2+2 {
		return fail("LIMIT_EXCEEDED", nil)
	}
	type pending struct {
		name        string
		m           saveManifest
		dirs        []*filesystem.Directory
		reservation *Reservation
	}
	saves := []pending{}
	drafts := []string{}
	manifestIDs := map[string]bool{}
	allTemps := map[string]bool{}
	requests := map[string]bool{}
	defer func() {
		for _, p := range saves {
			for _, d := range p.dirs {
				_ = d.Close()
			}
		}
	}()
	// Validate the complete bounded set before deleting any entry.
	for _, entry := range entries {
		n := entry.Name()
		diagnosticManifest = n
		diagnosticDirectory = ""
		diagnosticTemp = ""
		if n == ".lock" || n == "identity" {
			continue
		}
		if !manifestName.MatchString(n) && !draftName.MatchString(n) {
			return fs.ErrPermission
		}
		f, e := j.openPrivate(n, false)
		if e != nil {
			return e
		}
		info, e := f.Stat()
		if e != nil || info.Size() > manifestLimit {
			f.Close()
			return fail("LIMIT_EXCEEDED", e)
		}
		b, e := io.ReadAll(io.LimitReader(f, manifestLimit+1))
		e = errors.Join(e, f.Close())
		if e != nil {
			return e
		}
		if draftName.MatchString(n) {
			drafts = append(drafts, n)
			continue
		}
		if len(saves) >= s.options.MaxPending {
			return fail("LIMIT_EXCEEDED", nil)
		}
		var m saveManifest
		decoder := json.NewDecoder(strings.NewReader(string(b)))
		decoder.DisallowUnknownFields()
		if e = decoder.Decode(&m); e != nil {
			return e
		}
		if decoder.Decode(new(any)) != io.EOF {
			return fs.ErrInvalid
		}
		if _, e = contract.DecodeJSON(b, contract.Limits{MaxBytes: manifestLimit, MaxDepth: 32, MaxItems: 100000}); e != nil {
			return e
		}
		if m.Version != 1 || m.Agent != j.agent || m.Request == "" || len(m.Request) > 256 || !utf8.ValidString(m.Request) || strings.ContainsRune(m.Request, 0) || len(m.Entries) == 0 || len(m.Entries) > 65536 {
			return fs.ErrPermission
		}
		if requests[m.Request] {
			return fs.ErrPermission
		}
		requests[m.Request] = true
		manifestIDs[strings.TrimSuffix(strings.TrimPrefix(n, "m-"), ".json")] = true
		p := pending{name: n, m: m}
		saves = append(saves, p)
		idx := len(saves) - 1
		total := manifestReservation
		for _, v := range m.Entries {
			diagnosticDirectory = v.Directory
			diagnosticTemp = v.Temp
			if !filepath.IsAbs(v.Directory) || v.Reserved < 0 || v.Reserved > math.MaxInt64-total {
				return fs.ErrInvalid
			}
			if !filesystem.ValidTempName(v.Temp) {
				return fs.ErrPermission
			}
			if v.Identity != nil && (v.Identity.Owner == "" || v.Identity.Size < 0) || v.Replaced != nil && (v.Replaced.Owner == "" || v.Replaced.Size < 0 || v.Replaced.Size > v.Reserved) {
				return fs.ErrInvalid
			}
			d, e := filesystem.OpenDirectory(v.Directory)
			if e != nil {
				return e
			}
			saves[idx].dirs = append(saves[idx].dirs, d)
			if d.Identity() != v.DirectoryID {
				return fs.ErrPermission
			}
			key := fmt.Sprint(v.DirectoryID) + "/" + v.Temp
			if allTemps[key] {
				return fs.ErrPermission
			}
			allTemps[key] = true
			current, e := d.Inspect(v.Temp, v.Identity == nil)
			if e != nil && !errors.Is(e, fs.ErrNotExist) {
				return e
			}
			if e == nil {
				if v.Identity == nil {
					if current.Size != 0 {
						return fs.ErrPermission
					}
				} else if current.Identity == v.Identity.Identity {
					if current.Owner != v.Identity.Owner {
						return fs.ErrPermission
					}
					if _, e := d.Inspect(v.Temp, true); e != nil {
						return e
					}
				} else if v.Replaced == nil || !current.MatchesDisplaced(*v.Replaced) {
					return fs.ErrPermission
				}
				if current.Size > v.Reserved {
					v.Reserved = current.Size
				}
			}
			if v.Reserved > math.MaxInt64-total {
				return fail("LIMIT_EXCEEDED", nil)
			}
			total += v.Reserved
		}
		reservation, e := s.Reserve(total)
		if e != nil {
			return e
		}
		saves[idx].reservation = reservation
	}

	orphanDrafts := 0
	var draftReservations []*Reservation
	for _, name := range drafts {
		if !manifestIDs[strings.TrimPrefix(name, ".draft-")] {
			orphanDrafts++
			if len(saves)+orphanDrafts > s.options.MaxPending {
				return fail("LIMIT_EXCEEDED", nil)
			}
			r, e := s.Reserve(manifestReservation)
			if e != nil {
				return e
			}
			draftReservations = append(draftReservations, r)
		}
	}
	for _, p := range saves {
		for i, v := range p.m.Entries {
			diagnosticManifest = p.name
			diagnosticDirectory = v.Directory
			diagnosticTemp = v.Temp
			ids := []filesystem.FileIdentity{}
			if v.Identity != nil {
				ids = append(ids, *v.Identity)
			}
			if v.Identity != nil && v.Replaced != nil {
				ids = append(ids, *v.Replaced)
			}
			if e := p.dirs[i].RemoveTemp(v.Temp, ids, v.Identity == nil); e != nil {
				return e
			}
			if e := p.dirs[i].Sync(); e != nil {
				return e
			}
			j.event("recovery_cleanup")
		}
		if e := j.remove(p.name); e != nil {
			return e
		}
		if e := j.directory.Sync(); e != nil {
			return e
		}
	}
	for _, name := range drafts {
		if e := j.remove(name); e != nil {
			return e
		}
	}
	if e := j.directory.Sync(); e != nil {
		return e
	}
	for _, p := range saves {
		p.reservation.Release()
	}
	for _, r := range draftReservations {
		r.Release()
	}
	return nil
}
