package gateway

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/viewlegacy/onprest/internal/protocol"
	"github.com/viewlegacy/onprest/internal/ws"
)

type agentCallResult struct {
	Payload []byte
	Status  int
	Code    string
	Detail  string
}

func (r agentCallResult) OK() bool {
	return r.Status == http.StatusOK
}

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
	ac := &agentConn{conn: conn, pending: map[string]chan protocol.Response{}}
	s.agent = ac
	s.agentMu.Unlock()
	s.log("agent_connected", map[string]any{"remote": r.RemoteAddr})
	go s.fetchMeta()
	s.readAgent(ac)
}

func (s *Server) callAgent(ctx context.Context, capability string, params map[string]any) agentCallResult {
	s.agentMu.RLock()
	ac := s.agent
	s.agentMu.RUnlock()
	if ac == nil {
		return agentCallResult{Status: http.StatusServiceUnavailable, Code: errGatewayAgentOffline, Detail: "agent is not connected"}
	}
	id := newID()
	ch := make(chan protocol.Response, 1)
	ac.mu.Lock()
	ac.pending[id] = ch
	err := ac.conn.WriteText(protocol.MustJSON(protocol.Request{ID: id, Capability: capability, Params: params}))
	ac.mu.Unlock()
	if err != nil {
		return agentCallResult{Status: http.StatusServiceUnavailable, Code: errGatewayAgentOffline, Detail: err.Error()}
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.AgentTimeout)
	defer cancel()
	select {
	case resp := <-ch:
		if resp.Error != nil {
			status, code := agentErrorStatus(resp.Error.Code)
			return agentCallResult{Status: status, Code: code, Detail: publicErrorMessage(code)}
		}
		return agentCallResult{Payload: resp.Result, Status: http.StatusOK}
	case <-ctx.Done():
		ac.mu.Lock()
		delete(ac.pending, id)
		ac.mu.Unlock()
		return agentCallResult{Status: http.StatusGatewayTimeout, Code: errGatewayTimeout, Detail: ctx.Err().Error()}
	}
}

func (s *Server) readAgent(ac *agentConn) {
	defer func() {
		_ = ac.conn.Close()
		s.agentMu.Lock()
		if s.agent == ac {
			s.agent = nil
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

func (s *Server) fetchMeta() {
	result := s.callAgent(context.Background(), "meta", map[string]any{})
	if !result.OK() {
		s.log("openapi_fetch_failed", map[string]any{"code": result.Code, "http_status": result.Status})
		return
	}
	doc, err := openAPIFromMeta(result.Payload)
	if err != nil {
		s.log("openapi_parse_failed", map[string]any{"error": err.Error()})
		return
	}
	s.agentMu.Lock()
	s.openapi = doc
	s.agentMu.Unlock()
	pathCount := openAPIPathCount(doc)
	s.log("openapi_cached", map[string]any{"paths": pathCount})
	if s.cfg.EmitOpenAPISnapshot {
		s.log("openapi_snapshot", map[string]any{"paths": pathCount, "openapi": doc})
	}
}
