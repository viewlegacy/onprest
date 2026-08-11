package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/viewlegacy/onprest/internal/protocol"
	"github.com/viewlegacy/onprest/internal/ws"
)

const (
	agentWriteQueueSize = 128
	metaFetchAttempts   = 5
	metaInitialBackoff  = 100 * time.Millisecond
)

type agentCallResult struct {
	Payload []byte
	Status  int
	Code    string
	Message string
	Count   *int64
}

func (r agentCallResult) OK() bool { return r.Status == http.StatusOK }

func (s *Server) handleAgentWS(w http.ResponseWriter, r *http.Request) {
	if !s.authenticateAgent(r) {
		writeJSON(w, http.StatusUnauthorized, apiError(errGatewayAuthFailed, "invalid agent signature"))
		return
	}
	s.agentMu.Lock()
	if s.agent != nil {
		s.agentMu.Unlock()
		s.log("agent_rejected", map[string]any{"reason": "already_connected", "remote": r.RemoteAddr})
		writeJSON(w, http.StatusConflict, apiError(errGatewayAgentAlreadyConn, "agent already connected"))
		return
	}
	conn, err := ws.Accept(w, r)
	if err != nil {
		s.agentMu.Unlock()
		s.log("agent_upgrade_failed", map[string]any{"error": err.Error()})
		return
	}
	ac := newAgentConn(conn)
	s.agent = ac
	s.openapi = nil
	s.responseKinds = nil
	s.agentMu.Unlock()
	s.log("agent_connected", map[string]any{"remote": r.RemoteAddr})
	go s.writeAgent(ac)
	go s.keepAgentAlive(ac)
	go s.fetchMetaFor(ac)
	s.readAgent(ac)
}

func newAgentConn(conn textConn) *agentConn {
	return &agentConn{
		conn:       conn,
		pending:    map[string]chan protocol.Response{},
		send:       make(chan agentWrite, agentWriteQueueSize),
		control:    make(chan agentWrite, agentWriteQueueSize),
		sendStates: map[string]*requestSendState{},
		done:       make(chan struct{}),
	}
}

func (s *Server) callAgent(ctx context.Context, capability string, params map[string]any) agentCallResult {
	s.agentMu.RLock()
	ac := s.agent
	s.agentMu.RUnlock()
	return s.callAgentOn(ctx, ac, capability, params)
}

func (s *Server) callAgentOn(ctx context.Context, ac *agentConn, capability string, params map[string]any) agentCallResult {
	if ac == nil || ac.conn == nil {
		return agentOfflineResult()
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.AgentTimeout)
	defer cancel()

	id := newID()
	ch := make(chan protocol.Response, 1)
	ac.mu.Lock()
	if ac.closed {
		ac.mu.Unlock()
		return agentOfflineResult()
	}
	if ac.pending == nil {
		ac.pending = map[string]chan protocol.Response{}
	}
	if ac.sendStates == nil {
		ac.sendStates = map[string]*requestSendState{}
	}
	ac.pending[id] = ch
	ac.sendStates[id] = &requestSendState{phase: "queued"}
	ac.mu.Unlock()

	payload := protocol.MustJSON(protocol.Request{ID: id, Capability: capability, Params: params})
	if ac.send == nil {
		// Test adapters created before the production writer queue is started use
		// the same pending lifecycle, but perform the in-memory write directly.
		if err := ac.conn.WriteText(payload); err != nil {
			ac.removePending(id)
			return agentOfflineResult()
		}
	} else {
		writeResult := make(chan error, 1)
		write := agentWrite{id: id, payload: payload, result: writeResult, ctx: ctx}
		select {
		case ac.send <- write:
		case <-ac.done:
			ac.removePending(id)
			return agentOfflineResult()
		case <-ctx.Done():
			ac.removePending(id)
			s.requestAgentCancel(ac, id)
			return agentTimeoutResult()
		}
		select {
		case err := <-writeResult:
			if err != nil {
				ac.removePending(id)
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return agentTimeoutResult()
				}
				_ = ac.conn.Close()
				return agentOfflineResult()
			}
		case <-ac.done:
			ac.removePending(id)
			return agentOfflineResult()
		case <-ctx.Done():
			ac.removePending(id)
			s.requestAgentCancel(ac, id)
			return agentTimeoutResult()
		}
	}

	select {
	case resp := <-ch:
		ac.finishSendState(id)
		if resp.Error != nil {
			status, code := agentErrorStatus(resp.Error.Code)
			return agentCallResult{Status: status, Code: code, Message: resp.Error.Message}
		}
		if capability == "meta" {
			return agentCallResult{Payload: resp.Result, Status: http.StatusOK}
		}
		kind := s.responseKind(capability)
		count, invalid := extractCount(resp.Result)
		if invalid {
			if kind == responseKindSelect {
				return agentCallResult{Status: http.StatusBadGateway, Code: errAgentInternal, Message: "agent internal error"}
			}
			return agentCallResult{Status: http.StatusBadGateway, Code: errAgentTransactionOutcomeUnknown, Message: "transaction outcome is unknown"}
		}
		return agentCallResult{Payload: resp.Result, Status: http.StatusOK, Count: count}
	case <-ctx.Done():
		ac.removePending(id)
		s.requestAgentCancel(ac, id)
		return agentTimeoutResult()
	}
}

