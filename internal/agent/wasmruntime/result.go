package wasmruntime

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/viewlegacy/onprest/internal/agent/filesystem"
	"github.com/viewlegacy/onprest/internal/agent/hostservices"
	"github.com/viewlegacy/onprest/internal/agent/providers"
	"hash"
	"io"
	"io/fs"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/viewlegacy/onprest/internal/agent/artifacts"
	"github.com/viewlegacy/onprest/internal/contract"
)

const (
	invalid     = int32(-1)
	denied      = int32(-2)
	limit       = int32(-4)
	canceled    = int32(-5)
	ioError     = int32(-6)
	unsupported = int32(-7)
	notFound    = int32(-3)
	exists      = int32(-8)
)

type Error struct{ Code, Message, Detail string }

func (e *Error) Error() string           { return "Wasm execution failed: " + e.Code }
func failure(code, detail string) *Error { return &Error{Code: code, Detail: detail} }

type handle struct {
	business *business
	kind     string
	control  *bytes.Reader
	stage    contract.ArtifactStage
	reader   io.ReadCloser
	marker   int64
	ready    bool
	retained bool
	size     int64
	digest   hash.Hash
	metadata contract.ArtifactMetadata
}
type session struct {
	mu                                sync.Mutex
	ctx                               context.Context
	store                             *artifacts.Store
	l                                 Limits
	handles                           map[int64]*handle
	stages                            []*handle
	result                            int64
	committed, failed                 bool
	files                             int
	fileBytes                         int64
	value                             contract.ProviderResult
	terminalError                     error
	resultFailure                     error
	createStage                       func(context.Context, string, string, int64) (contract.ArtifactStage, error)
	logs                              *diagnostics
	bridge                            *Bridge
	async                             asyncState
	handleSlots                       atomic.Int64
	asyncFailure, terminal            atomic.Bool
	lifetime                          context.Context
	roots                             map[string]*filesystem.Root
	http                              *hostservices.Service
	readBytes, writeBytes, operations int64
}

func newSession(ctx context.Context, s *artifacts.Store, l Limits, d *diagnostics) *session {
	return &session{ctx: ctx, lifetime: ctx, store: s, l: l, handles: map[int64]*handle{}, logs: d, createStage: func(ctx context.Context, name, media string, size int64) (contract.ArtifactStage, error) {
		return s.NewScratch(ctx, name, media, size)
	}}
}
func (s *session) id(h *handle) int64 {
	for {
		var b [8]byte
		if _, e := rand.Read(b[:]); e != nil {
			return 0
		}
		id := int64(binary.LittleEndian.Uint64(b[:]) & ((1 << 62) - 1))
		if id > 0 && s.handles[id] == nil {
			if !s.reserveHandle() {
				return 0
			}
			s.handles[id] = h
			return id
		}
	}
}
func (s *session) control(v any) (int64, int32) {
	if s.handleCount() >= s.l.Handles {
		return 0, limit
	}
	b, e := json.Marshal(v)
	if e != nil || len(b) > Chunk {
		return 0, invalid
	}
	id := s.id(&handle{kind: "control", control: bytes.NewReader(b)})
	if id == 0 {
		if s.handleCount() >= s.l.Handles {
			return 0, limit
		}
		return 0, ioError
	}
	return id, 0
}
func codeFor(err error) int32 {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return canceled
	}
	var a *artifacts.Error
	if errors.As(err, &a) {
		switch a.Code {
		case "LIMIT_EXCEEDED", "BUSY":
			return limit
		case "VALIDATION_FAILED":
			return invalid
		}
	}
	var p *providers.Failure
	if errors.As(err, &p) {
		switch p.Code {
		case "PERMISSION_DENIED":
			return denied
		case "LIMIT_EXCEEDED", "BUSY":
			return limit
		case "VALIDATION_FAILED":
			return invalid
		case "EXECUTION_TIMEOUT":
			return canceled
		}
	}
	if errors.Is(err, fs.ErrInvalid) || errors.Is(err, syscall.EINVAL) {
		return invalid
	}
	if errors.Is(err, fs.ErrPermission) {
		return denied
	}
	if errors.Is(err, fs.ErrNotExist) {
		return notFound
	}
	if errors.Is(err, fs.ErrExist) {
		return exists
	}
	if errors.Is(err, errors.ErrUnsupported) {
		return unsupported
	}
	return ioError
}

