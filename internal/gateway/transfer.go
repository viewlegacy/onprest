package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/viewlegacy/onprest/internal/protocol"
	"github.com/viewlegacy/onprest/internal/ws"
	"io"
	"sync"
	"time"
)

type onceReader struct {
	io.ReadCloser
	once sync.Once
}

func (r *onceReader) Close() (e error) { r.once.Do(func() { e = r.ReadCloser.Close() }); return }

type inputRejected string

func (e inputRejected) Error() string { return string(e) }

type inputExchange struct {
	events        chan protocol.Frame
	result        chan protocol.Response
	noInputEvents bool
}

// InputStream is private transport input. External parsers and authorization
// remain owned by their REST/MCP handlers; no upload endpoint is exposed here.
type InputStream struct {
	Pointer, Name, MIMEType string
	Reader                  io.ReadCloser
}

func (ac *agentConn) initializeTransfers() {
	ctx, cancel := context.WithCancel(context.Background())
	ac.transferCtx = ctx
	ac.transferCancel = cancel
	ac.scheduler = ws.NewScheduler(ctx)
	ac.transferCalls = map[string]*inputExchange{}
	ac.transferRecent = map[string]time.Time{}
	ac.transfers = ws.NewTransfers(ctx, func(ctx context.Context, f protocol.Frame, priority bool, transfer string, grant func()) error {
		return ac.scheduler.Submit(ctx, priority, transfer, func() error {
			return ac.conn.WriteTextWithDeadlineStart(protocol.MustJSON(f), 5*time.Second, func() error {
				if e := ctx.Err(); e != nil {
					return e
				}
				if grant != nil {
					grant()
				}
				return nil
			})
		})
	})
}
func (ac *agentConn) handleTransfer(raw []byte) error {
	f, e := protocol.DecodeFrame(raw)
	if e != nil {
		return e
	}
	switch f.Type {
	case "ack", "transfer_chunk", "transfer_end", "transfer_cancel":
		return ac.transfers.Handle(f)
	case "request_accept", "input_accept", "input_ready":
		ac.mu.Lock()
		exchange := ac.transferCalls[f.ID]
		recent := time.Now().Before(ac.transferRecent[f.ID])
		ignore := exchange == nil || exchange.noInputEvents
		ac.mu.Unlock()
		if ignore && recent {
			return nil
		}
		if exchange == nil {
			return ws.ErrProtocol
		}
		select {
		case exchange.events <- f:
			return nil
		default:
			return ws.ErrProtocol
		}
	default:
		return ws.ErrProtocol
	}
}
func (ac *agentConn) frame(ctx context.Context, f protocol.Frame) error {
	return ac.scheduler.Submit(ctx, true, "", func() error {
		return ac.conn.WriteTextWithDeadlineStart(protocol.MustJSON(f), 5*time.Second, func() error {
			if e := ctx.Err(); e != nil {
				return e
			}
			if f.Type == "transfer_open" {
				return ac.transfers.Grant(f.TransferID)
			}
			return nil
		})
	})
}
func (ac *agentConn) exchange(until time.Time, id string) (*inputExchange, func(), error) {
	return ac.newExchange(until, id, false)
}
func (ac *agentConn) exchangeInput(until time.Time, id string) (*inputExchange, func(), error) {
	return ac.newExchange(until, id, true)
}
func (ac *agentConn) newExchange(until time.Time, id string, input bool) (*inputExchange, func(), error) {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	now := time.Now()
	if until.IsZero() || !now.Before(until) {
		return nil, nil, errors.New("finite transfer deadline required")
	}
	for key, expiry := range ac.transferRecent {
		if !now.Before(expiry) {
			delete(ac.transferRecent, key)
		}
	}
	if input && now.Before(ac.transferRecent[id]) {
		return nil, nil, errors.New("CONFLICT")
	}
	if ac.closed || ac.transfers == nil || ac.transferCalls[id] != nil || ac.pending[id] != nil || ac.sendStates[id] != nil || len(ac.transferCalls) >= 32 || (ac.transferRecent[id].IsZero() && len(ac.transferRecent) >= 4096) {
		return nil, nil, errors.New("transfer capacity unavailable")
	}
	v := &inputExchange{events: make(chan protocol.Frame, 4), result: make(chan protocol.Response, 1)}
	// A new finite operation may cover an older operation's late control. Frame
	// traffic itself never renews either operation's fixed expiry.
	expiry := until.Add(5 * time.Second)
	if expiry.After(ac.transferRecent[id]) {
		ac.transferRecent[id] = expiry
	}
	ac.transferCalls[id] = v
	return v, func() { ac.mu.Lock(); delete(ac.transferCalls, id); ac.mu.Unlock() }, nil
}
func (v *inputExchange) event(ctx context.Context, ac *agentConn, kind, transfer string) (protocol.Frame, error) {
	select {
	case f := <-v.events:
		if f.Type != kind || transfer != "" && f.TransferID != transfer {
			return f, ws.ErrProtocol
		}
		return f, nil
	case r := <-v.result:
		if r.Error != nil {
			return protocol.Frame{}, inputRejected(r.Error.Code)
		}
		return protocol.Frame{}, ws.ErrProtocol
	case <-ctx.Done():
		return protocol.Frame{}, ctx.Err()
	case <-ac.transferCtx.Done():
		return protocol.Frame{}, errors.New("agent disconnected")
	}
}

