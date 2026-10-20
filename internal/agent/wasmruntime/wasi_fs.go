package wasmruntime

import (
	"encoding/json"
	es "github.com/tetratelabs/wazero/experimental/sys"
	ws "github.com/tetratelabs/wazero/sys"
	"github.com/viewlegacy/onprest/internal/agent/filesystem"
	"io"
	"io/fs"
	"os"
	"runtime"
	"strings"
	"time"
)

// wasiFS carries no OS authority. Every operation is an RPC to the same
// request-owned parent bridge used by the host ABI.
type wasiFS struct {
	es.UnimplementedFS
	rpc        func(packet) packet
	prefix     string
	allocation *rawAllocation
}

func (f *wasiFS) path(p string) string {
	p = f.prefix + strings.TrimPrefix(p, "/")
	if runtime.GOOS == "windows" && f.prefix == "/" {
		native, e := filesystem.NativeWASIPath(p)
		if e != nil {
			return "\x00"
		}
		return native
	}
	return p
}
func errno(c int32) es.Errno {
	switch c {
	case 0:
		return 0
	case invalid:
		return es.EINVAL
	case denied:
		return es.EPERM
	case notFound:
		return es.ENOENT
	case exists:
		return es.EEXIST
	case limit:
		return es.ERANGE
	case canceled:
		return es.EINTR
	case unsupported:
		return es.ENOSYS
	}
	return es.EIO
}
func (f *wasiFS) call(method string, args map[string]any) (map[string]json.RawMessage, es.Errno) {
	for _, k := range []string{"path", "old_path", "new_path"} {
		if p, ok := args[k].(string); ok && !validPath(p) {
			return f.call("files.invalid", map[string]any{})
		}
	}
	raw, e := json.Marshal(map[string]any{"method": method, "args": args})
	if e != nil {
		return nil, es.EINVAL
	}
	p := f.rpc(packet{Op: "call", Data: raw})
	if p.Count < 0 {
		return nil, errno(p.Count)
	}
	id := p.Handle
	defer f.rpc(packet{Op: "close", Handle: id})
	var data []byte
	for {
		v := f.rpc(packet{Op: "read", Handle: id, Count: Chunk})
		if v.Count < 0 {
			return nil, errno(v.Count)
		}
		if v.Count == 0 {
			break
		}
		data = append(data, v.Data...)
		if len(data) > Chunk {
			return nil, es.ERANGE
		}
	}
	var out map[string]json.RawMessage
	if json.Unmarshal(data, &out) != nil {
		return nil, es.EIO
	}
	return out, 0
}
func fromMeta(m metadata) ws.Stat_t {
	t, _ := time.Parse(time.RFC3339Nano, m.ModTime)
	return ws.Stat_t{Mode: fs.FileMode(m.Mode), Size: m.Size, Nlink: 1, Mtim: t.UnixNano(), Atim: t.UnixNano(), Ctim: t.UnixNano()}
}
func (f *wasiFS) OpenFile(p string, flag es.Oflag, perm fs.FileMode) (es.File, es.Errno) {
	if p == "." && flag == es.O_RDONLY {
		return newWASIFile(f, 0, p, metadata{Name: ".", IsDir: true, Mode: uint32(fs.ModeDir | 0555)}, false), 0
	}
	flags := 0
	switch flag & 3 {
	case es.O_RDWR:
		flags = os.O_RDWR
	case es.O_WRONLY:
		flags = os.O_WRONLY
	}
	if flag&es.O_CREAT != 0 {
		flags |= os.O_CREATE
	}
	if flag&es.O_TRUNC != 0 {
		flags |= os.O_TRUNC
	}
	if flag&es.O_APPEND != 0 {
		flags |= os.O_APPEND
	}
	if flag&es.O_EXCL != 0 {
		flags |= os.O_EXCL
	}
	v, e := f.call("files.wasi_open", map[string]any{"path": f.path(p), "flags": flags, "perm": uint32(perm)})
	if e != 0 {
		return nil, e
	}
	var id int64
	var m metadata
	json.Unmarshal(v["body_handle"], &id)
	json.Unmarshal(v["metadata"], &m)
	return newWASIFile(f, id, p, m, flag&es.O_APPEND != 0), 0
}
func (f *wasiFS) Stat(p string) (ws.Stat_t, es.Errno) {
	v, e := f.call("files.stat", map[string]any{"path": f.path(p)})
	if e != 0 {
		return ws.Stat_t{}, e
	}
	raw, _ := json.Marshal(v)
	var m metadata
	json.Unmarshal(raw, &m)
	return fromMeta(m), 0
}
func (f *wasiFS) Lstat(p string) (ws.Stat_t, es.Errno) { return f.Stat(p) }
func (f *wasiFS) Mkdir(p string, perm fs.FileMode) es.Errno {
	_, e := f.call("files.mkdir", map[string]any{"path": f.path(p), "perm": uint32(perm)})
	return e
}
func (f *wasiFS) Unlink(p string) es.Errno {
	_, e := f.call("files.remove", map[string]any{"path": f.path(p)})
	return e
}
func (f *wasiFS) Rmdir(p string) es.Errno { return f.Unlink(p) }
func (f *wasiFS) Rename(a, b string) es.Errno {
	_, e := f.call("files.rename", map[string]any{"old_path": f.path(a), "new_path": f.path(b)})
	return e
}

