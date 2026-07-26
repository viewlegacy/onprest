package gateway

import (
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
	s.agentMu.Unlock()
	s.log("agent_connected", map[string]any{"remote": r.RemoteAddr})
	go s.writeAgent(ac)
	go s.keepAgentAlive(ac)
	go s.fetchMetaFor(ac)
	s.readAgent(ac)
}

func newAgentConn(conn textConn) *agentConn {
	return &agentConn{
		conn:    conn,
		pending: map[string]chan protocol.Response{},
		send:    make(chan agentWrite, agentWriteQueueSize),
		done:    make(chan struct{}),
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
	ac.pending[id] = ch
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
		write := agentWrite{payload: payload, result: writeResult, ctx: ctx}
		select {
		case ac.send <- write:
		case <-ac.done:
			ac.removePending(id)
			return agentOfflineResult()
		case <-ctx.Done():
			ac.removePending(id)
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
			return agentTimeoutResult()
		}
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			status, code := agentErrorStatus(resp.Error.Code)
			return agentCallResult{Status: status, Code: code, Message: resp.Error.Message}
		}
		return agentCallResult{Payload: resp.Result, Status: http.StatusOK}
	case <-ctx.Done():
		ac.removePending(id)
		return agentTimeoutResult()
	}
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

func (s *Server) writeAgent(ac *agentConn) {
	for {
		select {
		case write := <-ac.send:
			select {
			case <-write.ctx.Done():
				write.result <- write.ctx.Err()
				continue
			default:
			}
			if deadlineConn, ok := ac.conn.(interface{ SetWriteDeadline(time.Time) error }); ok {
				if err := deadlineConn.SetWriteDeadline(time.Now().Add(s.cfg.AgentWriteTimeout)); err != nil {
					write.result <- err
					_ = ac.conn.Close()
					return
				}
			}
			err := ac.conn.WriteText(write.payload)
			write.result <- err
			if err != nil {
				_ = ac.conn.Close()
				return
			}
		case <-ac.done:
			return
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
			if err := conn.SetWriteDeadline(time.Now().Add(s.cfg.AgentWriteTimeout)); err != nil {
				_ = conn.Close()
				return
			}
			if err := conn.WritePing([]byte("onprest-keepalive")); err != nil {
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
			doc, err := openAPIFromMeta(result.Payload)
			if err == nil {
				doc = s.finalizeOpenAPI(doc)
				s.agentMu.Lock()
				if s.agent != ac {
					s.agentMu.Unlock()
					return
				}
				s.openapi = doc
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
