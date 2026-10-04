package artifacts

import (
	"context"
	"errors"
	"io/fs"
	"strconv"
	"strings"
	"time"

	"github.com/viewlegacy/onprest/internal/contract"
)

// Request owns a single input phase and an explicit execution lifetime. Commit
// validates all completed FileValues with the common compiler; it runs no code.
type Request struct {
	store         *Store
	owner         Owner
	capability    *contract.Capability
	limits        contract.Limits
	deadline      time.Time
	ctx           context.Context
	cancel        context.CancelFunc
	execution     context.Context
	stopExecution context.CancelFunc
	state         string
	files         map[string]*Stage
	bytes         int64
	started       bool
}

func (s *Store) BeginInput(ctx context.Context, o Owner, c *contract.Capability, deadline time.Time) (*Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.available(); e != nil {
		return nil, e
	}
	if ctx == nil || !o.valid() || c == nil || c.Name != o.Capability || !hasFile(c.Input) {
		return nil, fail("VALIDATION_FAILED", fs.ErrInvalid)
	}
	if _, ok := s.requests[o.RequestID]; ok {
		return nil, fail("CONFLICT", nil)
	}
	if len(s.requests) >= s.options.MaxPending {
		return nil, fail("BUSY", nil)
	}
	now := time.Now()
	maximum := now.Add(s.options.TransferTimeout)
	if deadline.IsZero() || maximum.Before(deadline) {
		deadline = maximum
	}
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if !now.Before(deadline) || ctx.Err() != nil {
		return nil, fail("EXECUTION_TIMEOUT", ctx.Err())
	}
	l := c.Limits()
	if l.MaxFileBytes < 1 || l.MaxTotalFileBytes < l.MaxFileBytes || l.MaxFiles < 1 {
		return nil, fail("VALIDATION_FAILED", nil)
	}
	input, cancel := context.WithDeadline(ctx, deadline)
	r := &Request{store: s, owner: o, capability: c.Clone(), limits: l, deadline: deadline, ctx: input, cancel: cancel, state: "receiving", files: map[string]*Stage{}}
	s.requests[o.RequestID] = r
	return r, nil
}
func hasFile(s *contract.Schema) bool {
	if s == nil {
		return false
	}
	if s.File != nil {
		return true
	}
	if hasFile(s.Items) {
		return true
	}
	for _, p := range s.Properties {
		if hasFile(p) {
			return true
		}
	}
	return false
}
func schemaAt(s *contract.Schema, pointer string) *contract.Schema {
	if pointer == "" {
		return s
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil
	}
	for _, raw := range strings.Split(pointer[1:], "/") {
		var token strings.Builder
		for i := 0; i < len(raw); i++ {
			if raw[i] == '~' {
				i++
				if i >= len(raw) {
					return nil
				}
				if raw[i] == '0' {
					token.WriteByte('~')
				} else if raw[i] == '1' {
					token.WriteByte('/')
				} else {
					return nil
				}
			} else {
				token.WriteByte(raw[i])
			}
		}
		if s == nil {
			return nil
		}
		if s.Items != nil {
			v := token.String()
			index, e := strconv.ParseUint(v, 10, 63)
			if e != nil || strconv.FormatUint(index, 10) != v {
				return nil
			}
			if maximum, ok := s.Constraints["maxItems"].(int64); ok && index >= uint64(maximum) {
				return nil
			}
			s = s.Items
		} else {
			s = s.Properties[token.String()]
		}
	}
	return s
}
func (r *Request) check(o Owner, state string) error {
	if r.owner != o {
		return fail("PERMISSION_DENIED", nil)
	}
	if e := r.store.available(); e != nil {
		return e
	}
	if r.state != state {
		return fail("CONFLICT", nil)
	}
	if state != "executing" && r.ctx.Err() != nil {
		return fail("EXECUTION_TIMEOUT", r.ctx.Err())
	}
	return nil
}
func (r *Request) NewFile(o Owner, pointer, name, media string) (*Stage, error) {
	s := r.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := r.check(o, "receiving"); e != nil {
		return nil, e
	}
	node := schemaAt(r.capability.Input, pointer)
	if node == nil || node.File == nil {
		return nil, fail("VALIDATION_FAILED", nil)
	}
	if _, ok := r.files[pointer]; ok {
		return nil, fail("CONFLICT", nil)
	}
	if int64(len(r.files)) >= r.limits.MaxFiles {
		return nil, fail("LIMIT_EXCEEDED", nil)
	}
	st, e := s.newStage(r.ctx, o, "i", name, media)
	if e != nil {
		return nil, e
	}
	st.request = r
	r.files[pointer] = st
	return st, nil
}