// foreignSeekAdapter exposes wazero's errno ABI independently of io.Seeker.
// The generic parameter makes the foreign signature explicit; instantiated
// with int64 it satisfies es.File exactly, with no error/offset conversion.
type foreignSeekAdapter[T ~int64] struct {
	es.UnimplementedFile
	delegate func(T, int) (int64, es.Errno)
}

func (a foreignSeekAdapter[T]) Seek(offset T, whence int) (int64, es.Errno) {
	return a.delegate(offset, whence)
}

type wasiFile struct {
	foreignSeekAdapter[int64]
	owner      *wasiFS
	id         int64
	path       string
	meta       metadata
	append     bool
	entries    []es.Dirent
	index      int
	closed     bool
	loaded     bool
	entryError es.Errno
}

var _ es.File = (*wasiFile)(nil)

func newWASIFile(owner *wasiFS, id int64, path string, meta metadata, appendMode bool) *wasiFile {
	f := &wasiFile{owner: owner, id: id, path: path, meta: meta, append: appendMode}
	f.foreignSeekAdapter.delegate = f.seek
	return f
}

// The synthetic preopen grants no business access during module initialization.
// Its first real operation resolves authority once and pins the descriptor.
func (f *wasiFile) ensureDescriptor() es.Errno {
	if f.closed {
		return es.EBADF
	}
	if f.id != 0 {
		return 0
	}
	v, e := f.owner.call("files.wasi_open", map[string]any{"path": f.owner.path(f.path), "flags": os.O_RDONLY})
	if e != 0 {
		return e
	}
	if json.Unmarshal(v["body_handle"], &f.id) != nil || f.id == 0 {
		return es.EIO
	}
	return 0
}
func (f *wasiFile) IsDir() (bool, es.Errno) {
	if f.closed {
		return false, es.EBADF
	}
	return f.meta.IsDir, 0
}
func (f *wasiFile) IsAppend() bool { return f.append }
func (f *wasiFile) SetAppend(v bool) es.Errno {
	if f.closed {
		return es.EBADF
	}
	if v == f.append {
		return 0
	}
	_, e := f.owner.call("files.unsupported", map[string]any{})
	return e
}
func (f *wasiFile) Stat() (ws.Stat_t, es.Errno) {
	if f.closed {
		return ws.Stat_t{}, es.EBADF
	}
	if f.owner.allocation != nil && f.owner.allocation.active {
		a := f.owner.allocation
		_, e := f.owner.call("files.allocate", map[string]any{"body_handle": f.id, "offset": a.offset, "length": a.length})
		if e != 0 {
			return ws.Stat_t{}, e
		}
		return ws.Stat_t{Size: a.offset + a.length}, 0
	}
	if e := f.ensureDescriptor(); e != 0 {
		return ws.Stat_t{}, e
	}
	v, e := f.owner.call("files.fstat", map[string]any{"body_handle": f.id})
	if e != 0 {
		return ws.Stat_t{}, e
	}
	raw, _ := json.Marshal(v)
	var m metadata
	json.Unmarshal(raw, &m)
	return fromMeta(m), 0
}
func (f *wasiFile) Read(b []byte) (int, es.Errno) {
	if e := f.ensureDescriptor(); e != 0 {
		return 0, e
	}
	if f.closed {
		return 0, es.EBADF
	}
	if len(b) > Chunk {
		b = b[:Chunk]
	}
	v := f.owner.rpc(packet{Op: "read", Handle: f.id, Count: int32(len(b))})
	if v.Count < 0 {
		return 0, errno(v.Count)
	}
	return copy(b, v.Data), 0
}
func (f *wasiFile) Write(b []byte) (int, es.Errno) {
	if e := f.ensureDescriptor(); e != 0 {
		return 0, e
	}
	if f.closed {
		return 0, es.EBADF
	}
	if len(b) > Chunk {
		b = b[:Chunk]
	}
	v := f.owner.rpc(packet{Op: "write", Handle: f.id, Data: b})
	if v.Count < 0 {
		return 0, errno(v.Count)
	}
	return int(v.Count), 0
}
func (f *wasiFile) seek(off int64, whence int) (int64, es.Errno) {
	if f.closed {
		return 0, es.EBADF
	}
	if e := f.ensureDescriptor(); e != 0 {
		return 0, e
	}
	if f.meta.IsDir {
		if off != 0 || whence != io.SeekStart {
			return 0, es.EINVAL
		}
		if _, e := f.owner.call("files.rewind", map[string]any{"body_handle": f.id}); e != 0 {
			return 0, e
		}
		f.loaded = false
		f.entries = nil
		f.index = 0
		f.entryError = 0
		return 0, 0
	}

	v, e := f.owner.call("files.seek", map[string]any{"body_handle": f.id, "offset": off, "whence": whence})
	if e != 0 {
		return 0, e
	}
	var n int64
	json.Unmarshal(v["offset"], &n)
	if f.meta.IsDir && off == 0 {
		f.loaded = false
		f.entries = nil
		f.index = 0
	}
	return n, 0
}
func (f *wasiFile) Pread(b []byte, off int64) (int, es.Errno) {
	if f.closed {
		return 0, es.EBADF
	}
	if e := f.ensureDescriptor(); e != 0 {
		return 0, e
	}
	if len(b) > Chunk {
		b = b[:Chunk]
	}
	p := f.owner.rpc(packet{Op: "pread", Handle: f.id, Offset: off, Count: int32(len(b))})
	if p.Count < 0 {
		return 0, errno(p.Count)
	}
	return copy(b, p.Data), 0
}
func (f *wasiFile) Pwrite(b []byte, off int64) (int, es.Errno) {
	if f.closed {
		return 0, es.EBADF
	}
	if e := f.ensureDescriptor(); e != 0 {
		return 0, e
	}
	if len(b) > Chunk {
		b = b[:Chunk]
	}
	p := f.owner.rpc(packet{Op: "pwrite", Handle: f.id, Offset: off, Data: b})
	if p.Count < 0 {
		return 0, errno(p.Count)
	}
	return int(p.Count), 0
}