func (s *Server) responseKind(capability string) responseKind {
	s.agentMu.RLock()
	defer s.agentMu.RUnlock()
	if kind, ok := s.responseKinds[capability]; ok {
		return kind
	}
	return responseKindUnknown
}

func extractCount(payload []byte) (*int64, bool) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(payload, &object); err != nil {
		return nil, true
	}
	raw, ok := object["count"]
	if !ok {
		return nil, true
	}
	var number json.Number
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&number); err != nil {
		return nil, true
	}
	count, err := number.Int64()
	if err != nil || count < 0 {
		return nil, true
	}
	return &count, false
}

func agentOfflineResult() agentCallResult {
	return agentCallResult{Status: http.StatusServiceUnavailable, Code: errGatewayAgentOffline, Message: "agent is not connected"}
}

func agentTimeoutResult() agentCallResult {
	return agentCallResult{Status: http.StatusGatewayTimeout, Code: errGatewayTimeout, Message: "agent response timed out"}
}

func (ac *agentConn) removePending(id string) {
	ac.mu.Lock()
	delete(ac.pending, id)
	ac.mu.Unlock()
}

func (ac *agentConn) finishSendState(id string) {
	ac.mu.Lock()
	delete(ac.sendStates, id)
	ac.mu.Unlock()
}

func (s *Server) requestAgentCancel(ac *agentConn, id string) {
	ac.mu.Lock()
	state := ac.sendStates[id]
	if state == nil {
		ac.mu.Unlock()
		return
	}
	switch state.phase {
	case "queued":
		state.phase = "canceled"
		delete(ac.sendStates, id)
		ac.mu.Unlock()
		return
	case "sending":
		state.cancelAfterSend = true
		ac.mu.Unlock()
		return
	case "sent":
		state.phase = "canceling"
	case "canceling", "canceled":
		ac.mu.Unlock()
		return
	default:
		ac.mu.Unlock()
		return
	}
	ac.mu.Unlock()
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), s.cfg.AgentWriteTimeout)
	write := agentWrite{id: id, payload: protocol.MustJSON(protocol.CancelRequest{Type: "cancel", ID: id}), result: make(chan error, 1), ctx: cleanupCtx, control: true, cancel: cleanupCancel}
	select {
	case ac.control <- write:
	case <-ac.done:
		cleanupCancel()
		ac.finishSendState(id)
	default:
		cleanupCancel()
		ac.finishSendState(id)
		_ = ac.conn.Close()
	}
}

func (s *Server) writeAgent(ac *agentConn) {
	for {
		var write agentWrite
		select {
		case write = <-ac.control:
		default:
			select {
			case write = <-ac.control:
			case write = <-ac.send:
			case <-ac.done:
				return
			}
		}
		if !write.control {
			ac.mu.Lock()
			state := ac.sendStates[write.id]
			if state == nil || state.phase == "canceled" {
				ac.mu.Unlock()
				write.result <- context.Canceled
				continue
			}
			state.phase = "sending"
			ac.mu.Unlock()
		}
		select {
		case <-write.ctx.Done():
			write.result <- write.ctx.Err()
			ac.finishSendState(write.id)
			if write.cancel != nil {
				write.cancel()
			}
			if write.control {
				_ = ac.conn.Close()
				return
			}
			continue
		default:
		}
		err := ac.conn.WriteTextWithDeadline(write.payload, s.cfg.AgentWriteTimeout)
		write.result <- err
		if write.cancel != nil {
			write.cancel()
		}
		if err != nil {
			ac.finishSendState(write.id)
			_ = ac.conn.Close()
			return
		}
		if write.control {
			ac.finishSendState(write.id)
			continue
		}
		ac.mu.Lock()
		state := ac.sendStates[write.id]
		cancelAfter := state != nil && state.cancelAfterSend
		if state != nil {
			state.phase = "sent"
		}
		ac.mu.Unlock()
		if cancelAfter {
			s.requestAgentCancel(ac, write.id)
		}
	}
}

