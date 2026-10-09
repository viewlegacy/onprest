// Package onprest connects Go WASI command modules to an Onprest Agent.
package onprest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Result keeps JSON data and ordered file streams separate.
type Result struct {
	Data  any
	Files []File
}

// File transfers ownership of Reader to Run, including when the handler fails.
type File struct {
	Name     string
	MIMEType string
	Reader   io.Reader
}

// ErrLimitExceeded identifies every host capacity or I/O limit failure.
var ErrLimitExceeded = errors.New("Onprest limit exceeded")

type publicError struct{ code, message string }

func (e *publicError) Error() string { return e.code + ": " + e.message }

// PublicError marks a deliberately public application message. Invalid values
// remain internal errors and are never published as an application terminal.
func PublicError(code, message string) error { return &publicError{code, message} }

var appCode = regexp.MustCompile(`^APP_[A-Z][A-Z0-9_]{0,59}$`)

func validPublic(e *publicError) bool {
	if !appCode.MatchString(e.code) || len(e.message) < 1 || len(e.message) > 1024 || !utf8.ValidString(e.message) {
		return false
	}
	for _, r := range e.message {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

const chunk = 65536
const envelopeLimit = 32 << 20

type host interface {
	call([]byte) int64
	read(int64, []byte) int32
	write(int64, []byte) int32
	close(int64) int32
}
type sessionKey struct{}
type execution struct {
	host  host
	mu    sync.Mutex
	alive bool
	owned map[*owned]bool
	ctx   context.Context
}

var active struct {
	sync.Mutex
	run *execution
}

func (e *execution) check(ctx context.Context) error {
	if ctx == nil || ctx.Value(sessionKey{}) != e {
		return fs.ErrInvalid
	}
	e.mu.Lock()
	alive := e.alive
	e.mu.Unlock()
	if !alive {
		return fs.ErrClosed
	}
	return ctx.Err()
}
func resolve(ctx context.Context) (*execution, error) {
	if ctx == nil {
		return nil, fs.ErrInvalid
	}
	e, ok := ctx.Value(sessionKey{}).(*execution)
	if !ok {
		return nil, fs.ErrInvalid
	}
	return e, e.check(ctx)
}
func hostError(code int64, ctx context.Context) error {
	switch code {
	case -1:
		return fs.ErrInvalid
	case -2:
		return fs.ErrPermission
	case -3:
		return fs.ErrNotExist
	case -4:
		return ErrLimitExceeded
	case -5:
		if ctx != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		return context.Canceled
	case -7:
		return errors.ErrUnsupported
	case -8:
		return fs.ErrExist
	default:
		return errors.New("Onprest host I/O failed")
	}
}
func (e *execution) rawRead(h int64) ([]byte, error) {
	var out bytes.Buffer
	buf := make([]byte, chunk)
	for {
		n := e.host.read(h, buf)
		if n < 0 {
			return nil, hostError(int64(n), e.ctx)
		}
		if int(n) > len(buf) {
			return nil, fs.ErrInvalid
		}
		if n == 0 {
			return out.Bytes(), nil
		}
		if out.Len()+int(n) > chunk {
			return nil, ErrLimitExceeded
		}
		out.Write(buf[:n])
	}
}
func (e *execution) call(method string, args any) (map[string]any, error) {
	b, err := json.Marshal(map[string]any{"method": method, "args": args})
	if err != nil {
		return nil, err
	}
	if len(b) > chunk {
		return nil, ErrLimitExceeded
	}
	h := e.host.call(b)
	if h <= 0 {
		return nil, hostError(h, e.ctx)
	}
	b, err = e.rawRead(h)
	closeErr := hostErrorIf(e.host.close(h), e.ctx)
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var out map[string]any
	err = d.Decode(&out)
	return out, err
}
func hostErrorIf(c int32, ctx context.Context) error {
	if c < 0 {
		return hostError(int64(c), ctx)
	}
	return nil
}
func integer(v any) int64 {
	n, ok := v.(json.Number)
	if !ok {
		return 0
	}
	i, _ := n.Int64()
	return i
}
func deadline(ctx context.Context) int64 {
	if d, ok := ctx.Deadline(); ok {
		return d.UnixNano()
	}
	return 0
}
func (e *execution) operation(ctx context.Context, args map[string]any) (map[string]any, []byte, error) {
	if err := e.check(ctx); err != nil {
		return nil, nil, err
	}
	args["deadline_ns"] = deadline(ctx)
	opened, err := e.call("runtime.begin", args)
	if err != nil {
		return nil, nil, err
	}
	id := integer(opened["operation_handle"])
	if id <= 0 {
		return nil, nil, fs.ErrInvalid
	}
	cancelSent := false
	for {
		if ctx.Err() != nil && !cancelSent {
			_, err = e.call("runtime.cancel", map[string]any{"operation_handle": id})
			if err != nil {
				return nil, nil, err
			}
			cancelSent = true
		}
		v, err := e.call("runtime.poll", map[string]any{"operation_handle": id})
		if err != nil {
			return nil, nil, err
		}
		if ready, _ := v["ready"].(bool); !ready {
			time.Sleep(time.Millisecond)
			continue
		}
		body := integer(v["body_handle"])
		var data []byte
		if body > 0 {
			data, err = e.rawRead(body)
			c := hostErrorIf(e.host.close(body), e.ctx)
			if err == nil {
				err = c
			}
		}
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		if err != nil {
			return nil, nil, err
		}
		if n := integer(v["count"]); n < 0 {
			return nil, nil, hostError(n, ctx)
		}
		return v, data, nil
	}
}
func (e *execution) businessCall(ctx context.Context, method string, args any) (map[string]any, error) {
	_, raw, err := e.operation(ctx, map[string]any{"op": "call", "request": map[string]any{"method": method, "args": args}})
	if err != nil {
		return nil, err
	}
	var out map[string]any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	err = d.Decode(&out)
	return out, err
}

type owned struct {
	e        *execution
	ctx      context.Context
	handle   int64
	op, path string
	writer   bool
	cancel   context.CancelFunc
	mu       sync.Mutex
	closed   bool
	err      error
	after    func() error
}

func (e *execution) own(ctx context.Context, h int64, op, path string, writer bool) *owned {
	ctx, cancel := context.WithCancel(ctx)
	v := &owned{e: e, ctx: ctx, cancel: cancel, handle: h, op: op, path: path, writer: writer}
	e.mu.Lock()
	e.owned[v] = true
	e.mu.Unlock()
	return v
}
func (o *owned) wrap(err error) error {
	if err == nil {
		return nil
	}
	if o.path != "" {
		return &fs.PathError{Op: o.op, Path: o.path, Err: err}
	}
	return err
}
func (o *owned) Read(b []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return 0, o.wrap(fs.ErrClosed)
	}
	if len(b) == 0 {
		return 0, nil
	}
	n := len(b)
	if n > chunk {
		n = chunk
	}
	_, raw, err := o.e.operation(o.ctx, map[string]any{"op": "read", "handle": o.handle, "count": n})
	if err != nil {
		return 0, o.wrap(err)
	}
	if len(raw) > n {
		return 0, o.wrap(fs.ErrInvalid)
	}
	copy(b, raw)
	if len(raw) == 0 {
		return 0, io.EOF
	}
	return len(raw), nil
}
func (o *owned) Write(b []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return 0, o.wrap(fs.ErrClosed)
	}
	total := 0
	for len(b) > 0 {
		n := len(b)
		if n > chunk {
			n = chunk
		}
		v, err := o.e.call("runtime.buffer", map[string]any{})
		if err != nil {
			return total, o.wrap(err)
		}
		h := integer(v["body_handle"])
		w := o.e.host.write(h, b[:n])
		c := o.e.host.close(h)
		if w < 0 || c < 0 {
			return total, o.wrap(hostError(int64(min(w, c)), o.ctx))
		}
		out, _, err := o.e.operation(o.ctx, map[string]any{"op": "write", "handle": o.handle, "input_handle": h})
		if err != nil {
			return total, o.wrap(err)
		}
		written := int(integer(out["count"]))
		if written < 0 || written > n {
			return total, o.wrap(fs.ErrInvalid)
		}
		total += written
		b = b[written:]
		if written == 0 {
			return total, o.wrap(io.ErrShortWrite)
		}
	}
	return total, nil
}
func (o *owned) Close() error {
	o.cancel()
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return o.err
	}
	o.closed = true
	_, _, err := o.e.operation(o.e.ctx, map[string]any{"op": "close", "handle": o.handle})
	if o.after != nil {
		err = errors.Join(err, o.after())
	}
	o.err = o.wrap(err)
	o.e.mu.Lock()
	delete(o.e.owned, o)
	o.e.mu.Unlock()
	return o.err
}
func (e *execution) cleanup() error {
	var errs []error
	for {
		e.mu.Lock()
		var o *owned
		for v := range e.owned {
			o = v
			break
		}
		e.mu.Unlock()
		if o == nil {
			break
		}
		errs = append(errs, o.Close())
	}
	return errors.Join(errs...)
}

