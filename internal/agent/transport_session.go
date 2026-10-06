package agent

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/viewlegacy/onprest/internal/agent/artifacts"
	"github.com/viewlegacy/onprest/internal/contract"
	"github.com/viewlegacy/onprest/internal/protocol"
	"github.com/viewlegacy/onprest/internal/ws"
	"io"
	"sync"
	"time"
)

type outputSessionKey struct{}

type inputCall struct {
	owner               artifacts.Owner
	request             *artifacts.Request
	ctx                 context.Context
	cancel              context.CancelFunc
	cap                 *contract.Capability
	files               map[string]contract.FileValue
	until               time.Time
	opening             int
	executing, terminal bool
}
type pendingTransfer struct {
	request string
	ctx     context.Context
	cancel  context.CancelFunc
}
type retainedOwner struct {
	owner   artifacts.Owner
	files   map[string]artifacts.Descriptor
	expires time.Time
}
type transportSession struct {
	runner     *Runner
	ctx        context.Context
	cancel     context.CancelFunc
	conn       *ws.Conn
	scheduler  *ws.Scheduler
	transfers  *ws.Transfers
	generation string
	mu         sync.Mutex
	calls      map[string]*inputCall
	owners     map[string]retainedOwner
	opens      map[string]*pendingTransfer
	recentMax  int
	wg         sync.WaitGroup
	inflight   *inflightRegistry
}