// Preserve final result transport failures independently of envelope validation.
// Worker failure and execution timeout still take precedence in Execute.
func (s *session) resultFailed(code int32, err error) {
	if s.resultFailure != nil {
		return
	}
	name := "OUTPUT_INVALID"
	if code == limit {
		name = "LIMIT_EXCEEDED"
	} else if code == canceled {
		name = "EXECUTION_TIMEOUT"
	}
	detail := "result transport failed"
	if err != nil {
		detail = err.Error()
	}
	s.resultFailure = failure(name, detail)
}
func (s *session) reject() (int64, int32) { s.failed = true; return 0, invalid }
func (s *session) call(b []byte) (int64, int32) {
	if len(b) > Chunk {
		return 0, limit
	}
	v, e := contract.DecodeJSON(b, contract.Limits{MaxBytes: Chunk, MaxDepth: 8, MaxItems: 1024})
	if e != nil {
		return 0, invalid
	}
	obj, ok := v.(map[string]any)
	if !ok || len(obj) != 2 {
		return s.reject()
	}
	method, ok := obj["method"].(string)
	args, ok2 := obj["args"].(map[string]any)
	if !ok || !ok2 {
		return s.reject()
	}
	if method == "log.write" {
		if len(args) != 2 {
			return 0, invalid
		}
		level, ok := args["level"].(string)
		message, ok2 := args["message"].(string)
		if !ok || !ok2 {
			return 0, invalid
		}
		s.logs.log(level, message)
		return s.control(map[string]any{})
	}
	if s.failed || s.committed || s.asyncFailure.Load() {
		return s.reject()
	}
	switch method {
	case "result.file", "result.open":
		if s.handleCount()+3 > s.l.Handles {
			if method == "result.open" {
				s.resultFailed(limit, nil)
			}
			return 0, limit
		}
		name, media := "result.json", "application/json"
		kind := "result"
		if method == "result.open" {
			if len(args) != 0 || s.result != 0 {
				return s.reject()
			}
		} else {
			if len(args) != 2 {
				return s.reject()
			}
			name, ok = args["name"].(string)
			media, ok2 = args["mime_type"].(string)
			if !ok || !ok2 {
				return s.reject()
			}
			kind = "file"
			if s.files >= s.l.Files {
				return 0, limit
			}
		}
		st, e := s.createStage(s.ctx, name, media, 0)
		if e != nil {
			if kind == "result" {
				s.resultFailed(codeFor(e), e)
			}
			return 0, codeFor(e)
		}
		h := &handle{kind: kind, stage: st, digest: sha256.New(), metadata: contract.ArtifactMetadata{Name: name, MIMEType: media}}
		marker := s.id(h)
		if marker == 0 {
			code := ioError
			if s.handleCount() >= s.l.Handles {
				code = limit
			}
			if kind == "result" {
				s.resultFailed(code, nil)
			}
			_ = st.Abort()
			return 0, code
		}
		body := s.id(&handle{kind: "body", marker: marker})
		if body == 0 {
			code := ioError
			if s.handleCount() >= s.l.Handles {
				code = limit
			}
			if kind == "result" {
				s.resultFailed(code, nil)
			}
			s.deleteHandle(marker)
			_ = st.Abort()
			return 0, code
		}
		s.stages = append(s.stages, h)
		if kind == "result" {
			s.result = marker
		} else {
			s.files++
		}
		key := "file_handle"
		if kind == "result" {
			key = "result_handle"
		}
		ack, code := s.control(map[string]any{key: marker, "body_handle": body})
		if kind == "result" && code != 0 {
			s.resultFailed(code, nil)
		}
		return ack, code
	case "result.commit":
		if s.asyncJobs() != 0 {
			return s.reject()
		}
		if len(args) != 1 {
			return s.reject()
		}
		id, e := numberHandle(args["result_handle"])
		h := s.handles[id]
		if e != nil || id != s.result || h == nil || h.kind != "result" || !h.ready {
			return s.reject()
		}
		ack, code := s.control(map[string]any{})
		if code != 0 {
			s.resultFailed(code, nil)
			s.failed = true
			return 0, code
		}
		if e = s.envelope(h); e != nil {
			s.deleteHandle(ack)
			s.failed = true
			s.terminalError = e
			return 0, invalid
		}
		s.committed = true
		s.terminal.Store(true)
		return ack, 0
	case "input.open", "http.open", "http.do", "http.abort":
		return s.businessCall(method, args)
	default:
		if strings.HasPrefix(method, "files.") {
			return s.businessCall(method, args)
		}
		return 0, unsupported
	}
}
func numberHandle(v any) (int64, error) {
	var id int64
	var err error
	switch n := v.(type) {
	case int64:
		id = n
	case json.Number:
		id, err = n.Int64()
	default:
		err = errors.New("invalid handle")
	}
	if err != nil || id <= 0 {
		return 0, errors.New("invalid handle")
	}
	return id, nil
}

