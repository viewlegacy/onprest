package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"encoding/hex"
	"fmt"
	"github.com/viewlegacy/onprest/internal/contract"
	"github.com/viewlegacy/onprest/internal/protocol"
	"github.com/viewlegacy/onprest/internal/ws"
)

const (
	agentWriteQueueSize = 128
	metaFetchAttempts   = 5
	metaInitialBackoff  = 100 * time.Millisecond
)

type agentCallResult struct {
	Failure   *contract.ErrorValue
	Metrics   *protocol.Metrics
	RequestID string
	Payload   []byte
	Status    int
	Code      string
	Message   string
	Count     *int64
}

func (r agentCallResult) OK() bool { return r.Status == http.StatusOK }

func (s *Server) handleAgentWS(w http.ResponseWriter, r *http.Request) {
	if len(r.Header.Values(protocol.MajorHeader)) != 1 || r.Header.Get(protocol.MajorHeader) != protocol.Major {
		writeJSON(w, http.StatusBadRequest, requestAPIError(r, errGatewayInvalidRequest, "unsupported protocol major; require 1"))
		return
	}
	w.Header().Set(protocol.MajorHeader, protocol.Major)
	if !s.authenticateAgent(r) {
		writeJSON(w, http.StatusUnauthorized, requestAPIError(r, errGatewayAuthFailed, "invalid agent signature"))
		return
	}
	s.agentMu.Lock()
	if s.agent != nil {
		s.agentMu.Unlock()
		s.log("agent_rejected", map[string]any{"reason": "already_connected", "remote": r.RemoteAddr})
		writeJSON(w, http.StatusConflict, requestAPIError(r, errGatewayAgentAlreadyConn, "agent already connected"))
		return
	}
	conn, err := ws.Accept(w, r)
	if err != nil {
		s.agentMu.Unlock()
		s.log("agent_upgrade_failed", map[string]any{"error": err.Error()})
		return
	}
	wire := textConn(conn)
	if s.agentWire != nil {
		wire = s.agentWire(conn)
	}
	ac := newAgentConn(wire)
	ac.initializeTransfers()
	s.agent = ac
	s.openapi = nil
	s.responseKinds = nil
	s.capabilities = nil
	s.references = map[string]fileReference{}
	s.metadataError = ""
	s.agentMu.Unlock()
	s.log("agent_connected", map[string]any{"remote": r.RemoteAddr})
	go func() {
		if ac.scheduler.Run() != nil {
			ac.conn.Close()
		}
	}()
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

func (s *Server) callAgentOn(ctx context.Context, ac *agentConn, capability string, params map[string]any) (result agentCallResult) {
	identity, _ := ctx.Value(callIdentityKey{}).(callIdentity)
	id := identity.ID
	if id == "" {
		id = newID()
	}
	defer func() { result.RequestID = id }()
	if ac == nil || ac.conn == nil {
		return agentOfflineResult()
	}
	s.agentMu.RLock()
	metadata, known := s.capabilities[capability]
	ready := s.agent == ac && s.openapi != nil && s.capabilities != nil
	s.agentMu.RUnlock()
	if capability != "meta" {
		if !ready {
			s.agentMu.RLock()
			problem := s.metadataError
			s.agentMu.RUnlock()
			if problem == "ONPREST_PUBLIC_BASE_URL is required for file reference delivery" {
				return deliveryFailure("DEPENDENCY_UNAVAILABLE", problem, "not_started")
			}
			return agentOfflineResult()
		}
		if !known {
			failure := &contract.ErrorValue{Code: "NOT_FOUND", Message: "capability is not defined", Outcome: "not_started"}
			return agentCallResult{Status: http.StatusNotFound, Code: failure.Code, Message: failure.Message, Failure: failure}
		}
		if metadata.DefinitionHash == "" {
			return agentOfflineResult()
		}
	}
	budget := s.cfg.AgentTimeout
	if known {
		budget, _ = time.ParseDuration(metadata.ExecutionTimeout)
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

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

	payload := protocol.MustJSON(protocol.Request{Type: "request", ID: id, Capability: capability, Params: params, CallerBinding: identity.Binding, DefinitionHash: metadata.DefinitionHash, Deadline: deadlineOf(ctx)})
	if ac.send == nil {
		// Test adapters created before the production writer queue is started use
		// the same pending lifecycle, but perform the in-memory write directly.
		if err := ac.conn.WriteText(payload); err != nil {
			ac.removePending(id)
			return agentOfflineFor(ac, id)
		}
	} else {
		writeResult := make(chan error, 1)
		write := agentWrite{id: id, payload: payload, result: writeResult, ctx: ctx}
		select {
		case ac.send <- write:
		case <-ac.done:
			ac.removePending(id)
			return agentOfflineFor(ac, id)
		case <-ctx.Done():
			ac.removePending(id)
			failure := agentTimeoutFor(ac, id)
			s.requestAgentCancel(ac, id)
			return failure
		}
		select {
		case err := <-writeResult:
			if err != nil {
				ac.removePending(id)
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return agentTimeoutFor(ac, id)
				}
				_ = ac.conn.Close()
				return agentOfflineFor(ac, id)
			}
		case <-ac.done:
			ac.removePending(id)
			return agentOfflineFor(ac, id)
		case <-ctx.Done():
			ac.removePending(id)
			failure := agentTimeoutFor(ac, id)
			s.requestAgentCancel(ac, id)
			return failure
		}
	}

	select {
	case resp := <-ch:
		ac.finishSendState(id)
		if resp.Error != nil {
			status, code := agentErrorStatus(resp.Error.Code)
			if resp.Error.Validate() != nil {
				return invalidAgentResult(id)
			}
			return agentCallResult{RequestID: id, Status: status, Code: code, Message: resp.Error.Message, Failure: resp.Error}
		}
		if capability == "meta" {
			if resp.Type != "metadata" {
				return invalidAgentResult(id)
			}
			return agentCallResult{Payload: resp.Result, Status: http.StatusOK}
		}
		envelope, metrics, err := validateAgentResult(id, resp, metadata, known)
		if err != nil {
			s.log("agent_output_rejected", map[string]any{"request_id": id, "reason": err.Error()})
			return invalidAgentResult(id)
		}
		count := metrics.ReturnedRows
		if count == nil {
			count = metrics.AffectedRows
		}
		result := agentCallResult{RequestID: id, Payload: envelope, Status: http.StatusOK, Count: count, Metrics: metrics}
		if known && metadata.Output != nil {
			return s.registerFiles(ac, identity, capability, metadata, result)
		}
		return result
	case <-ctx.Done():
		ac.removePending(id)
		failure := agentTimeoutFor(ac, id)
		s.requestAgentCancel(ac, id)
		return failure
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
	return agentCallResult{Status: 503, Code: "AGENT_OFFLINE", Message: "agent is not connected", Failure: &contract.ErrorValue{Code: "AGENT_OFFLINE", Message: "agent is not connected", Outcome: "not_started"}}
}

func agentTimeoutResult() agentCallResult {
	return agentCallResult{Status: http.StatusGatewayTimeout, Code: errGatewayTimeout, Message: "agent response timed out", Failure: &contract.ErrorValue{Code: "EXECUTION_TIMEOUT", Message: "agent response timed out", Outcome: "unknown"}}
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
			ac.finishSendState(write.id)
			write.result <- write.ctx.Err()
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
		writeFrame := func() error { return ac.conn.WriteTextWithDeadline(write.payload, s.cfg.AgentWriteTimeout) }
		var err error
		if ac.scheduler != nil {
			var completed <-chan error
			completed, err = ac.scheduler.Post(write.ctx, write.control, "", writeFrame)
			if err == nil {
				select {
				case err = <-completed:
				case <-ac.done:
					err = errors.New("agent disconnected")
				}
			}
		} else {
			err = writeFrame()
		}
		if err != nil {
			ac.finishSendState(write.id)
			write.result <- err
			if write.cancel != nil {
				write.cancel()
			}
			if !write.control && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
				continue
			}
			_ = ac.conn.Close()
			return
		}
		write.result <- nil
		if write.cancel != nil {
			write.cancel()
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
			s.capabilities = nil
			s.references = nil
		}
		s.agentMu.Unlock()
		s.log("agent_disconnected", nil)
	}()
	for {
		msg, err := ac.conn.ReadText()
		if err != nil {
			return
		}
		if ac.transfers != nil {
			var kind struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(msg, &kind) != nil {
				return
			}
			if protocol.IsTransferType(kind.Type) {
				if ac.handleTransfer(msg) != nil {
					return
				}
				continue
			}
		}
		var resp protocol.Response
		if err := json.Unmarshal(msg, &resp); err != nil {
			s.log("agent_bad_message", map[string]any{"error": err.Error()})
			continue
		}
		ac.mu.Lock()
		if exchange := ac.transferCalls[resp.ID]; exchange != nil {
			ac.mu.Unlock()
			select {
			case exchange.result <- resp:
			default:
				return
			}
			continue
		}
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
	if ac.transferCancel != nil {
		ac.transferCancel()
	}
	ac.mu.Lock()
	if ac.closed {
		ac.mu.Unlock()
		return
	}
	ac.closed = true
	if ac.transfers != nil {
		defer ac.transfers.Close()
	}
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
		ch <- protocol.Response{Error: &protocol.Error{Code: errGatewayAgentOffline, Message: "agent is not connected", Outcome: "unknown"}}
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
			var metadata protocol.Metadata
			if err == nil {
				err = decodeJSONNumber(result.Payload, &metadata)
			}
			if err == nil {
				err = validateMetadata(metadata)
				if err == nil {
					err = s.validateFileOrigin(metadata)
				}
			}
			if err == nil {
				doc = s.finalizeOpenAPI(doc)
				s.agentMu.Lock()
				if s.agent != ac {
					s.agentMu.Unlock()
					return
				}
				s.openapi = doc
				s.responseKinds = kinds
				s.capabilities = metadata.Capabilities
				s.agentMu.Unlock()
				pathCount := openAPIPathCount(doc)
				s.log("openapi_cached", map[string]any{"paths": pathCount})
				if s.cfg.EmitOpenAPISnapshot {
					s.log("openapi_snapshot", map[string]any{"paths": pathCount, "openapi": doc})
				}
				return
			}
			s.agentMu.Lock()
			if s.agent == ac {
				s.metadataError = err.Error()
			}
			s.agentMu.Unlock()
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

type callIdentityKey struct{}
type callIdentity struct{ ID, Binding string }

func deadlineOf(ctx context.Context) time.Time { v, _ := ctx.Deadline(); return v }
func invalidAgentResult(id string) agentCallResult {
	e := &contract.ErrorValue{Code: "OUTPUT_INVALID", Message: "agent output does not satisfy the protocol contract", Outcome: "unknown"}
	return agentCallResult{RequestID: id, Status: 502, Code: e.Code, Message: e.Message, Failure: e}
}

var capabilityMetadataName = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_.-]{0,127}$`)

func validateMetadata(metadata protocol.Metadata) error {
	values := metadata.Capabilities
	if len(values) == 0 || len(metadata.ResponseKinds) != len(values) || metadata.Data == nil || metadata.Data["openapi"] != "3.1.0" {
		return fmt.Errorf("missing capability metadata")
	}
	paths, ok := metadata.Data["paths"].(map[string]any)
	if !ok {
		return fmt.Errorf("invalid OpenAPI paths")
	}
	info, ok := metadata.Data["info"].(map[string]any)
	if !ok {
		return fmt.Errorf("invalid OpenAPI info")
	}
	title, titleOK := info["title"].(string)
	version, versionOK := info["version"].(string)
	if !titleOK || !versionOK || title == "" || version == "" {
		return fmt.Errorf("invalid OpenAPI info")
	}
	for name, v := range values {
		if !capabilityMetadataName.MatchString(name) || strings.HasPrefix(name, "onprest_") {
			return fmt.Errorf("invalid capability metadata")
		}
		_, hashErr := hex.DecodeString(v.DefinitionHash)
		d, e := time.ParseDuration(v.ExecutionTimeout)
		if e != nil || d <= 0 || d > 300*time.Second || len(v.DefinitionHash) != 64 || hashErr != nil || v.DefinitionHash != strings.ToLower(v.DefinitionHash) || (v.EffectClass != "none" && v.EffectClass != "possible") || !validExecutorKind(v.ExecutorKind) || v.InputSchema == nil || (len(v.InputLimits) != 3 && len(v.InputLimits) != 6) || v.InputLimits["max_input_bytes"] <= 0 || v.InputLimits["max_json_depth"] <= 0 || v.InputLimits["max_json_items"] <= 0 {
			return fmt.Errorf("invalid capability metadata")
		}
		if len(v.InputLimits) == 6 {
			timeout, e := time.ParseDuration(v.InputTimeout)
			if e != nil || timeout <= 0 || timeout > 300*time.Second || v.InputLimits["max_input_file_bytes"] <= 0 || v.InputLimits["max_total_input_file_bytes"] < v.InputLimits["max_input_file_bytes"] || v.InputLimits["max_input_files"] <= 0 {
				return fmt.Errorf("invalid input transfer metadata")
			}
		} else if v.InputTimeout != "" {
			return fmt.Errorf("unexpected input transfer timeout")
		}
		if err := validateOutputMetadata(v.Output); err != nil {
			return err
		}
		kind := metadata.ResponseKinds[name]
		if v.InputSchema["type"] != "object" || v.ExecutorKind == "db" && (v.EffectClass == "none" && kind != "select" || v.EffectClass == "possible" && kind != "mutation") || v.ExecutorKind != "db" && kind != "generic" {
			return fmt.Errorf("invalid capability response kind")
		}
	}
	for path, methods := range paths {
		name := strings.TrimPrefix(path, "/api/v1/capabilities/")
		if path == name {
			return fmt.Errorf("invalid capability path")
		}
		if _, ok := values[name]; !ok {
			return fmt.Errorf("unknown public capability")
		}
		operations, ok := methods.(map[string]any)
		if !ok || len(operations) != 1 {
			return fmt.Errorf("invalid capability operation")
		}
		operation, ok := operations["post"].(map[string]any)
		if !ok || operation["x-onprest-capability"] != name {
			return fmt.Errorf("invalid public capability binding")
		}
	}
	return nil
}
func validateAgentResult(id string, resp protocol.Response, meta protocol.CapabilityMetadata, known bool) ([]byte, *protocol.Metrics, error) {
	if resp.Type != "result" || resp.ID != id || resp.Error != nil || resp.Metrics == nil {
		return nil, nil, fmt.Errorf("invalid result frame")
	}
	var result struct {
		Data  json.RawMessage                 `json:"data"`
		Files []contract.PublicFileDescriptor `json:"files"`
	}
	dec := json.NewDecoder(bytes.NewReader(resp.Result))
	dec.DisallowUnknownFields()
	if e := dec.Decode(&result); e != nil || ensureJSONEOF(dec) != nil || len(result.Data) == 0 || result.Files == nil {
		return nil, nil, fmt.Errorf("invalid result")
	}
	value, e := contract.DecodeJSON(result.Data, contract.Limits{MaxBytes: 16 << 20, MaxDepth: 32, MaxItems: 100000})
	if e != nil {
		return nil, nil, e
	}
	m := resp.Metrics
	if !validExecutorKind(m.ExecutorKind) || known && m.ExecutorKind != meta.ExecutorKind || m.FileCount != int64(len(result.Files)) || m.InputFileCount != 0 || m.InputFileBytes != 0 || m.ResponseBytes < 0 || (m.ExecutorKind == "db" && m.ReturnedRows == nil && m.AffectedRows == nil) || m.ReturnedRows != nil && m.AffectedRows != nil || m.ExecutorKind != "db" && (m.ReturnedRows != nil || m.AffectedRows != nil) {
		return nil, nil, fmt.Errorf("invalid metrics")
	}
	if m.ReturnedRows != nil && *m.ReturnedRows < 0 || m.AffectedRows != nil && *m.AffectedRows < 0 {
		return nil, nil, fmt.Errorf("invalid count")
	}
	if known && meta.ExecutorKind == "db" && (meta.EffectClass == "none" && m.ReturnedRows == nil || meta.EffectClass == "possible" && m.AffectedRows == nil) {
		return nil, nil, fmt.Errorf("metric effect class mismatch")
	}

	if m.ExecutorKind == "db" && value != nil {
		object, ok := value.(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("invalid DB data shape")
		}
		if m.ReturnedRows != nil {
			rows, ok := object["rows"].([]any)
			if !ok || int64(len(rows)) != *m.ReturnedRows || object["count"] != *m.ReturnedRows {
				return nil, nil, fmt.Errorf("invalid row metrics")
			}
		} else if object["count"] != *m.AffectedRows {
			return nil, nil, fmt.Errorf("invalid mutation metrics")
		}
	}

	if e := validateFileDescriptors(result.Files, meta.Output); e != nil {
		return nil, nil, e
	}
	canonical, e := contract.CanonicalJSON(value)
	if e != nil || int64(len(canonical)) != m.ResponseBytes {
		return nil, nil, fmt.Errorf("invalid response size")
	}
	envelope := contract.NewSuccessEnvelope(id, value, result.Files)
	b, e := json.Marshal(envelope)
	return b, m, e
}

func agentTimeoutFor(ac *agentConn, id string) agentCallResult {
	r := agentTimeoutResult()
	ac.mu.Lock()
	state := ac.sendStates[id]
	if state != nil && (state.phase == "queued" || state.phase == "canceled") {
		r.Failure.Outcome = "not_started"
	}
	ac.mu.Unlock()
	return r
}

func agentOfflineFor(ac *agentConn, id string) agentCallResult {
	r := agentOfflineResult()
	r.Failure.Outcome = "unknown"
	ac.mu.Lock()
	if state := ac.sendStates[id]; state != nil && (state.phase == "queued" || state.phase == "canceled") {
		r.Failure.Outcome = "not_started"
	}
	ac.mu.Unlock()
	return r
}

func validExecutorKind(kind string) bool {
	switch kind {
	case "db", "csv", "http", "file", "wasm":
		return true
	}
	return false
}