func (r *Runner) serveTransport(parent context.Context, conn *ws.Conn) {
	ctx, cancel := context.WithCancel(parent)
	s := &transportSession{runner: r, ctx: ctx, cancel: cancel, conn: conn, generation: randomTransportID(), calls: map[string]*inputCall{}, owners: map[string]retainedOwner{}, opens: map[string]*pendingTransfer{}, inflight: &inflightRegistry{cancels: map[string]context.CancelFunc{}}}
	s.scheduler = ws.NewScheduler(ctx)
	s.transfers = ws.NewTransfers(ctx, s.sendTransferFrame)
	runtime := r.versioned.Runtime()
	s.recentMax = int(runtime["input_limits"].(map[string]any)["max_recent_requests"].(int64))
	s.wg.Add(2)
	go func() {
		defer s.wg.Done()
		if s.scheduler.Run() != nil {
			cancel()
			conn.Close()
		}
	}()
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.sweep()
			case <-ctx.Done():
				conn.Close()
				return
			}
		}
	}()
	defer func() {
		cancel()
		s.inflight.cancelAll()
		s.mu.Lock()
		for _, c := range s.calls {
			c.cancel()
			if c.request != nil {
				c.request.Abort(c.owner)
			}
		}
		s.mu.Unlock()
		if r.store != nil {
			r.store.RevokeGeneration(s.generation)
		}
		conn.Close()
		s.transfers.Close()
		s.wg.Wait()
		s.mu.Lock()
		for _, c := range s.calls {
			if c.request != nil {
				c.request.ReleaseStopped(c.owner)
			}
		}
		s.mu.Unlock()
	}()
	max := *r.cf.Runtime.MaxConcurrentRequests
	queue := make(chan requestTask, max)
	for range max {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			for {
				select {
				case task := <-queue:
					if task.ctx.Err() == nil {
						resp := r.executePrepared(context.WithValue(task.ctx, outputSessionKey{}, s), task.prepared)
						s.respond(task.ctx, resp)
					}
					s.inflight.finish(task.id)
					if task.cancel != nil {
						task.cancel()
					}
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	for ctx.Err() == nil {
		raw, e := conn.ReadText()
		if e != nil {
			return
		}
		var guard struct {
			Type       string        `json:"type"`
			ID         boundedWireID `json:"id"`
			TransferID boundedWireID `json:"transfer_id"`
		}
		if json.Unmarshal(raw, &guard) != nil {
			return
		}
		if guard.Type == "ack" {
			if !protocol.ValidID(string(guard.TransferID)) {
				return
			}
		} else if !protocol.ValidID(string(guard.ID)) {
			return
		}
		if protocol.IsTransferType(guard.Type) {
			switch guard.Type {
			case "request_begin", "input_open", "request_commit", "transfer_open", "transfer_chunk", "transfer_end", "transfer_cancel", "ack":
			default:
				return
			}
			f, e := protocol.DecodeFrame(raw)
			if e != nil {
				return
			}
			if f.Type == "ack" || f.Type == "transfer_chunk" || f.Type == "transfer_end" {
				if e = s.transfers.Handle(f); e != nil {
					return
				}
				continue
			}
			s.dispatch(f)
			continue
		}
		if guard.Type == "cancel" {
			value, e := contract.DecodeJSON(raw, contract.Limits{MaxBytes: protocol.ManagementBytes, MaxDepth: 4, MaxItems: 8})
			if e != nil {
				return
			}
			m, ok := value.(map[string]any)
			if !ok || len(m) != 2 {
				return
			}
			s.dispatch(protocol.Frame{Type: "cancel", ID: string(guard.ID)})
			continue
		}
		var req protocol.Request
		if json.Unmarshal(raw, &req) != nil {
			s.respond(ctx, protocol.Response{ID: string(guard.ID), Error: inputFailure("INVALID_REQUEST")})
			continue
		}
		var release func()
		if req.Capability == "meta" {
			var code string
			var sample bool
			release, code, sample = r.controls.admit("read")
			if code != "" {
				s.rejected(req.ID, code, sample, "read")
				continue
			}
		}
		prepared, rejected := r.prepare(req)
		if rejected == nil && prepared.admissionRejection != nil {
			rejected = prepared.admissionRejection
		}
		if rejected != nil {
			s.respond(ctx, *rejected)
			if release != nil {
				release()
			}
			continue
		}
		taskCtx, taskCancel := context.WithCancel(ctx)
		s.mu.Lock()
		conflict := s.calls[req.ID] != nil || s.owners[req.ID].files != nil
		capacity := s.capacity(req.ID)
		accepted := !conflict && capacity && s.inflight.add(req.ID, taskCancel)
		s.mu.Unlock()
		if !accepted {
			taskCancel()
			code := "CONFLICT"
			if !capacity {
				code = "BUSY"
			}
			s.respond(ctx, protocol.Response{ID: req.ID, Error: inputFailure(code)})
			if release != nil {
				release()
			}
			continue
		}
		closeTask := func() {
			taskCancel()
			if release != nil {
				release()
			}
		}
		task := requestTask{id: req.ID, ctx: taskCtx, prepared: prepared, cancel: closeTask}
		select {
		case queue <- task:
		default:
			s.inflight.finish(req.ID)
			closeTask()
			s.respond(ctx, protocol.Response{ID: req.ID, Error: inputFailure("BUSY")})
		}
	}
}
func inputFailure(code string) *contract.ErrorValue {
	return &contract.ErrorValue{Code: code, Message: executionMessage(code), Outcome: "not_started"}
}
func (s *transportSession) send(ctx context.Context, value any, priority bool, transfer string) error {
	return s.scheduler.Submit(ctx, priority, transfer, func() error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return s.conn.WriteTextWithDeadline(protocol.MustJSON(value), agentResponseWriteTimeout)
	})
}
func (s *transportSession) sendTransferFrame(ctx context.Context, f protocol.Frame, priority bool, transfer string, grant func()) error {
	return s.scheduler.Submit(ctx, priority, transfer, func() error {
		return s.conn.WriteTextWithDeadlineStart(protocol.MustJSON(f), agentResponseWriteTimeout, func() error {
			if e := ctx.Err(); e != nil {
				return e
			}
			if grant != nil {
				grant()
			}
			return nil
		})
	})
}
func (s *transportSession) sendFrame(ctx context.Context, f protocol.Frame, priority bool, transfer string) error {
	if f.Type == "input_accept" {
		return s.scheduler.Submit(ctx, true, "", func() error {
			return s.conn.WriteTextWithDeadlineStart(protocol.MustJSON(f), agentResponseWriteTimeout, func() error {
				if e := ctx.Err(); e != nil {
					return e
				}
				return s.transfers.Grant(f.TransferID)
			})
		})
	}
	return s.send(ctx, f, priority, transfer)
}
func (s *transportSession) respond(ctx context.Context, response protocol.Response) {
	payload := protocol.MustJSON(response)
	if response.Type == "metadata" {
		if len(payload) > protocol.ManagementBytes {
			payload = protocol.MustJSON(protocol.Response{ID: response.ID, Error: inputFailure("LIMIT_EXCEEDED")})
		} else if s.runner.controls.consume(ctx, len(payload)) != nil {
			return
		}
	}
	_, e := s.scheduler.Post(s.ctx, response.Error != nil, "", func() error { return s.conn.WriteTextWithDeadline(payload, agentResponseWriteTimeout) })
	if e != nil && s.ctx.Err() == nil {
		s.cancel()
		s.conn.Close()
	}
}
func (s *transportSession) rejected(id, code string, sample bool, class string) {
	if sample {
		s.runner.detailError("", code, executionMessage(code), "management "+class+" limit exceeded", id)
	}
	s.respond(s.ctx, protocol.Response{ID: id, Error: inputFailure(code)})
}
func (s *transportSession) dispatch(f protocol.Frame) {
	class := "write"
	switch f.Type {
	case "input_open", "transfer_open":
		class = "transfer"
	case "cancel", "transfer_cancel":
		class = "cancel"
	}
	release, code, sample := s.runner.controls.admit(class)
	if code != "" {
		s.rejected(f.ID, code, sample, class)
		return
	}
	var pending *pendingTransfer
	finish := func() {}
	if f.Type == "transfer_open" {
		s.mu.Lock()
		if s.opens[f.TransferID] != nil {
			s.mu.Unlock()
			release()
			s.cancel()
			s.conn.Close()
			return
		}
		timeout, _ := time.ParseDuration(s.runner.versioned.Runtime()["transfer_timeout"].(string))
		ctx, cancel := context.WithTimeout(s.ctx, timeout)
		pending = &pendingTransfer{request: f.ID, ctx: ctx, cancel: cancel}
		s.opens[f.TransferID] = pending
		s.mu.Unlock()
		var finished sync.Once
		finish = func() {
			finished.Do(func() {
				pending.cancel()
				s.mu.Lock()
				if s.opens[f.TransferID] == pending {
					delete(s.opens, f.TransferID)
				}
				s.mu.Unlock()
			})
		}
	}
	if f.Type == "input_open" {
		s.mu.Lock()
		if c := s.calls[f.ID]; c != nil && !c.executing && !c.terminal {
			c.opening++
		}
		s.mu.Unlock()
	}
	var opened sync.Once
	finishOpening := func() {
		opened.Do(func() {
			if f.Type == "input_open" {
				s.mu.Lock()
				if c := s.calls[f.ID]; c != nil && c.opening > 0 {
					c.opening--
				}
				s.mu.Unlock()
			}
		})
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer finishOpening()
		if class != "transfer" {
			defer release()
		}
		if f.Type != "cancel" && f.Type != "transfer_cancel" {
			timeout, _ := time.ParseDuration(s.runner.versioned.Runtime()["transfer_timeout"].(string))
			budget, cancel := context.WithTimeout(s.ctx, timeout)
			if pending != nil {
				cancel()
				budget = pending.ctx
				cancel = func() {}
			}
			if f.Type == "input_open" || f.Type == "request_commit" {
				s.mu.Lock()
				call := s.calls[f.ID]
				s.mu.Unlock()
				if call != nil {
					cancel()
					// Management belongs to the original input phase even if an
					// earlier commit has already replaced the worker context.
					budget, cancel = context.WithDeadline(call.ctx, call.request.Deadline())
				}
			}
			defer cancel()
			if e := s.runner.controls.consume(budget, len(protocol.MustJSON(f))); e != nil {
				if class == "transfer" {
					finish()
					release()
				}
				s.respond(s.ctx, protocol.Response{ID: f.ID, Error: inputFailure("EXECUTION_TIMEOUT")})
				return
			}
		}
		switch f.Type {
		case "request_begin":
			s.begin(f)
		case "input_open":
			s.openInput(f, release, finishOpening)
		case "request_commit":
			s.commit(f)
		case "transfer_open":
			s.openOutput(f, release, pending.ctx, finish)
		case "cancel":
			s.inflight.cancel(f.ID)
			s.mu.Lock()
			c := s.calls[f.ID]
			s.mu.Unlock()
			if c != nil {
				s.abort(c, "", false)
			}
		case "transfer_cancel":
			s.mu.Lock()
			op := s.opens[f.TransferID]
			s.mu.Unlock()
			if op != nil && op.request == f.ID {
				op.cancel()
				return
			}
			e := s.transfers.Handle(f)
			if errors.Is(e, ws.ErrNotFound) {
				s.respond(s.ctx, protocol.Response{ID: f.ID, Error: inputFailure("NOT_FOUND")})
			} else if e != nil {
				s.cancel()
				s.conn.Close()
			}
		}
	}()
}
func (s *transportSession) sweep() {
	now := time.Now()
	s.mu.Lock()
	var expire []*inputCall
	for id, c := range s.calls {
		if c.terminal && !now.Before(c.until) {
			delete(s.calls, id)
		} else if !c.executing && !c.terminal && (c.ctx.Err() != nil || !now.Before(c.request.Deadline())) {
			expire = append(expire, c)
		}
	}
	for id, o := range s.owners {
		if !now.Before(o.expires) {
			delete(s.owners, id)
		}
	}
	s.mu.Unlock()
	for _, c := range expire {
		s.abort(c, "EXECUTION_TIMEOUT", true)
	}
	if s.runner.store != nil {
		if e := s.runner.store.Sweep(); e != nil {
			s.cancel()
			s.conn.Close()
		}
	}
}
func (s *transportSession) begin(f protocol.Frame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls[f.ID] != nil || s.owners[f.ID].files != nil || s.inflight.contains(f.ID) {
		s.respond(s.ctx, protocol.Response{ID: f.ID, Error: inputFailure("CONFLICT")})
		return
	}
	if !s.capacity(f.ID) {
		s.respond(s.ctx, protocol.Response{ID: f.ID, Error: inputFailure("BUSY")})
		return
	}
	snapshot, ok := s.runner.versioned.Snapshot(f.Capability)
	if !ok {
		s.respond(s.ctx, protocol.Response{ID: f.ID, Error: inputFailure("NOT_FOUND")})
		return
	}
	if snapshot.DefinitionHash() != f.DefinitionHash {
		s.respond(s.ctx, protocol.Response{ID: f.ID, Error: inputFailure("CONFLICT")})
		return
	}
	cap := snapshot.Definition()
	if !schemaHasFile(cap.Input) || s.runner.store == nil {
		s.respond(s.ctx, protocol.Response{ID: f.ID, Error: inputFailure("INVALID_REQUEST")})
		return
	}
	if rate := s.runner.controller.rates[f.Capability]; rate != nil {
		if ok, _ := rate.take(time.Now()); !ok {
			s.respond(s.ctx, protocol.Response{ID: f.ID, Error: inputFailure("RATE_LIMITED")})
			return
		}
	}
	owner := artifacts.Owner{RequestID: f.ID, Capability: f.Capability, CallerBinding: f.CallerBinding, DefinitionHash: f.DefinitionHash, AuthGeneration: s.generation}
	ctx, cancel := context.WithCancel(s.ctx)
	request, e := s.runner.store.BeginInput(ctx, owner, cap, f.InputDeadline)
	if e != nil {
		cancel()
		s.respond(s.ctx, protocol.Response{ID: f.ID, Error: inputFailure(artifactCode(e))})
		return
	}
	runtime := s.runner.versioned.Runtime()
	transfer, _ := time.ParseDuration(runtime["transfer_timeout"].(string))
	execution, _ := time.ParseDuration(cap.Policy["timeout"].(string))
	c := &inputCall{owner: owner, request: request, cap: cap, ctx: ctx, cancel: cancel, files: map[string]contract.FileValue{}, until: request.Deadline().Add(execution + transfer)}
	s.calls[f.ID] = c
	s.sendFrame(s.ctx, protocol.Frame{Type: "request_accept", ID: f.ID}, true, "")
}
func artifactCode(e error) string {
	var a *artifacts.Error
	if errors.As(e, &a) {
		return a.Code
	}
	if errors.Is(e, context.DeadlineExceeded) || errors.Is(e, context.Canceled) {
		return "EXECUTION_TIMEOUT"
	}
	return "EXECUTION_FAILED"
}
func (s *transportSession) abort(c *inputCall, code string, notify bool) {
	s.mu.Lock()
	if c.terminal {
		s.mu.Unlock()
		return
	}
	c.terminal = true
	c.cancel()
	c.request.Abort(c.owner)
	executing := c.executing
	s.mu.Unlock()
	s.transfers.CancelRequest(c.owner.RequestID)
	if !executing {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.transfers.WaitRequest(c.owner.RequestID)
			if e := c.request.ReleaseStopped(c.owner); e != nil {
				s.cancel()
				s.conn.Close()
			}
		}()
	}
	if notify && code != "" {
		s.respond(s.ctx, protocol.Response{ID: c.owner.RequestID, Error: inputFailure(code)})
	}
}
func (s *transportSession) openInput(f protocol.Frame, release, opened func()) {
	handedOff := false
	defer func() {
		if !handedOff {
			release()
		}
	}()
	s.mu.Lock()
	c := s.calls[f.ID]
	if c == nil || c.terminal || c.executing {
		s.mu.Unlock()
		s.respond(s.ctx, protocol.Response{ID: f.ID, Error: inputFailure("CONFLICT")})
		return
	}
	s.mu.Unlock()
	slot, ok := s.runner.controls.transferSlot()
	if !ok {
		s.respond(s.ctx, protocol.Response{ID: f.ID, Error: inputFailure("BUSY")})
		return
	}
	var stage *artifacts.Stage
	ctx, e := c.request.Context(c.owner)
	if e != nil {
		slot()
		s.abort(c, artifactCode(e), true)
		return
	}
	var ready protocol.Frame
	done, e := s.transfers.ReceiveReservedDone(ctx, f.ID, f.TransferID, func(b []byte) error {
		if e := s.runner.controls.consume(ctx, len(b)); e != nil {
			return e
		}
		_, e := stage.Write(b)
		return e
	}, func(end protocol.Frame) error {
		if e := stage.Close(); e != nil {
			return e
		}
		v, e := c.request.Complete(c.owner, stage, end.Size, end.SHA256)
		if e != nil {
			return e
		}
		s.mu.Lock()
		c.files[v.ID] = v
		s.mu.Unlock()
		ready = protocol.Frame{Type: "input_ready", ID: f.ID, TransferID: f.TransferID, FileID: v.ID, SizeBytes: v.SizeBytes, SHA256: v.SHA256}
		return nil
	}, func(e error) { slot(); release(); s.abort(c, artifactCode(e), true) })
	if e != nil {
		slot()
		if errors.Is(e, ws.ErrCapacity) {
			s.respond(s.ctx, protocol.Response{ID: f.ID, Error: inputFailure("BUSY")})
		} else {
			s.cancel()
			s.conn.Close()
		}
		return
	}
	handedOff = true
	stage, e = c.request.NewFile(c.owner, f.Pointer, f.Name, f.MIMEType)
	if e != nil {
		s.abort(c, artifactCode(e), true)
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		e := <-done
		slot()
		release()
		if e == nil && s.sendFrame(ctx, ready, true, "") != nil {
			s.abort(c, "EXECUTION_TIMEOUT", true)
		}
	}()
	opened()
	if s.sendFrame(ctx, protocol.Frame{Type: "input_accept", ID: f.ID, TransferID: f.TransferID}, true, "") != nil {
		s.abort(c, "EXECUTION_TIMEOUT", true)
	}
}
func (s *transportSession) commit(f protocol.Frame) {
	s.mu.Lock()
	c := s.calls[f.ID]
	if c == nil || c.terminal || c.executing {
		s.mu.Unlock()
		s.respond(s.ctx, protocol.Response{ID: f.ID, Error: inputFailure("CONFLICT")})
		return
	}
	if c.opening > 0 {
		s.mu.Unlock()
		s.abort(c, "VALIDATION_FAILED", true)
		return
	}
	params, e := replaceInputFiles(f.Params, f.Files, c.files)
	if e == nil {
		params, e = c.request.Commit(c.owner, params)
	}
	if e != nil {
		s.mu.Unlock()
		s.abort(c, "VALIDATION_FAILED", true)
		return
	}
	c.executing = true
	s.mu.Unlock()
	var scope *artifacts.OutputScope
	if c.cap.Output.Files != nil {
		p := c.cap.Policy
		ttl, _ := time.ParseDuration(p["file_ttl"].(string))
		scope, e = s.runner.store.Output(c.owner, artifacts.OutputLimits{MaxFileBytes: p["max_output_file_bytes"].(int64), MaxTotalBytes: p["max_total_output_file_bytes"].(int64), MaxFiles: p["max_output_files"].(int64), TTL: ttl})
		if e != nil {
			s.abort(c, artifactCode(e), true)
			c.request.ReleaseStopped(c.owner)
			return
		}
	}
	execution, _ := time.ParseDuration(c.cap.Policy["timeout"].(string))
	ctx, cancel := context.WithDeadline(c.ctx, time.Now().Add(execution))
	defer cancel()
	out, failure := s.runner.controller.Execute(ctx, ExecutionRequest{ID: f.ID, Capability: c.owner.Capability, DefinitionHash: c.owner.DefinitionHash, Params: params, Deadline: deadlineOfContext(ctx), admitted: true, input: c.request, owner: c.owner, stager: scope})
	if out != nil {
		defer out.Close()
	}
	c.request.ReleaseStopped(c.owner)
	s.mu.Lock()
	c.terminal = true
	s.mu.Unlock()
	if failure != nil {
		s.respond(s.ctx, protocol.Response{ID: f.ID, Error: failure})
		return
	}
	descriptors := []contract.PublicFileDescriptor{}
	if scope != nil {
		retained, e := scope.Retain(out.Files)
		if e != nil {
			s.respond(s.ctx, protocol.Response{ID: f.ID, Error: inputFailure(artifactCode(e))})
			return
		}
		owner := retainedOwner{owner: c.owner, files: map[string]artifacts.Descriptor{}}
		for _, d := range retained {
			owner.files[d.ID] = d
			owner.expires = d.ExpiresAt
			descriptors = append(descriptors, contract.PublicFileDescriptor{ID: d.ID, ArtifactMetadata: d.ArtifactMetadata, ExpiresAt: d.ExpiresAt.UTC().Format(time.RFC3339Nano)})
		}
		if len(retained) > 0 {
			s.mu.Lock()
			s.owners[f.ID] = owner
			s.mu.Unlock()
		}
	}
	data, _ := contract.CanonicalJSON(out.Data)
	var inputBytes int64
	for _, v := range c.files {
		inputBytes += v.SizeBytes
	}
	resp := protocol.ResultResponse(f.ID, contract.ResultValue{Data: out.Data, Files: descriptors})
	resp.Metrics = &protocol.Metrics{ExecutorKind: c.cap.Executor.Type, ReturnedRows: out.ReturnedRows, AffectedRows: out.AffectedRows, FileCount: int64(len(descriptors)), ResponseBytes: int64(len(data)), InputFileCount: int64(len(c.files)), InputFileBytes: inputBytes}
	s.respond(s.ctx, resp)
}
func deadlineOfContext(ctx context.Context) time.Time { d, _ := ctx.Deadline(); return d }
func (s *transportSession) openOutput(f protocol.Frame, transferRelease func(), ctx context.Context, finish func()) {
	handedOff := false
	defer func() {
		if !handedOff {
			transferRelease()
			finish()
		}
	}()
	release, code, sample := s.runner.controls.admit("read")
	if code != "" {
		s.rejected(f.ID, code, sample, "read")
		return
	}
	defer release()
	s.mu.Lock()
	owner, ok := s.owners[f.ID]
	descriptor, found := owner.files[f.FileID]
	s.mu.Unlock()
	if !ok || !found || !time.Now().Before(descriptor.ExpiresAt) {
		s.respond(s.ctx, protocol.Response{ID: f.ID, Error: inputFailure("NOT_FOUND")})
		return
	}
	slot, ok := s.runner.controls.transferSlot()
	if !ok {
		s.respond(s.ctx, protocol.Response{ID: f.ID, Error: inputFailure("BUSY")})
		return
	}
	done, e := s.transfers.SendOpen(ctx, f.ID, f.TransferID, func() (io.ReadCloser, error) {
		return s.runner.store.OpenOutput(ctx, owner.owner, f.FileID)
	}, s.runner.controls.consume)
	if e != nil {
		slot()
		if errors.Is(e, ws.ErrProtocol) {
			s.cancel()
			s.conn.Close()
		} else if errors.Is(e, ws.ErrCapacity) {
			s.respond(s.ctx, protocol.Response{ID: f.ID, Error: inputFailure("BUSY")})
		} else {
			s.respond(s.ctx, protocol.Response{ID: f.ID, Error: inputFailure(artifactCode(e))})
		}
		return
	}
	handedOff = true
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer finish()
		defer slot()
		defer transferRelease()
		if e := <-done; e != nil && !errors.Is(e, context.Canceled) && !errors.Is(e, io.EOF) {
			s.respond(s.ctx, protocol.Response{ID: f.ID, Error: inputFailure(artifactCode(e))})
		}
	}()
}

// capacity reserves the output ownership table before normal execution. Caller
// holds s.mu; in-flight IDs also reserve a slot until their result is retained.
func (s *transportSession) capacity(id string) bool {
	ids := map[string]bool{}
	for k := range s.calls {
		ids[k] = true
	}
	for k := range s.owners {
		ids[k] = true
	}
	s.inflight.mu.Lock()
	for k := range s.inflight.cancels {
		ids[k] = true
	}
	s.inflight.mu.Unlock()
	return ids[id] || len(ids) < s.recentMax
}