// Complete verifies transfer end claims, computes metadata independently, and
// applies the leaf's extensions/MIME/empty/size constraints before returning ID.
func (r *Request) Complete(o Owner, st *Stage, size int64, digest string) (contract.FileValue, error) {
	s := r.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := r.check(o, "receiving"); e != nil {
		return contract.FileValue{}, e
	}
	if st == nil || st.request != r || st.owner != o || st.retired || !st.sealed || st.ready || st.writeErr != nil {
		return contract.FileValue{}, fail("VALIDATION_FAILED", nil)
	}
	m := st.metadata()
	if m.SizeBytes != size || m.SHA256 != digest {
		return contract.FileValue{}, fail("VALIDATION_FAILED", nil)
	}
	var pointer string
	for p, v := range r.files {
		if v == st {
			pointer = p
			break
		}
	}
	v := contract.FileValue{Pointer: pointer, ID: st.id, RequestID: o.RequestID, Name: m.Name, MIMEType: m.MIMEType, SizeBytes: m.SizeBytes, SHA256: m.SHA256}
	if e := schemaAt(r.capability.Input, pointer).File.Validate(v); e != nil {
		return contract.FileValue{}, fail("VALIDATION_FAILED", e)
	}
	st.ready = true
	return v, nil
}
func (r *Request) Commit(o Owner, params map[string]any) (map[string]any, error) {
	s := r.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := r.check(o, "receiving"); e != nil {
		return nil, e
	}
	normalized, leaves, e := r.capability.ValidateInput(params)
	if e != nil {
		return nil, fail("VALIDATION_FAILED", e)
	}
	if len(leaves) != len(r.files) {
		return nil, fail("VALIDATION_FAILED", nil)
	}
	seen := map[string]bool{}
	for _, leaf := range leaves {
		st := r.files[leaf.Pointer]
		if st == nil || !st.ready || st.retired || seen[st.id] || leaf.Value.ID != st.id || leaf.Value.RequestID != o.RequestID || leaf.Value.Pointer != leaf.Pointer {
			return nil, fail("VALIDATION_FAILED", nil)
		}
		m := st.metadata()
		if leaf.Value.Name != m.Name || leaf.Value.MIMEType != m.MIMEType || leaf.Value.SizeBytes != m.SizeBytes || leaf.Value.SHA256 != m.SHA256 {
			return nil, fail("VALIDATION_FAILED", nil)
		}
		seen[st.id] = true
	}
	r.state = "ready"
	return normalized, nil
}

// Start replaces the transfer phase with the controller's execution context.
// The fixed transfer deadline is never an independent TTL on executing input.
func (r *Request) Start(o Owner, execution context.Context) error {
	s := r.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := r.check(o, "ready"); e != nil {
		return e
	}
	if execution == nil || execution.Err() != nil {
		return fail("EXECUTION_TIMEOUT", nil)
	}
	r.state = "executing"
	r.started = true
	r.execution, r.stopExecution = context.WithCancel(execution)
	r.cancel()
	return nil
}
func (r *Request) Open(o Owner, v contract.FileValue) (*Reader, error) {
	s := r.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := r.check(o, "executing"); e != nil {
		return nil, e
	}
	st := r.files[v.Pointer]
	if st == nil || st.id != v.ID || v.RequestID != o.RequestID {
		return nil, fail("PERMISSION_DENIED", nil)
	}
	m := st.metadata()
	if v.Name != m.Name || v.MIMEType != m.MIMEType || v.SizeBytes != m.SizeBytes || v.SHA256 != m.SHA256 {
		return nil, fail("VALIDATION_FAILED", nil)
	}
	return st.openLocked(r.execution)
}
func (r *Request) Deadline() time.Time { return r.deadline }
func (r *Request) State() string       { r.store.mu.Lock(); defer r.store.mu.Unlock(); return r.state }

// Context lets the dispatcher share the same cancellation with its worker.
func (r *Request) Context(o Owner) (context.Context, error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if r.owner != o {
		return nil, fail("PERMISSION_DENIED", nil)
	}
	if r.state == "executing" {
		return r.execution, nil
	}
	if r.state == "receiving" || r.state == "ready" {
		return r.ctx, nil
	}
	return nil, fail("CONFLICT", nil)
}

// Abort stops new input operations. Executing workers must be stopped by their
// controller before ReleaseStopped; readers remain accounted until closed.
func (r *Request) Abort(o Owner) error {
	s := r.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if o != r.owner {
		return fail("PERMISSION_DENIED", nil)
	}
	r.abortLocked()
	return nil
}
func (r *Request) abortLocked() {
	if r.state == "released" {
		return
	}
	r.cancel()
	if r.stopExecution != nil {
		r.stopExecution()
	}
	r.state = "aborted"
}
func (r *Request) ReleaseStopped(o Owner) error {
	s := r.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if o != r.owner {
		return fail("PERMISSION_DENIED", nil)
	}
	return r.releaseStoppedLocked()
}
func (r *Request) releaseStoppedLocked() error {
	r.cancel()
	if r.stopExecution != nil {
		r.stopExecution()
	}
	r.state = "released"
	var e error
	for _, st := range r.files {
		st.retired = true
		e = join(e, st.cleanupLocked())
	}
	r.releaseSlotLocked()
	return e
}
func (r *Request) releaseSlotLocked() {
	if r.state != "released" {
		return
	}
	for _, st := range r.files {
		if _, ok := r.store.stages[st.id]; ok {
			return
		}
	}
	if r.store.requests[r.owner.RequestID] == r {
		delete(r.store.requests, r.owner.RequestID)
	}
}
func join(a, b error) error {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	return safe(errors.Join(a, b))
}
