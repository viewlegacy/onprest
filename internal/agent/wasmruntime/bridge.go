package wasmruntime

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/viewlegacy/onprest/internal/agent/filesystem"
	"github.com/viewlegacy/onprest/internal/agent/hostservices"
	"github.com/viewlegacy/onprest/internal/agent/providers"
	"github.com/viewlegacy/onprest/internal/contract"
)

// Bridge is private prepared authority. It never comes from request parameters.
type Bridge struct {
	Base        string
	Sandbox     map[string]any
	Environment map[string]string
	Input       map[string]contract.FileValue
	OpenInput   func(context.Context, contract.FileValue) (io.ReadCloser, error)
	Effects     *providers.Effects
}
type business struct {
	closeMu                               sync.Mutex
	closed                                bool
	interrupted                           bool
	closeErr                              error
	contextCancel                         context.CancelFunc
	file                                  *os.File
	reader                                io.ReadCloser
	size, position, read, write, reserved int64
	writable, append, knownWrite          bool
	effectObserved                        bool
	effect                                *providers.Effect
	request                               *hostservices.Request
	body                                  io.ReadCloser
}
type metadata struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	Mode    uint32 `json:"mode"`
	ModTime string `json:"mod_time"`
	IsDir   bool   `json:"is_dir"`
}