// TransferInput drives the internal begin/commit barrier. Its input deadline is
// fixed by the caller at body admission, and never restarted for another file.
// The returned frame is internal; public rendering is added by later handlers.
func (s *Server) TransferInput(ctx context.Context, begin protocol.Frame, params map[string]any, streams []InputStream) (protocol.Response, error) {
	streams = append([]InputStream(nil), streams...)
	for i := range streams {
		if streams[i].Reader != nil {
			streams[i].Reader = &onceReader{ReadCloser: streams[i].Reader}
		}
	}
	defer func() {
		for _, stream := range streams {
			if stream.Reader != nil {
				stream.Reader.Close()
			}
		}
	}()
	s.agentMu.RLock()
	ac := s.agent
	metadata, known := s.capabilities[begin.Capability]
	ready := s.agent == ac && s.openapi != nil && s.capabilities != nil
	s.agentMu.RUnlock()
	if ac == nil {
		return protocol.Response{}, errors.New("agent offline")
	}
	if !ready {
		return protocol.Response{}, errors.New("OPENAPI_NOT_READY")
	}
	if !known {
		return protocol.Response{}, errors.New("NOT_FOUND")
	}
	if metadata.DefinitionHash != begin.DefinitionHash {
		return protocol.Response{}, errors.New("CONFLICT")
	}
	input, ie := time.ParseDuration(metadata.InputTimeout)
	execution, ee := time.ParseDuration(metadata.ExecutionTimeout)
	if ie != nil || ee != nil || input <= 0 || execution <= 0 || begin.InputDeadline.IsZero() {
		return protocol.Response{}, errors.New("INVALID_REQUEST")
	}
	maximum := time.Now().Add(input)
	if maximum.Before(begin.InputDeadline) {
		begin.InputDeadline = maximum
	}
	if !time.Now().Before(begin.InputDeadline) {
		return protocol.Response{}, context.DeadlineExceeded
	}
	exchange, release, e := ac.exchangeInput(begin.InputDeadline.Add(execution+input), begin.ID)
	if e != nil {
		return protocol.Response{}, e
	}
	defer release()
	inputCtx, cancel := context.WithDeadline(ctx, begin.InputDeadline)
	defer cancel()
	success, denied := false, false
	var beginMu sync.Mutex
	beginStarted, beginCanceled := false, false
	defer func() {
		beginMu.Lock()
		beginCanceled = true
		started := beginStarted
		beginMu.Unlock()
		if started && !denied && !success {
			ac.transfers.CancelRequest(begin.ID)
			cleanup, closeCleanup := context.WithTimeout(context.Background(), 5*time.Second)
			defer closeCleanup()
			if e := ac.scheduler.Submit(cleanup, true, "", func() error {
				return ac.conn.WriteTextWithDeadline(protocol.MustJSON(protocol.CancelRequest{Type: "cancel", ID: begin.ID}), 5*time.Second)
			}); e != nil {
				ac.conn.Close()
			}
		}
		for _, stream := range streams {
			if stream.Reader != nil {
				stream.Reader.Close()
			}
		}
		ac.transfers.WaitRequest(begin.ID)
	}()
	if e = ac.scheduler.Submit(inputCtx, true, "", func() error {
		beginMu.Lock()
		if beginCanceled || inputCtx.Err() != nil {
			beginMu.Unlock()
			return context.Canceled
		}
		beginStarted = true
		beginMu.Unlock()
		return ac.conn.WriteTextWithDeadline(protocol.MustJSON(begin), 5*time.Second)
	}); e != nil {
		return protocol.Response{}, e
	}
	if _, e = exchange.event(inputCtx, ac, "request_accept", ""); e != nil {
		var rejected inputRejected
		denied = errors.As(e, &rejected)
		return protocol.Response{}, e
	}
	bindings := []protocol.FileBinding{}
	for _, stream := range streams {
		if stream.Reader == nil {
			return protocol.Response{}, errors.New("nil input reader")
		}
		transfer := newID()
		f := protocol.Frame{Type: "input_open", ID: begin.ID, TransferID: transfer, Pointer: stream.Pointer, Name: stream.Name, MIMEType: stream.MIMEType}
		if e = ac.frame(inputCtx, f); e != nil {
			return protocol.Response{}, e
		}
		if _, e = exchange.event(inputCtx, ac, "input_accept", transfer); e != nil {
			return protocol.Response{}, e
		}
		done, e := ac.transfers.Send(inputCtx, begin.ID, transfer, stream.Reader, nil)
		if e != nil {
			return protocol.Response{}, e
		}
		select {
		case e = <-done:
			if e != nil {
				return protocol.Response{}, e
			}
		case <-inputCtx.Done():
			return protocol.Response{}, inputCtx.Err()
		}
		ready, e := exchange.event(inputCtx, ac, "input_ready", transfer)
		if e != nil {
			return protocol.Response{}, e
		}
		bindings = append(bindings, protocol.FileBinding{Pointer: stream.Pointer, FileID: ready.FileID})
	}
	if e = ac.frame(inputCtx, protocol.Frame{Type: "request_commit", ID: begin.ID, Params: params, Files: bindings}); e != nil {
		return protocol.Response{}, e
	}
	select {
	case r := <-exchange.result:
		success = true
		return r, nil
	case <-ctx.Done():
		return protocol.Response{}, ctx.Err()
	case <-ac.transferCtx.Done():
		return protocol.Response{}, errors.New("agent disconnected")
	}
}