func (f *wasiFile) Truncate(size int64) es.Errno {
	if f.closed {
		return es.EBADF
	}
	_, e := f.owner.call("files.truncate", map[string]any{"body_handle": f.id, "size": size})
	return e
}
func (f *wasiFile) Sync() es.Errno {
	if e := f.ensureDescriptor(); e != 0 {
		return e
	}
	if f.closed {
		return es.EBADF
	}
	_, e := f.owner.call("files.sync", map[string]any{"body_handle": f.id})
	return e
}
func (f *wasiFile) Datasync() es.Errno { return f.Sync() }
func (f *wasiFile) Close() es.Errno {
	if f.closed {
		return es.EBADF
	}
	f.closed = true
	f.entries = nil
	if f.id == 0 {
		return 0
	}
	return errno(f.owner.rpc(packet{Op: "close", Handle: f.id}).Count)
}
func (f *wasiFile) Readdir(n int) ([]es.Dirent, es.Errno) {
	if e := f.ensureDescriptor(); e != 0 {
		return nil, e
	}
	if f.closed {
		return nil, es.EBADF
	}
	if f.loaded {
		if _, e := f.owner.call("files.guard", map[string]any{}); e != 0 {
			return nil, e
		}
	}
	if !f.loaded {
		v, e := f.owner.call("files.readdir", map[string]any{"body_handle": f.id})
		if e != 0 {
			return nil, e
		}
		var id int64
		var c int32
		json.Unmarshal(v["body_handle"], &id)
		json.Unmarshal(v["error"], &c)
		f.entryError = errno(-c)
		var data []byte
		for {
			p := f.owner.rpc(packet{Op: "read", Handle: id, Count: Chunk})
			if p.Count < 0 {
				f.entryError = errno(p.Count)
				break
			}
			if p.Count == 0 {
				break
			}
			data = append(data, p.Data...)
		}
		f.owner.rpc(packet{Op: "close", Handle: id})
		for _, line := range strings.Split(string(data), "\n") {
			if line == "" {
				continue
			}
			var m metadata
			if json.Unmarshal([]byte(line), &m) != nil {
				return nil, es.EIO
			}
			f.entries = append(f.entries, es.Dirent{Name: m.Name, Type: fs.FileMode(m.Mode) & fs.ModeType})
		}
		f.loaded = true
	}
	end := len(f.entries)
	if n > 0 {
		end = min(end, f.index+n)
	}
	v := f.entries[f.index:end]
	f.index = end
	if f.index == len(f.entries) {
		return v, f.entryError
	}
	return v, 0
}