func meta(i fs.FileInfo) metadata {
	return metadata{i.Name(), i.Size(), uint32(i.Mode()), i.ModTime().UTC().Format(time.RFC3339Nano), i.IsDir()}
}
func validPath(p string) bool { return utf8.ValidString(p) && !strings.ContainsRune(p, 0) }
func (s *session) operation() int32 {
	if s.ctx.Err() != nil {
		return canceled
	}
	if s.failed || s.committed || s.asyncFailure.Load() {
		return invalid
	}
	if s.operations >= s.l.FileOperations {
		return limit
	}
	s.operations++
	return 0
}
func (s *session) reserve(h *business, n int64, write bool) (int64, int32) {
	if n < 0 {
		return 0, invalid
	}
	left := s.l.TotalReadBytes - s.readBytes
	single := s.l.ReadBytes - h.read
	if write {
		left = s.l.TotalWriteBytes - s.writeBytes
		single = s.l.WriteBytes - h.write
	}
	n = min(n, left, single)
	if n <= 0 {
		return 0, limit
	}
	if write {
		s.writeBytes += n
		h.write += n
	} else {
		s.readBytes += n
		h.read += n
	}
	return n, 0
}
func (s *session) refund(h *business, n int64, write bool) {
	if write {
		s.writeBytes -= n
		h.write -= n
	} else {
		s.readBytes -= n
		h.read -= n
	}
}
func (s *session) authorize(p string, write bool) (*filesystem.Root, string, error) {
	if !validPath(p) || p == "" {
		return nil, "", fs.ErrInvalid
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(s.bridge.Base, p)
	}
	p = filepath.Clean(p)
	if e := filesystem.ValidateHostPath(p); e != nil {
		return nil, "", e
	}
	if strings.HasPrefix(p, `\\?\`) || strings.HasPrefix(p, `\\.\`) {
		return nil, "", fs.ErrInvalid
	}
	if s.bridge.Sandbox["enabled"] != true {
		return nil, p, nil
	}
	grants := s.bridge.Sandbox["permissions"].(map[string]any)["filesystem"].([]any)
	// Most specific eligible grant wins. Every path is resolved from a pinned root.
	var best string
	for _, raw := range grants {
		g := raw.(map[string]any)
		root := g["root"].(string)
		if write && g["access"] != "read_write" {
			continue
		}
		rel, e := filepath.Rel(root, p)
		if e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && (best == "" || len(root) > len(best)) {
			best = root
		}
	}
	if best == "" {
		return nil, "", fs.ErrPermission
	}
	r := s.roots[best]
	if r == nil {
		var e error
		r, e = filesystem.OpenRoot(best)
		if e != nil {
			return nil, "", e
		}
		s.roots[best] = r
	}
	rel, _ := filepath.Rel(best, p)
	return r, filepath.ToSlash(rel), nil
}
func (s *session) open(p string, flags int, perm fs.FileMode) (*os.File, error) {
	f, _, e := s.openObserved(p, flags, perm)
	return f, e
}
func (s *session) openObserved(p string, flags int, perm fs.FileMode) (*os.File, bool, error) {
	r, n, e := s.authorize(p, flags&(os.O_RDWR|os.O_WRONLY|os.O_CREATE|os.O_TRUNC) != 0)
	if e != nil {
		return nil, false, e
	}
	if r != nil {
		return r.HostOpenObserved(n, flags, perm)
	}
	// OFF deliberately uses ordinary OS-user path semantics.
	f, mutated, e := filesystem.OpenOrdinaryObserved(n, flags&^os.O_TRUNC, perm)
	if e != nil {
		return nil, mutated, e
	}
	i, e := f.Stat()
	if e == nil && !i.Mode().IsRegular() && !i.IsDir() {
		e = fs.ErrPermission
	}
	if e == nil && flags&os.O_TRUNC != 0 {
		if i.IsDir() {
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
func (s *session) chargeMetadata(h *business, m metadata) int32 {
	if !validPath(m.Name) {
		return invalid
	}
	b, e := contract.CanonicalJSON(metadataMap(m))
	if e != nil {
		return invalid
	}
	n, c := s.reserve(h, int64(len(b)), false)
	if c != 0 {
		return c
	}
	if n != int64(len(b)) {
		s.refund(h, n, false)
		return limit
	}
	return 0
}
func metadataMap(m metadata) map[string]any {
	return map[string]any{"name": m.Name, "size": m.Size, "mode": int64(m.Mode), "mod_time": m.ModTime, "is_dir": m.IsDir}
}
func (s *session) bodyHandle(b *business, kind string) (int64, int32) {
	if s.handleCount()+2 > s.l.Handles {
		return 0, limit
	}
	id := s.id(&handle{kind: kind, business: b})
	if id == 0 {
		if s.handleCount() >= s.l.Handles {
			return 0, limit
		}
		return 0, ioError
	}
	return id, 0
}
func (s *session) businessCall(method string, a map[string]any) (int64, int32) {
	if s.bridge == nil {
		if strings.HasPrefix(method, "files.") {
			return 0, unsupported
		}
		return 0, denied
	}
	if strings.HasPrefix(method, "files.") {
		if c := s.operation(); c != 0 {
			return 0, c
		}
		return s.filesCall(method, a)
	}
	switch method {
	case "input.open":
		if c := s.operation(); c != 0 {
			return 0, c
		}
		if len(a) != 1 {
			return 0, invalid
		}
		id, ok := a["file_id"].(string)
		v, exists := s.bridge.Input[id]
		if !ok || !exists || s.bridge.OpenInput == nil {
			return 0, denied
		}
		if s.handleCount()+2 > s.l.Handles {
			return 0, limit
		}
		r, e := s.bridge.OpenInput(s.ctx, v)
		if e != nil {
			return 0, codeFor(e)
		}
		h, c := s.bodyHandle(&business{reader: r, size: v.SizeBytes}, "input")
		if c != 0 {
			r.Close()
			return 0, c
		}
		return s.control(map[string]any{"body_handle": h})
	case "http.open":
		if len(a) != 4 {
			return 0, invalid
		}
		method, ok := a["method"].(string)
		url, ok2 := a["url"].(string)
		length, ok3 := a["content_length"].(int64)
		headers, ok4 := a["headers"].(map[string]any)
		if !ok || !ok2 || !ok3 || !ok4 {
			return 0, invalid
		}
		if s.handleCount()+3 > s.l.Handles {
			return 0, limit
		}
		header := http.Header{}
		for k, v := range headers {
			items, ok := v.([]any)
			if !ok {
				return 0, invalid
			}
			for _, v := range items {
				t, ok := v.(string)
				if !ok {
					return 0, invalid
				}
				header.Add(k, t)
			}
		}
		if s.http == nil {
			options := hostservices.Options{Context: s.lifetime, Store: s.store, MaxRequestBytes: s.l.HTTPRequestBytes, MaxResponseBytes: s.l.HTTPResponseBytes, MaxRequests: s.l.Handles}
			var e error
			if s.bridge.Sandbox["enabled"] == true {
				s.http, e = hostservices.WasmON(s.bridge.Sandbox["permissions"].(map[string]any)["http"].([]any), options)
			} else {
				s.http, e = hostservices.WasmOFF(options)
			}
			if e != nil {
				return 0, codeFor(e)
			}
		}
		r, e := s.http.Open(s.ctx, method, url, header, length, s.bridge.Effects)
		if e != nil {
			return 0, codeFor(e)
		}
		b := &business{request: r}
		id := s.id(&handle{kind: "http_request", business: b})
		body := s.id(&handle{kind: "http_write", business: b})
		if id == 0 || body == 0 {
			_ = s.abortRequest(r)
			return 0, limit
		}
		return s.control(map[string]any{"request_handle": id, "body_handle": body})
	case "http.do", "http.abort":
		if len(a) != 1 {
			return 0, invalid
		}
		id, e := numberHandle(a["request_handle"])
		h := s.handles[id]
		if e != nil || h == nil || h.kind != "http_request" {
			return 0, invalid
		}
		if method == "http.abort" {
			e = s.abortRequest(h.business.request)
			if e != nil {
				return 0, codeFor(e)
			}
			return s.control(map[string]any{})
		}
		if s.handleCount()+2 > s.l.Handles {
			return 0, limit
		}
		v, e := h.business.request.Do()
		if e != nil {
			return 0, codeFor(e)
		}
		body := s.id(&handle{kind: "http_read", business: &business{request: h.business.request, body: v.Body}})
		if body == 0 {
			_ = s.abortRequest(h.business.request)
			return 0, limit
		}
		return s.control(map[string]any{"status": v.Status, "headers": v.Headers, "content_length": v.ContentLength, "body_handle": body})
	}
	return 0, unsupported
}
func (s *session) filesCall(method string, a map[string]any) (int64, int32) {
	fields := map[string]string{
		"files.open": "path", "files.read_file": "path", "files.create": "path perm", "files.write_file": "path perm size", "files.wasi_open": "path perm flags", "files.stat": "path", "files.read_dir": "path", "files.mkdir": "path perm", "files.mkdir_all": "path perm", "files.remove": "path", "files.rename": "old_path new_path", "files.seek": "body_handle offset whence", "files.truncate": "body_handle size", "files.allocate": "body_handle offset length", "files.sync": "body_handle", "files.fstat": "body_handle", "files.readdir": "body_handle", "files.rewind": "body_handle", "files.guard": "", "files.invalid": "", "files.unsupported": "",
	}
	if names, known := fields[method]; known {
		for k := range a {
			allowed := false
			for _, name := range strings.Fields(names) {
				if k == name {
					allowed = true
					break
				}
			}
			if !allowed {
				return 0, invalid
			}
		}
	}

	p, hasPath := a["path"].(string)
	switch method {
	case "files.open", "files.read_file", "files.create", "files.write_file", "files.wasi_open":
		if !hasPath {
			return 0, invalid
		}
		if s.handleCount()+2 > s.l.Handles {
			return 0, limit
		}
		flags := os.O_RDONLY
		perm := fs.FileMode(0666)
		b := &business{}
		if method == "files.create" || method == "files.write_file" {
			flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
			b.writable = true
		}
		if method == "files.wasi_open" {
			v, ok := a["flags"].(int64)
			if !ok {
				return 0, invalid
			}
			flags = int(v)
			if flags&^(os.O_RDWR|os.O_WRONLY|os.O_CREATE|os.O_TRUNC|os.O_APPEND|os.O_EXCL) != 0 {
				return 0, unsupported
			}
			b.writable = flags&(os.O_WRONLY|os.O_RDWR) != 0
		}
		if raw, ok := a["perm"]; ok {
			v, ok := raw.(int64)
			if !ok || v < 0 || v > 0777 {
				return 0, invalid
			}
			perm = fs.FileMode(v)
		}
		if method == "files.write_file" {
			b.knownWrite = true
			v, ok := a["size"].(int64)
			if !ok || v < 0 {
				return 0, invalid
			}
			if v > 0 {
				n, c := s.reserve(b, v, true)
				if c != 0 || n != v {
					if n > 0 {
						s.refund(b, n, true)
					}
					return 0, limit
				}
				b.reserved = v
			}
		}
		if !validPath(p) {
			if b.reserved > 0 {
				s.refund(b, b.reserved, true)
			}
			return 0, invalid
		}
		// Authorize before recording a possible effect or touching the filesystem.
		if _, _, e := s.authorize(p, b.writable || flags&(os.O_CREATE|os.O_TRUNC) != 0); e != nil {
			s.refund(b, b.reserved, true)
			return 0, codeFor(e)
		}
		f, mutated, e := s.openObserved(p, flags, perm)
		if s.bridge.Effects != nil && (mutated || e != nil && flags&(os.O_CREATE|os.O_TRUNC) != 0) {
			b.effect = s.bridge.Effects.Begin()
		}
		if e != nil {
			s.refund(b, b.reserved, true)
			return 0, codeFor(e)
		}
		i, e := f.Stat()
		if e != nil || !validPath(i.Name()) {
			f.Close()
			s.refund(b, b.reserved, true)
			return 0, invalid
		}
		if method != "files.wasi_open" && i.IsDir() {
			f.Close()
			s.refund(b, b.reserved, true)
			return 0, invalid
		}
		b.file = f
		b.effectObserved = mutated
		b.size = i.Size()
		if flags&os.O_APPEND != 0 {
			b.position = i.Size()
			b.append = true
		}
		h, c := s.bodyHandle(b, "fs")
		if c != 0 {
			f.Close()
			s.refund(b, b.reserved, true)
			return 0, c
		}
		return s.control(map[string]any{"body_handle": h, "metadata": meta(i)})
	case "files.stat":
		if !hasPath {
			return 0, invalid
		}
		// Metadata never opens the target: write-only files, FIFOs and devices
		// are described as the OS describes them, without reading or blocking.
		r, n, e := s.authorize(p, false)
		if e != nil {
			return 0, codeFor(e)
		}
		var i fs.FileInfo
		if r != nil {
			i, e = r.HostStat(n)
		} else {
			// OFF keeps ordinary OS-user semantics, following links like os.Stat.
			i, e = os.Stat(n)
		}
		if e != nil {
			return 0, codeFor(e)
		}
		m := meta(i)
		if c := s.chargeMetadata(&business{}, m); c != 0 {
			return 0, c
		}
		return s.control(m)
	case "files.read_dir":
		if !hasPath {
			return 0, invalid
		}
		f, e := s.open(p, os.O_RDONLY, 0)
		if e != nil {
			return 0, codeFor(e)
		}
		defer f.Close()
		return s.readDirectory(f)
	case "files.mkdir_all", "files.mkdir":
		perm, ok := a["perm"].(int64)
		if !hasPath || !ok || perm < 0 || perm > 0777 {
			return 0, invalid
		}
		r, n, e := s.authorize(p, true)
		if e != nil {
			return 0, codeFor(e)
		}
		parts := []string{n}
		if method == "files.mkdir_all" {
			parts = nil
			dir := filepath.Clean(n)
			for dir != "." && dir != string(filepath.Separator) && (r != nil || filepath.IsAbs(p) || dir != s.bridge.Base) {
				parts = append(parts, dir)
				parent := filepath.Dir(dir)
				if parent == dir {
					break
				}
				dir = parent
			}
			for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
				parts[i], parts[j] = parts[j], parts[i]
			}
		}
		for index, dir := range parts {
			if index > 0 {
				if c := s.operation(); c != 0 {
					return 0, c
				}
			}
			created := true
			if r != nil {
				e = r.HostMkdir(filepath.ToSlash(dir), fs.FileMode(perm))
			} else {
				e = os.Mkdir(dir, fs.FileMode(perm))
			}
			if errors.Is(e, fs.ErrExist) && method == "files.mkdir_all" {
				created = false
				if r != nil {
					e = r.HostInspectDirectory(filepath.ToSlash(dir))
				} else {
					e = filesystem.InspectOrdinaryDirectory(dir)
				}
			}
			if e != nil {
				return 0, codeFor(e)
			}
			if created && s.bridge.Effects != nil {
				effect := s.bridge.Effects.Begin()
				effect.Committed()
			}
		}
		return s.control(map[string]any{})
	case "files.remove":
		if !hasPath {
			return 0, invalid
		}
		r, n, e := s.authorize(p, true)
		if e != nil {
			return 0, codeFor(e)
		}
		if e = s.inspectMutation(r, n); e != nil {
			return 0, s.mutationInspectionFailure(e)
		}
		var effect *providers.Effect
		if s.bridge.Effects != nil {
			effect = s.bridge.Effects.Begin()
		}
		if r != nil {
			e = r.HostRemove(n)
		} else {
			e = os.Remove(n)
		}
		if e != nil {
			return 0, codeFor(e)
		}
		if effect != nil {
			effect.Committed()
		}
		return s.control(map[string]any{})
	case "files.rename":
		old, ok := a["old_path"].(string)
		new, ok2 := a["new_path"].(string)
		if !ok || !ok2 || !validPath(old) || !validPath(new) {
			return 0, invalid
		}
		r, n, e := s.authorize(old, true)
		if e != nil {
			return 0, codeFor(e)
		}
		t, m, e := s.authorize(new, true)
		if e != nil {
			return 0, codeFor(e)
		}
		if e = s.inspectMutation(r, n); e != nil {
			return 0, s.mutationInspectionFailure(e)
		}
		if e = s.inspectMutation(t, m); e != nil && !errors.Is(e, fs.ErrNotExist) {
			return 0, s.mutationInspectionFailure(e)
		}
		var effect *providers.Effect
		if s.bridge.Effects != nil {
			effect = s.bridge.Effects.Begin()
		}
		if r != nil {
			e = r.HostRename(n, t, m)
		} else {
			e = os.Rename(n, m)
		}
		if e != nil {
			return 0, codeFor(e)
		}
		if effect != nil {
			effect.Committed()
		}
		return s.control(map[string]any{})
	case "files.seek", "files.truncate", "files.pread", "files.pwrite", "files.sync", "files.fstat", "files.allocate", "files.readdir", "files.rewind":
		return s.fdCall(method, a)
	case "files.invalid":
		return 0, invalid
	case "files.guard":
		return s.control(map[string]any{})
	}
	return 0, unsupported
}
func (s *session) readDirectory(f *os.File) (int64, int32) {
	info, e := f.Stat()
	if e != nil {
		return 0, codeFor(e)
	}
	if !info.IsDir() {
		return 0, invalid
	}
	entries := []fs.DirEntry{}
	for int64(len(entries)) <= s.l.JSONItems {
		batch, err := f.ReadDir(1)
		if err != nil && err != io.EOF {
			return 0, codeFor(err)
		}
		entries = append(entries, batch...)
		if err == io.EOF || len(batch) == 0 {
			break
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	h := &business{}
	var raw bytes.Buffer
	c := int32(0)
	for index, v := range entries {
		if int64(index) >= s.l.JSONItems {
			c = limit
			break
		}
		if !validPath(v.Name()) {
			c = invalid
			break
		}
		if c = s.operation(); c != 0 {
			break
		}
		// Each entry is described relative to the held directory without being
		// opened, like a native directory listing; special entries keep their type.
		info, e := filesystem.HostDirectoryEntryInfo(f, v.Name())
		if e != nil {
			c = codeFor(e)
			break
		}
		m := meta(info)
		if c = s.chargeMetadata(h, m); c != 0 {
			break
		}
		data, _ := contract.CanonicalJSON(metadataMap(m))
		raw.Write(data)
		raw.WriteByte('\n')
	}
	id, errcode := s.bodyHandle(&business{reader: io.NopCloser(bytes.NewReader(raw.Bytes())), size: int64(raw.Len())}, "metadata")
	if errcode != 0 {
		return 0, errcode
	}
	return s.control(map[string]any{"body_handle": id, "error": -c})
}
func (s *session) fdCall(method string, a map[string]any) (int64, int32) {
	id, e := numberHandle(a["body_handle"])
	h := s.handles[id]
	if e != nil || h == nil || h.kind != "fs" {
		return 0, invalid
	}
	b := h.business
	switch method {
	case "files.readdir":
		return s.readDirectory(b.file)
	case "files.rewind":
		f, e := filesystem.HostRewindDirectory(b.file, s.bridge.Sandbox["enabled"] == true)
		if e != nil {
			return 0, codeFor(e)
		}
		old := b.file
		b.file = f
		b.position = 0
		if e := old.Close(); e != nil {
			return 0, codeFor(e)
		}
		return s.control(map[string]any{})
	case "files.allocate":
		offset, ok := a["offset"].(int64)
		length, ok2 := a["length"].(int64)
		if !ok || !ok2 || offset < 0 || length < 0 || offset > int64(^uint64(0)>>1)-length {
			return 0, invalid
		}
		if !b.writable {
			return 0, denied
		}
		if length == 0 {
			return s.control(map[string]any{})
		}
		need := max(length, offset+length-b.size)
		n, c := s.reserve(b, need, true)
		if c != 0 || n != need {
			if n > 0 {
				s.refund(b, n, true)
			}
			return 0, limit
		}
		if e := filesystem.HostAllocate(b.file, offset, length); e != nil {
			s.refund(b, need, true)
			return 0, codeFor(e)
		}
		if s.bridge.Effects != nil {
			effect := s.bridge.Effects.Begin()
			effect.Committed()
		}
		b.size = max(b.size, offset+length)
		return s.control(map[string]any{})
	case "files.seek":
		off, ok := a["offset"].(int64)
		whence, ok2 := a["whence"].(int64)
		if !ok || !ok2 || whence < io.SeekStart || whence > io.SeekEnd {
			return 0, invalid
		}
		base := int64(0)
		if whence == io.SeekCurrent {
			base, e = b.file.Seek(0, io.SeekCurrent)
		} else if whence == io.SeekEnd {
			var info fs.FileInfo
			info, e = b.file.Stat()
			if e == nil {
				base = info.Size()
			}
		}
		if e != nil {
			return 0, codeFor(e)
		}
		if off < -base || off > int64(^uint64(0)>>1)-base {
			return 0, invalid
		}
		v, e := b.file.Seek(base+off, io.SeekStart)
		if e != nil {
			return 0, codeFor(e)
		}
		b.position = v
		return s.control(map[string]any{"offset": v})
	case "files.fstat":
		i, e := b.file.Stat()
		if e != nil {
			return 0, codeFor(e)
		}
		m := meta(i)
		if c := s.chargeMetadata(&business{}, m); c != 0 {
			return 0, c
		}
		return s.control(m)
	case "files.sync":
		if e := b.file.Sync(); e != nil {
			return 0, codeFor(e)
		}
		return s.control(map[string]any{})
	case "files.truncate":
		size, ok := a["size"].(int64)
		if !ok || size < 0 {
			return 0, invalid
		}
		if !b.writable {
			return 0, denied
		}
		i, e := b.file.Stat()
		if e != nil {
			return 0, codeFor(e)
		}
		delta := max(int64(0), size-i.Size())
		if delta > 0 {
			n, c := s.reserve(b, delta, true)
			if c != 0 || n != delta {
				if n > 0 {
					s.refund(b, n, true)
				}
				return 0, limit
			}
		}
		var effect *providers.Effect
		if s.bridge.Effects != nil {
			effect = s.bridge.Effects.Begin()
		}
		e = b.file.Truncate(size)
		if e != nil {
			s.refund(b, delta, true)
			return 0, codeFor(e)
		}
		b.size = size
		if effect != nil {
			effect.Committed()
		}
		return s.control(map[string]any{})
	}
	return 0, unsupported
}
func (s *session) businessRead(h *handle, n int) ([]byte, int32) {
	if s.committed || s.failed {
		s.failed = true
		return nil, invalid
	}
	b := h.business
	if n == 0 {
		return nil, 0
	}
	var reader io.Reader = b.reader
	if h.kind == "http_read" {
		reader = b.body
	}
	if h.kind == "fs" {
		reader = b.file
	}
	if h.kind == "fs" || h.kind == "input" {
		if c := s.operation(); c != 0 {
			return nil, c
		}
		count, c := s.reserve(b, int64(n), false)
		if c != 0 {
			if h.kind == "fs" {
				info, e := b.file.Stat()
				if e != nil {
					return nil, codeFor(e)
				}
				b.size = info.Size()
				position, e := b.file.Seek(0, io.SeekCurrent)
				if e != nil {
					return nil, codeFor(e)
				}
				b.position = position
			}
			if b.position >= b.size {
				return nil, 0
			}
			return nil, c
		}
		n = int(count)
		raw := make([]byte, n)
		actual, e := reader.Read(raw)
		s.refund(b, int64(n-actual), false)
		b.position += int64(actual)
		if e != nil && e != io.EOF {
			return nil, codeFor(e)
		}
		return raw[:actual], int32(actual)
	}
	raw := make([]byte, n)
	actual, e := reader.Read(raw)
	if e != nil && e != io.EOF {
		return nil, codeFor(e)
	}
	return raw[:actual], int32(actual)
}
func (s *session) businessWrite(h *handle, raw []byte) int32 {
	if s.committed || s.failed {
		s.failed = true
		return invalid
	}
	b := h.business
	if len(raw) == 0 {
		return 0
	}
	if h.kind == "http_write" {
		n, e := b.request.Write(raw)
		if e != nil {
			return codeFor(e)
		}
		return int32(n)
	}
	if h.kind != "fs" || !b.writable {
		return denied
	}
	return s.writeFS(h, raw, nil)
}

func (s *session) writeFS(h *handle, raw []byte, at *int64) int32 {
	b := h.business
	if c := s.operation(); c != 0 {
		return c
	}
	position := b.position
	if at != nil {
		if b.append || *at < 0 || *at > int64(^uint64(0)>>1)-int64(len(raw)) {
			return invalid
		}
		position = *at
	}
	if len(raw) > Chunk {
		return invalid
	}
	if len(raw) == 0 {
		return 0
	}
	if !b.writable {
		return denied
	}
	current, e := b.file.Stat()
	if e != nil {
		return codeFor(e)
	}
	b.size = current.Size()
	if b.append && at == nil {
		position, e := b.file.Seek(0, io.SeekEnd)
		if e != nil {
			return codeFor(e)
		}
		b.position = position
	}
	if at == nil {
		position = b.position
	}
	if position > int64(^uint64(0)>>1)-int64(len(raw)) {
		return invalid
	}
	need := int64(len(raw))
	reserved := b.knownWrite
	if !reserved {
		need = max(need, position+int64(len(raw))-b.size)
		n, c := s.reserve(b, need, true)
		if c != 0 {
			return c
		}
		if n < need {
			if position > b.size {
				s.refund(b, n, true)
				return limit
			}
			raw = raw[:n]
			need = n
		}
	} else {
		if int64(len(raw)) > b.reserved {
			return limit
		}
		// The known body reservation covers bytes, not a hole introduced later
		// through another raw descriptor operation on the same owned handle.
		need = max(int64(0), position+int64(len(raw))-b.size-int64(len(raw)))
		if need > 0 {
			n, c := s.reserve(b, need, true)
			if c != 0 || n != need {
				if n > 0 {
					s.refund(b, n, true)
				}
				return limit
			}
		}
	}
	if b.effect == nil && s.bridge.Effects != nil {
		b.effect = s.bridge.Effects.Begin()
	}
	var n int
	if at == nil {
		n, e = b.file.Write(raw)
		b.position += int64(n)
	} else {
		n, e = b.file.WriteAt(raw, position)
	}
	position += int64(n)
	growth := int64(0)
	if n > 0 {
		b.effectObserved = true
		growth = max(int64(0), position-b.size)
		b.size = max(b.size, position)
	}
	if reserved {
		b.reserved -= int64(n)
		s.refund(b, need-max(int64(0), growth-int64(n)), true)
	} else {
		s.refund(b, need-max(int64(n), growth), true)
	}
	if e != nil {
		return codeFor(e)
	}
	return int32(n)
}
func (s *session) businessClose(h *handle) int32 {
	if h.business.contextCancel != nil && h.kind != "http_write" {
		defer h.business.contextCancel()
	}
	b := h.business
	var e error
	switch h.kind {
	case "http_write":
		e = b.request.Ready()
	case "http_request":
		e = s.abortRequest(b.request)
	case "http_read":
		e = b.closeOwned()
	case "fs":
		e = b.closeOwned()
		s.refund(b, b.reserved, true)
		b.reserved = 0
		if e == nil && b.effect != nil && b.effectObserved && !b.wasInterrupted() {
			b.effect.Committed()
		}
	default:
		if b.reader != nil {
			e = b.closeOwned()
		}
	}
	if e != nil {
		return codeFor(e)
	}
	return 0
}

// Closing a request invalidates every shared body handle. Request owns the
// spools/connection and closes them once, including a partially read response.
func (s *session) abortRequest(request *hostservices.Request) error {
	e := request.Close()
	for id, h := range s.handles {
		if h.business != nil && h.business.request == request {
			if h.business.contextCancel != nil {
				h.business.contextCancel()
			}
			s.deleteHandle(id)
		}
	}
	return e
}

// Positioned RPC is one file operation. Native ReadAt/WriteAt never change the
// owned descriptor offset or route internal seeks through the public meter.
func (s *session) positioned(p packet) packet {
	out := packet{Op: "response"}
	if p.Op == "pwrite" {
		h := s.handles[p.Handle]
		if h == nil || h.kind != "fs" {
			if c := s.operation(); c != 0 {
				out.Count = c
			} else {
				out.Count = invalid
			}
			return out
		}
		out.Count = s.writeFS(h, p.Data, &p.Offset)
		return out
	}
	if c := s.operation(); c != 0 {
		out.Count = c
		return out
	}
	h := s.handles[p.Handle]
	n := int(p.Count)
	if h == nil || h.kind != "fs" || n < 0 || n > Chunk || p.Offset < 0 || p.Offset > int64(^uint64(0)>>1)-int64(n) {
		out.Count = invalid
		return out
	}

	if n == 0 {
		return out
	}
	b := h.business
	count, c := s.reserve(b, int64(n), false)
	if c != 0 {
		info, e := b.file.Stat()
		if e != nil {
			out.Count = codeFor(e)
		} else if p.Offset < info.Size() {
			out.Count = c
		}
		return out
	}
	raw := make([]byte, int(count))
	actual, e := b.file.ReadAt(raw, p.Offset)
	s.refund(b, count-int64(actual), false)
	if e != nil && e != io.EOF {
		out.Count = codeFor(e)
		return out
	}
	out.Data = raw[:actual]
	out.Count = int32(actual)
	return out
}

// OFF keeps OS pathname semantics; ON uses the existing authorized native root
// and no-follow descriptor inspection. Both reject special objects before a
// business mutation or Effect begins. Internal inspection is one wrapper op.
func (s *session) inspectMutation(r *filesystem.Root, p string) error {
	if r != nil {
		return r.HostInspectMutation(p)
	}
	return filesystem.InspectOrdinaryMutation(p)
}
func (s *session) mutationInspectionFailure(e error) int32 {
	if !errors.Is(e, fs.ErrPermission) && s.bridge.Effects != nil {
		s.bridge.Effects.Begin()
	}
	return codeFor(e)
}

func (b *business) wasInterrupted() bool {
	b.closeMu.Lock()
	defer b.closeMu.Unlock()
	return b.interrupted
}
func (b *business) closeOwned() error {
	b.closeMu.Lock()
	defer b.closeMu.Unlock()
	return b.closeLocked()
}
func (b *business) closeLocked() error {
	if b.closed {
		return b.closeErr
	}
	b.closed = true
	if b.file != nil {
		b.closeErr = b.file.Close()
	} else if b.reader != nil {
		b.closeErr = b.reader.Close()
	} else if b.body != nil {
		b.closeErr = b.body.Close()
	}
	return b.closeErr
}
func (b *business) interrupt() {
	b.closeMu.Lock()
	defer b.closeMu.Unlock()
	b.interrupted = true
	if b.request != nil {
		_ = b.request.Close()
	} else {
		_ = b.closeLocked()
	}
}