func (s *session) write(id int64, b []byte) int32 {
	h := s.handles[id]
	if h != nil && h.business != nil {
		return s.businessWrite(h, b)
	}
	if s.failed || s.committed || s.asyncFailure.Load() {
		s.failed = true
		return invalid
	}
	if h == nil || h.kind != "body" || len(b) > Chunk {
		return invalid
	}
	marker := s.handles[h.marker]
	if marker == nil || marker.ready {
		return invalid
	}
	max := int64(EnvelopeLimit)
	if marker.kind == "file" {
		max = s.l.FileBytes
		if int64(len(b)) > s.l.TotalFileBytes-s.fileBytes {
			return limit
		}
	}
	if int64(len(b)) > max-marker.size {
		if marker.kind == "result" {
			s.resultFailed(limit, nil)
		}
		return limit
	}
	n, e := marker.stage.Write(b)
	marker.size += int64(n)
	_, _ = marker.digest.Write(b[:n])
	if marker.kind == "file" {
		s.fileBytes += int64(n)
	}
	if e != nil {
		code := codeFor(e)
		if marker.kind == "result" || code != limit {
			s.resultFailed(code, e)
		}
		if code != limit {
			s.failed = true
			s.terminalError = failure("EXECUTION_FAILED", e.Error())
		}
		return code
	}
	return int32(n)
}
func (s *session) read(id int64, n int) ([]byte, int32) {
	h := s.handles[id]
	if n < 0 || n > Chunk {
		return nil, invalid
	}
	if h != nil && h.business != nil {
		return s.businessRead(h, n)
	}
	if h == nil || h.kind != "control" || n < 0 || n > Chunk {
		return nil, invalid
	}
	b := make([]byte, n)
	count, e := h.control.Read(b)
	if e != nil && e != io.EOF {
		return nil, ioError
	}
	return b[:count], int32(count)
}
func (s *session) close(id int64) int32 {
	h := s.handles[id]
	if h == nil {
		return invalid
	}
	if h.business != nil {
		c := s.businessClose(h)
		s.deleteHandle(id)
		return c
	}
	if h.kind == "body" {
		marker := s.handles[h.marker]
		if marker == nil || marker.ready {
			return invalid
		}
		if e := marker.stage.Close(); e != nil {
			s.resultFailed(codeFor(e), e)
			s.failed = true
			s.terminalError = failure("EXECUTION_FAILED", e.Error())
			return codeFor(e)
		}
		marker.metadata.SizeBytes = marker.size
		marker.metadata.SHA256 = hex.EncodeToString(marker.digest.Sum(nil))
		r, e := marker.stage.Finish(marker.metadata)
		if e != nil {
			s.resultFailed(codeFor(e), e)
			s.failed = true
			return codeFor(e)
		}
		marker.reader = r
		marker.ready = true
	}
	if h.kind == "file" || h.kind == "result" {
		if h.kind == "result" && !s.committed {
			s.failed = true
		}
		// Marker close is cleanup: invalidate the reference and release its private
		// spool, including after a caught quota error. Limits themselves never reset.
		if s.committed && h.retained {
			s.deleteHandle(id)
			return 0
		}
		var err error
		if h.reader != nil {
			err = h.reader.Close()
		}
		err = errors.Join(err, h.stage.Abort())
		s.deleteHandle(id)
		if err != nil {
			s.failed = true
			return codeFor(err)
		}
		return 0
	}
	s.deleteHandle(id)
	return 0
}
func (s *session) envelope(h *handle) error {
	b, e := io.ReadAll(io.LimitReader(h.reader, EnvelopeLimit+1))
	if e != nil {
		return failure("OUTPUT_INVALID", e.Error())
	}
	v, e := contract.DecodeJSON(b, contract.Limits{MaxBytes: EnvelopeLimit, MaxDepth: s.l.JSONDepth + 4, MaxItems: s.l.JSONItems + int64(s.l.Files)*3 + 8})
	if e != nil {
		return failure("OUTPUT_INVALID", e.Error())
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return failure("OUTPUT_INVALID", "terminal envelope must be object")
	}
	if raw, exists := obj["error"]; exists {
		if len(obj) != 1 {
			return failure("OUTPUT_INVALID", "mixed terminal envelope")
		}
		err, ok := raw.(map[string]any)
		if !ok {
			return failure("OUTPUT_INVALID", "invalid terminal error")
		}
		switch err["kind"] {
		case "internal":
			if len(err) != 2 {
				return failure("OUTPUT_INVALID", "invalid internal error")
			}
			code := ""
			switch err["category"] {
			case "execution":
				code = "EXECUTION_FAILED"
			case "output":
				code = "OUTPUT_INVALID"
			case "limit":
				code = "LIMIT_EXCEEDED"
			default:
				return failure("OUTPUT_INVALID", "unknown internal category")
			}
			s.terminalError = failure(code, "")
			return nil
		case "public":
			code, _ := err["code"].(string)
			message, _ := err["message"].(string)
			if len(err) != 3 || !stringsHasAPP(code) || (contract.ErrorValue{Code: code, Message: message, Outcome: "unknown"}).Validate() != nil {
				return failure("EXECUTION_FAILED", "invalid public error")
			}
			s.terminalError = &Error{Code: code, Message: message}
			return nil
		default:
			return failure("OUTPUT_INVALID", "unknown error kind")
		}
	}
	data, exists := obj["data"]
	raw, ok := obj["files"].([]any)
	if len(obj) != 2 || !exists || !ok || len(raw) > s.l.Files {
		return failure("OUTPUT_INVALID", "invalid success envelope")
	}
	if !dataWithinLimits(data, s.l.JSONDepth, s.l.JSONItems) {
		return failure("OUTPUT_INVALID", "raw data structural limit exceeded")
	}
	seen := map[int64]bool{}
	files := []contract.ProviderFile{}
	for _, v := range raw {
		ref, ok := v.(map[string]any)
		if !ok || len(ref) != 1 {
			return failure("OUTPUT_INVALID", "invalid file reference")
		}
		id, e := numberHandle(ref["handle"])
		f := s.handles[id]
		if e != nil || seen[id] || f == nil || f.kind != "file" || !f.ready {
			return failure("OUTPUT_INVALID", "unready or foreign output handle")
		}
		seen[id] = true
		f.retained = true
		size := f.size
		files = append(files, contract.ProviderFile{Name: f.metadata.Name, MIMEType: f.metadata.MIMEType, Reader: f.reader, SizeBytes: &size, SHA256: f.metadata.SHA256})
	}
	s.value = contract.ProviderResult{Data: data, Files: files}
	return nil
}
func stringsHasAPP(s string) bool { return len(s) >= 4 && s[:4] == "APP_" }
func (s *session) cleanup() error {
	s.stopAsync()
	var errs []error
	for id, h := range s.handles {
		if h.business != nil {
			var err error
			if h.business.request != nil {
				err = s.abortRequest(h.business.request)
			} else if c := s.businessClose(h); c != 0 {
				err = errors.New("host resource close failed")
			}
			if err != nil {
				errs = append(errs, err)
			}
			s.deleteHandle(id)
		}
	}
	if s.http != nil {
		errs = append(errs, s.http.Close())
	}
	for _, r := range s.roots {
		errs = append(errs, r.Close())
	}
	for _, h := range s.stages {
		if h.reader != nil {
			errs = append(errs, h.reader.Close())
		}
		errs = append(errs, h.stage.Abort())
	}
	return errors.Join(errs...)
}
func (s *session) dispatch(p packet) packet {
	if out, ok := s.asyncDispatch(p); ok {
		return out
	}
	return s.dispatchCore(s.lifetime, p)
}
func (s *session) dispatchCore(ctx context.Context, p packet) packet {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.ctx
	s.ctx = ctx
	defer func() { s.ctx = previous }()
	out := packet{Op: "response"}
	if s.ctx.Err() != nil {
		out.Count = canceled
		return out
	}
	switch p.Op {
	case "call":
		out.Handle, out.Count = s.call(p.Data)
	case "read":
		out.Data, out.Count = s.read(p.Handle, int(p.Count))
	case "write":
		out.Count = s.write(p.Handle, p.Data)
	case "pread", "pwrite":
		out = s.positioned(p)
	case "close":
		out.Count = s.close(p.Handle)
	default:
		out.Count = invalid
		s.failed = true
	}
	return out
}

func dataWithinLimits(value any, depth, items int64) bool {
	var count int64
	var visit func(any, int64) bool
	visit = func(v any, level int64) bool {
		count++
		if level > depth || count > items {
			return false
		}
		switch x := v.(type) {
		case map[string]any:
			for _, child := range x {
				count++
				if count > items || !visit(child, level+1) {
					return false
				}
			}
		case []any:
			for _, child := range x {
				if !visit(child, level+1) {
					return false
				}
			}
		}
		return true
	}
	return visit(value, 1)
}