func (s *Server) keepAgentAlive(ac *agentConn) {
	conn, ok := ac.conn.(deadlineTextConn)
	if !ok {
		return
	}
	refreshReadDeadline := func() {
		_ = conn.SetReadDeadline(time.Now().Add(s.cfg.AgentPingInterval + s.cfg.AgentPongTimeout))
	}
	conn.SetPongHandler(refreshReadDeadline)
	refreshReadDeadline()
	ticker := time.NewTicker(s.cfg.AgentPingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := conn.WritePingWithDeadline([]byte("onprest-keepalive"), s.cfg.AgentWriteTimeout); err != nil {
				_ = conn.Close()
				return
			}
		case <-ac.done:
			return
		}
	}
}

func (s *Server) readAgent(ac *agentConn) {
	defer func() {
		ac.disconnectPending()
		_ = ac.conn.Close()
		s.agentMu.Lock()
		if s.agent == ac {
			s.agent = nil
			s.openapi = nil
			s.responseKinds = nil
		}
		s.agentMu.Unlock()
		s.log("agent_disconnected", nil)
	}()
	for {
		msg, err := ac.conn.ReadText()
		if err != nil {
			return
		}
		var resp protocol.Response
		if err := json.Unmarshal(msg, &resp); err != nil {
			s.log("agent_bad_message", map[string]any{"error": err.Error()})
			continue
		}
		ac.mu.Lock()
		ch := ac.pending[resp.ID]
		delete(ac.pending, resp.ID)
		delete(ac.sendStates, resp.ID)
		ac.mu.Unlock()
		if ch != nil {
			ch <- resp
		}
	}
}

func (ac *agentConn) disconnectPending() {
	ac.mu.Lock()
	if ac.closed {
		ac.mu.Unlock()
		return
	}
	ac.closed = true
	pending := ac.pending
	ac.pending = map[string]chan protocol.Response{}
	ac.sendStates = map[string]*requestSendState{}
	ac.mu.Unlock()
	ac.doneOnce.Do(func() {
		if ac.done != nil {
			close(ac.done)
		}
	})
	for _, ch := range pending {
		ch <- protocol.Response{Error: &protocol.Error{Code: errGatewayAgentOffline, Message: "agent is not connected"}}
	}
}

func (s *Server) fetchMeta() {
	s.agentMu.RLock()
	ac := s.agent
	s.agentMu.RUnlock()
	s.fetchMetaFor(ac)
}

func (s *Server) fetchMetaFor(ac *agentConn) {
	if ac == nil {
		s.log("openapi_fetch_failed", map[string]any{"attempt": 1, "code": errGatewayAgentOffline, "http_status": http.StatusServiceUnavailable})
		return
	}
	backoff := metaInitialBackoff
	for attempt := 1; attempt <= metaFetchAttempts; attempt++ {
		if !s.isCurrentAgent(ac) {
			return
		}
		result := s.callAgentOn(context.Background(), ac, "meta", map[string]any{})
		if result.OK() {
			doc, kinds, err := metaData(result.Payload)
			if err == nil {
				doc = s.finalizeOpenAPI(doc)
				s.agentMu.Lock()
				if s.agent != ac {
					s.agentMu.Unlock()
					return
				}
				s.openapi = doc
				s.responseKinds = kinds
				s.agentMu.Unlock()
				pathCount := openAPIPathCount(doc)
				s.log("openapi_cached", map[string]any{"paths": pathCount})
				if s.cfg.EmitOpenAPISnapshot {
					s.log("openapi_snapshot", map[string]any{"paths": pathCount, "openapi": doc})
				}
				return
			}
			s.log("openapi_parse_failed", map[string]any{"attempt": attempt, "error": err.Error()})
		} else {
			s.log("openapi_fetch_failed", map[string]any{"attempt": attempt, "code": result.Code, "http_status": result.Status})
		}
		if attempt == metaFetchAttempts {
			return
		}
		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-ac.done:
			if !timer.Stop() {
				<-timer.C
			}
			return
		}
		if backoff < 2*time.Second {
			backoff *= 2
		}
	}
}

func (s *Server) isCurrentAgent(ac *agentConn) bool {
	s.agentMu.RLock()
	defer s.agentMu.RUnlock()
	return s.agent == ac
}
