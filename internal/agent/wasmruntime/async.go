package wasmruntime

// SDK calls yield between bounded polls. No host import waits for business I/O,
// so the single-threaded Go WASI scheduler can deliver a caller's cancellation.
import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/viewlegacy/onprest/internal/contract"
)

const asyncBit int64 = 1 << 62

type asyncEntry struct {
	kind     string
	data     *bytes.Reader
	buffer   bytes.Buffer
	ready    bool
	job      *asyncJob
	borrowed bool
}
type asyncJob struct {
	cancel        context.CancelFunc
	done          chan struct{}
	response      packet
	bytes         []byte
	control, data int64
}
type asyncState struct {
	mu       sync.Mutex
	entries  map[int64]*asyncEntry
	stopping bool
	jobs     int
	wg       sync.WaitGroup
}

func (s *session) reserveHandle() bool {
	for {
		n := s.handleSlots.Load()
		if n >= int64(s.l.Handles) {
			return false
		}
		if s.handleSlots.CompareAndSwap(n, n+1) {
			return true
		}
	}
}
func (s *session) handleCount() int { return int(s.handleSlots.Load()) }
func (s *session) deleteHandle(id int64) {
	if _, ok := s.handles[id]; ok {
		delete(s.handles, id)
		s.handleSlots.Add(-1)
	}
}
func (s *session) asyncID(e *asyncEntry) int64 {
	if !s.reserveHandle() {
		return 0
	}
	if s.async.entries == nil {
		s.async.entries = map[int64]*asyncEntry{}
	}
	for {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			s.handleSlots.Add(-1)
			return 0
		}
		id := int64(binary.LittleEndian.Uint64(b[:])&uint64(asyncBit-1)) | asyncBit
		if s.async.entries[id] == nil {
			s.async.entries[id] = e
			return id
		}
	}
}
func (s *session) asyncDelete(id int64) {
	if s.async.entries[id] != nil {
		delete(s.async.entries, id)
		s.handleSlots.Add(-1)
	}
}
func (s *session) asyncControl(v any) packet {
	raw, e := json.Marshal(v)
	if e != nil || len(raw) > Chunk {
		return packet{Op: "response", Count: invalid}
	}
	id := s.asyncID(&asyncEntry{kind: "data", data: bytes.NewReader(raw)})
	if id == 0 {
		return packet{Op: "response", Count: limit}
	}
	return packet{Op: "response", Handle: id}
}