// Log writes local diagnostics only. It does not change the handler's result.
func Log(level, message string) {
	active.Lock()
	e := active.run
	active.Unlock()
	if e == nil {
		return
	}
	_, _ = e.call("log.write", map[string]any{"level": level, "message": message})
}

// Run decodes one normalized input, invokes the handler, commits a terminal
// result through the dedicated ABI, and releases its owned resources.
func Run[P any](handler func(context.Context, P) (Result, error)) {
	run[P](os.Stdin, os.Stderr, platformHost(), handler)
}
func run[P any](in io.Reader, diagnostic io.Writer, h host, handler func(context.Context, P) (Result, error)) {
	e := &execution{host: h, alive: true, owned: map[*owned]bool{}}
	e.ctx = context.WithValue(context.Background(), sessionKey{}, e)
	active.Lock()
	active.run = e
	active.Unlock()
	defer func() {
		e.mu.Lock()
		e.alive = false
		e.mu.Unlock()
		active.Lock()
		if active.run == e {
			active.run = nil
		}
		active.Unlock()
	}()
	var result Result
	var failure error
	category := "execution"
	func() {
		defer func() {
			if p := recover(); p != nil {
				failure = fmt.Errorf("handler panic: %v", p)
			}
		}()
		raw, err := io.ReadAll(io.LimitReader(in, (1<<20)+1))
		if err != nil || len(raw) > 1<<20 {
			failure = errors.New("invalid normalized input size")
			return
		}
		var p P
		p, err = decode[P](raw, e)
		if err != nil {
			failure = err
			return
		}
		result, failure = handler(e.ctx, p)
	}()
	var files []map[string]int64
	if failure == nil {
		for _, f := range result.Files {
			if nilReader(f.Reader) || !validName(f.Name) || !validMIME(f.MIMEType) {
				failure = errors.New("invalid result file")
				category = "output"
				break
			}
			v, err := e.call("result.file", map[string]any{"name": f.Name, "mime_type": f.MIMEType})
			if err != nil {
				failure = err
				category = "output"
				break
			}
			body := integer(v["body_handle"])
			err = copyRaw(e, body, f.Reader)
			if err != nil {
				failure = err
				category = "output"
				break
			}
			files = append(files, map[string]int64{"handle": integer(v["file_handle"])})
		}
	}
	for _, f := range result.Files {
		if !nilReader(f.Reader) {
			if c, ok := f.Reader.(io.Closer); ok {
				if err := c.Close(); err != nil {
					if failure == nil {
						failure = err
						category = "output"
					} else {
						fmt.Fprintln(diagnostic, "result cleanup:", err)
					}
				}
			}
		}
	}
	if err := e.cleanup(); err != nil {
		if failure == nil {
			failure = err
			category = "execution"
		} else {
			fmt.Fprintln(diagnostic, "resource cleanup:", err)
		}
	}
	var envelope any
	var raw []byte
	if failure == nil {
		if files == nil {
			files = []map[string]int64{}
		}
		envelope = map[string]any{"data": result.Data, "files": files}
		var marshalErr error
		raw, marshalErr = json.Marshal(envelope)
		if marshalErr != nil {
			failure = marshalErr
			category = "output"
		} else if len(raw) > envelopeLimit {
			failure = ErrLimitExceeded
			category = "limit"
		}
	}
	if failure != nil {
		fmt.Fprintln(diagnostic, "Wasm handler:", failure)
		var public *publicError
		if errors.Is(failure, ErrLimitExceeded) {
			category = "limit"
		}
		if category != "limit" && errors.As(failure, &public) && validPublic(public) {
			envelope = map[string]any{"error": map[string]any{"kind": "public", "code": public.code, "message": public.message}}
		} else {
			envelope = map[string]any{"error": map[string]any{"kind": "internal", "category": category}}
		}
	}
	var err error
	if failure != nil {
		raw, err = json.Marshal(envelope)
	}
	if err == nil {
		var v map[string]any
		v, err = e.call("result.open", map[string]any{})
		if err == nil {
			err = copyRaw(e, integer(v["body_handle"]), bytes.NewReader(raw))
			if err == nil {
				_, err = e.call("result.commit", map[string]any{"result_handle": integer(v["result_handle"])})
			}
		}
	}
	if err != nil {
		fmt.Fprintln(diagnostic, "Wasm result:", err)
	}
}
func copyRaw(e *execution, h int64, r io.Reader) error {
	buf := make([]byte, chunk)
	empty := 0
	for {
		n, err := r.Read(buf)
		if n < 0 || n > len(buf) {
			return errors.New("invalid reader count")
		}
		if n > 0 {
			empty = 0
		}
		if n > 0 {
			left := buf[:n]
			for len(left) > 0 {
				w := e.host.write(h, left)
				if w < 0 {
					return hostError(int64(w), e.ctx)
				}
				if w == 0 || int(w) > len(left) {
					return io.ErrShortWrite
				}
				left = left[w:]
			}
		}
		if err != nil {
			if err != io.EOF {
				return err
			}
			break
		}
		if n == 0 {
			empty++
			if empty >= 100 {
				return io.ErrNoProgress
			}
			time.Sleep(time.Millisecond)
		}
	}
	return hostErrorIf(e.host.close(h), e.ctx)
}
func validName(s string) bool {
	if len(s) < 1 || len(s) > 255 || s == "." || s == ".." || !utf8.ValidString(s) || strings.ContainsAny(s, "/\\") {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validMIME(s string) bool {
	if s == "" || s != strings.ToLower(s) || strings.TrimSpace(s) != s {
		return false
	}
	media, params, err := mime.ParseMediaType(s)
	return err == nil && len(params) == 0 && media == s && strings.Contains(s, "/") && !strings.Contains(s, "*")
}