// TransferOutput streams a previously retained artifact over the same outbound
// Agent connection. The caller supplies validated metadata and a finite budget.
// destination must stop its I/O when the supplied context is canceled.
func (s *Server) TransferOutput(ctx context.Context, requestID, fileID string, size int64, digest string, destination func(context.Context, []byte) (int, error)) (resultErr error) {
	if destination == nil || size < 0 || len(digest) != 64 {
		return errors.New("invalid output sink or metadata")
	}
	if _, ok := ctx.Deadline(); !ok {
		return errors.New("transfer deadline required")
	}
	s.agentMu.RLock()
	ac := s.agent
	s.agentMu.RUnlock()
	if ac == nil {
		return errors.New("agent offline")
	}
	exchange, release, e := ac.exchange(deadlineOf(ctx), requestID)
	if e != nil {
		return e
	}
	defer release()
	ac.mu.Lock()
	exchange.noInputEvents = true
	ac.mu.Unlock()
	id := newID()
	opened, denied := false, false
	defer func() {
		ac.transfers.Cancel(id)
		ac.transfers.Wait(id)
		if opened && !denied && resultErr != nil {
			cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			ac.frame(cleanup, protocol.Frame{Type: "transfer_cancel", ID: requestID, TransferID: id})
		}
	}()
	hash := sha256.New()
	var received int64
	finished := make(chan error, 1)
	complete := func(e error) {
		select {
		case finished <- e:
		default:
		}
	}
	e = ac.transfers.ReceiveReserved(ctx, requestID, id, func(body []byte) error {
		if int64(len(body)) > size-received {
			return errors.New("OUTPUT_INVALID")
		}
		n, e := destination(ctx, body)
		if e == nil && n != len(body) {
			e = io.ErrShortWrite
		}
		if e == nil {
			hash.Write(body)
			received += int64(n)
		}
		return e
	}, func(end protocol.Frame) error {
		if end.Size != size || received != size || end.SHA256 != digest || hex.EncodeToString(hash.Sum(nil)) != digest {
			e := errors.New("OUTPUT_INVALID")
			complete(e)
			return e
		}
		complete(nil)
		return nil
	}, complete)
	if e != nil {
		return e
	}
	opened = true
	if e = ac.frame(ctx, protocol.Frame{Type: "transfer_open", ID: requestID, TransferID: id, FileID: fileID}); e != nil {
		ac.transfers.Cancel(id)
		return e
	}
	select {
	case e := <-finished:
		return e
	case response := <-exchange.result:
		denied = true
		if response.Error != nil {
			return errors.New(response.Error.Code)
		}
		return ws.ErrProtocol
	case <-ctx.Done():
		return ctx.Err()
	case <-ac.transferCtx.Done():
		return errors.New("agent disconnected")
	}
}