// A job reserves its control and payload slots before any side effect. Poll and
// cancel therefore remain usable at the handle limit. Closing a borrowed
// control returns it to the reservation until the terminal poll detaches it.
func (s *session) jobControl(j *asyncJob, v any) packet {
	e := s.async.entries[j.control]
	if e == nil || e.kind != "reserved" {
		return packet{Op: "response", Count: invalid}
	}
	raw, err := json.Marshal(v)
	if err != nil || len(raw) > Chunk {
		return packet{Op: "response", Count: invalid}
	}
	e.kind = "data"
	e.data = bytes.NewReader(raw)
	e.borrowed = true
	return packet{Op: "response", Handle: j.control}
}
func (s *session) asyncJobs() int { s.async.mu.Lock(); defer s.async.mu.Unlock(); return s.async.jobs }
func (s *session) stopAsync() {
	s.async.mu.Lock()
	s.async.stopping = true
	for _, e := range s.async.entries {
		if e.job != nil {
			e.job.cancel()
		}
	}
	s.async.mu.Unlock()
	s.async.wg.Wait()
	s.async.mu.Lock()
	defer s.async.mu.Unlock()
	for id := range s.async.entries {
		s.asyncDelete(id)
	}
	s.async.jobs = 0
}
func (s *session) asyncDispatch(p packet) (packet, bool) {
	out := packet{Op: "response", Count: invalid}
	if p.Op != "call" {
		if p.Handle&asyncBit == 0 {
			return out, false
		}
		s.async.mu.Lock()
		defer s.async.mu.Unlock()
		e := s.async.entries[p.Handle]
		if e == nil {
			return out, true
		}
		switch p.Op {
		case "read":
			if e.kind != "data" || p.Count < 0 || p.Count > Chunk {
				return out, true
			}
			out.Data = make([]byte, p.Count)
			n, err := e.data.Read(out.Data)
			if err != nil && err != io.EOF {
				out.Count = ioError
				return out, true
			}
			out.Data = out.Data[:n]
			out.Count = int32(n)
		case "write":
			if e.kind != "buffer" || e.ready || len(p.Data) > Chunk || e.buffer.Len()+len(p.Data) > Chunk {
				out.Count = limit
				return out, true
			}
			n, _ := e.buffer.Write(p.Data)
			out.Count = int32(n)
		case "close":
			if e.kind == "buffer" {
				if e.ready {
					return out, true
				}
				e.ready = true
				out.Count = 0
				return out, true
			}
			if e.job != nil {
				return out, true
			}
			if e.borrowed {
				e.kind = "reserved"
				e.data = nil
				e.borrowed = false
			} else {
				s.asyncDelete(p.Handle)
			}
			out.Count = 0
		}
		return out, true
	}
	value, err := contract.DecodeJSON(p.Data, contract.Limits{MaxBytes: Chunk, MaxDepth: 12, MaxItems: 1024})
	if err != nil {
		return out, false
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return out, false
	}
	method, _ := obj["method"].(string)
	// Diagnostics must not freeze the Go WASI scheduler behind business I/O.
	if method == "log.write" {
		args, ok := obj["args"].(map[string]any)
		if !ok || len(obj) != 2 || len(args) != 2 {
			return out, true
		}
		level, ok := args["level"].(string)
		message, ok2 := args["message"].(string)
		if !ok || !ok2 {
			return out, true
		}
		s.async.mu.Lock()
		defer s.async.mu.Unlock()
		if s.lifetime.Err() != nil {
			out.Count = canceled
			return out, true
		}
		s.logs.log(level, message)
		return s.asyncControl(map[string]any{}), true
	}
	if method == "result.commit" {
		s.async.mu.Lock()
		if s.async.jobs != 0 {
			s.asyncFailure.Store(true)
			for _, e := range s.async.entries {
				if e.job != nil {
					e.job.cancel()
				}
			}
			s.async.mu.Unlock()
			return out, true
		}
		s.async.mu.Unlock()
	}
	if !strings.HasPrefix(method, "runtime.") {
		return out, false
	}
	args, ok := obj["args"].(map[string]any)
	if !ok || len(obj) != 2 {
		return out, true
	}
	s.async.mu.Lock()
	defer s.async.mu.Unlock()
	if s.async.stopping || s.lifetime.Err() != nil {
		out.Count = canceled
		return out, true
	}
	if (s.asyncFailure.Load() || s.terminal.Load()) && (method == "runtime.begin" || method == "runtime.buffer") {
		s.asyncFailure.Store(true)
		return out, true
	}
	switch method {
	case "runtime.buffer":
		if len(args) != 0 {
			return out, true
		}
		id := s.asyncID(&asyncEntry{kind: "buffer"})
		if id == 0 {
			out.Count = limit
			return out, true
		}
		r := s.asyncControl(map[string]any{"body_handle": id})
		if r.Count < 0 {
			s.asyncDelete(id)
		}
		return r, true
	case "runtime.begin":
		if s.async.jobs >= s.l.HostCalls {
			out.Count = limit
			return out, true
		}
		op, ok := args["op"].(string)
		deadline, ok2 := args["deadline_ns"].(int64)
		if !ok || !ok2 || deadline < 0 {
			return out, true
		}
		task := packet{Op: op}
		var callMethod string
		switch op {
		case "call":
			request, ok := args["request"].(map[string]any)
			if !ok || len(args) != 3 {
				return out, true
			}
			var validMethod bool
			callMethod, validMethod = request["method"].(string)
			_, validArgs := request["args"].(map[string]any)
			if len(request) != 2 || !validMethod || !validArgs {
				return out, true
			}
			if strings.HasPrefix(callMethod, "runtime.") || strings.HasPrefix(callMethod, "result.") || callMethod == "log.write" {
				return out, true
			}
			task.Data, err = contract.CanonicalJSON(request)
			if err != nil || len(task.Data) > Chunk {
				out.Count = limit
				return out, true
			}
		case "read", "write", "close":
			task.Handle, err = numberHandle(args["handle"])
			if err != nil || task.Handle&asyncBit != 0 {
				return out, true
			}
			if op == "read" {
				n, ok := args["count"].(int64)
				if !ok || n < 0 || n > Chunk || len(args) != 4 {
					return out, true
				}
				task.Count = int32(n)
			}
			if op == "write" {
				id, e := numberHandle(args["input_handle"])
				entry := s.async.entries[id]
				if e != nil || entry == nil || entry.kind != "buffer" || !entry.ready || len(args) != 4 {
					return out, true
				}
				task.Data = append([]byte(nil), entry.buffer.Bytes()...)
				s.asyncDelete(id)
			}
			if op == "close" && len(args) != 3 {
				return out, true
			}
		default:
			return out, true
		}
		ctx, cancel := context.WithCancel(s.lifetime)
		if deadline != 0 {
			cancel()
			ctx, cancel = context.WithDeadline(s.lifetime, time.Unix(0, deadline))
		}
		j := &asyncJob{cancel: cancel, done: make(chan struct{})}
		id := s.asyncID(&asyncEntry{kind: "job", job: j})
		if id == 0 {
			cancel()
			out.Count = limit
			return out, true
		}
		j.control = s.asyncID(&asyncEntry{kind: "reserved"})
		j.data = s.asyncID(&asyncEntry{kind: "reserved"})
		if j.control == 0 || j.data == 0 {
			cancel()
			s.asyncDelete(id)
			s.asyncDelete(j.control)
			s.asyncDelete(j.data)
			out.Count = limit
			return out, true
		}
		ack := s.jobControl(j, map[string]any{"operation_handle": id})
		if ack.Count < 0 {
			cancel()
			s.asyncDelete(id)
			return ack, true
		}
		s.async.jobs++
		s.async.wg.Add(1)
		go s.runAsync(ctx, j, task, callMethod)
		return ack, true
	case "runtime.poll", "runtime.cancel":
		id, e := numberHandle(args["operation_handle"])
		entry := s.async.entries[id]
		if e != nil || len(args) != 1 || entry == nil || entry.job == nil {
			return out, true
		}
		j := entry.job
		if method == "runtime.cancel" {
			j.cancel()
			return s.jobControl(j, map[string]any{}), true
		}
		select {
		case <-j.done:
		default:
			return s.jobControl(j, map[string]any{"ready": false}), true
		}
		if e := s.async.entries[j.control]; e == nil || e.kind != "reserved" {
			return out, true
		}
		dataID := int64(0)
		if len(j.bytes) > 0 {
			dataID = j.data
			e := s.async.entries[dataID]
			e.kind = "data"
			e.data = bytes.NewReader(j.bytes)
		} else {
			s.asyncDelete(j.data)
		}
		result := s.jobControl(j, map[string]any{"ready": true, "count": j.response.Count, "body_handle": dataID, "handle": j.response.Handle})
		s.async.entries[j.control].borrowed = false

		s.asyncDelete(id)
		s.async.jobs--
		return result, true
	}
	return out, true
}
func (s *session) runAsync(ctx context.Context, j *asyncJob, p packet, method string) {
	defer s.async.wg.Done()
	s.mu.Lock()
	previous := s.ctx
	s.ctx = ctx
	out := packet{Op: "response"}
	before := make(map[int64]bool, len(s.handles))
	for id := range s.handles {
		before[id] = true
	}
	handoff := false
	if s.asyncFailure.Load() {
		out.Count = invalid
	} else if ctx.Err() != nil {
		out.Count = canceled
	} else {
		var stop func() bool
		id := p.Handle
		if method == "http.do" || method == "http.abort" {
			v, _ := contract.DecodeJSON(p.Data, contract.Limits{MaxBytes: Chunk, MaxDepth: 8, MaxItems: 1024})
			a := v.(map[string]any)["args"].(map[string]any)
			id, _ = numberHandle(a["request_handle"])
		}
		if h := s.handles[id]; h != nil && h.business != nil && p.Op != "close" {
			b := h.business
			stop = context.AfterFunc(ctx, func() { b.interrupt() })
		}
		switch p.Op {
		case "call":
			out.Handle, out.Count = s.call(p.Data)
		case "read":
			out.Data, out.Count = s.read(p.Handle, int(p.Count))
		case "write":
			out.Count = s.write(p.Handle, p.Data)
		case "close":
			out.Count = s.close(p.Handle)
		}
		if stop != nil && !stop() { // synchronize interruption before reporting completion
			// business.interrupt serializes with close; it is idempotent.
			if h := s.handles[id]; h != nil && h.business != nil {
				h.business.interrupt()
			}
		}
		if ctx.Err() != nil {
			out.Count = canceled
		}
		if out.Handle > 0 {
			h := s.handles[out.Handle]
			if h != nil && h.control != nil {
				j.bytes = append([]byte(nil), func() []byte { b, _ := io.ReadAll(h.control); return b }()...)
				s.deleteHandle(out.Handle)
				out.Handle = 0
				// An open context lives as long as its returned business resource.
				var resource int64
				decoded, _ := contract.DecodeJSON(j.bytes, contract.Limits{MaxBytes: Chunk, MaxDepth: 8, MaxItems: 1024})
				if m, ok := decoded.(map[string]any); ok {
					if n, e := numberHandle(m["request_handle"]); e == nil {
						resource = n
					} else if n, e := numberHandle(m["body_handle"]); e == nil {
						resource = n
					}
				}
				if h := s.handles[resource]; out.Count == 0 && h != nil && h.business != nil && (method == "http.open" || method == "input.open" || strings.HasPrefix(method, "files.")) {
					h.business.contextCancel = j.cancel
					handoff = true
				}
			}
		}
		if p.Op == "read" {
			j.bytes = out.Data
			out.Data = nil
		}
	}
	if out.Count < 0 {
		for id, h := range s.handles {
			if !before[id] && h.business != nil {
				_ = s.close(id)
			}
		}
	}
	s.ctx = previous
	s.mu.Unlock()
	if !handoff {
		j.cancel()
	}
	j.response = out
	close(j.done)
}
