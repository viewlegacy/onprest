package gateway

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"time"
)

const maxMCPFileResponses = 4
const maxAgentResultBytes = 16 << 20 // Existing WebSocket frame and result-data bound.

// reserveMCPResponse accounts for the complete response lifetime, including
// collection, JSON/Base64 expansion and a blocked external HTTP write. Sixteen
// times the policy byte bound covers raw buffer growth, strings and encoder
// buffers even for six-byte JSON escaping; the fixed frame allowance covers
// data/descriptor maps. This is a reservation, not a new file-size restriction.
func (s *Server) reserveMCPResponse(bytes int64, id any, uriBytes int) (func(), bool) {
	s.mcpResponseMu.Lock()
	defer s.mcpResponseMu.Unlock()
	if s.mcpResponseCount == maxMCPFileResponses {
		return nil, false
	}
	encodedID, _ := json.Marshal(id)
	reserved := new(big.Int).SetInt64(bytes)
	reserved.Add(reserved, big.NewInt(maxAgentResultBytes))
	reserved.Mul(reserved, big.NewInt(16))
	reserved.Add(reserved, big.NewInt(int64(2*len(encodedID)+6*uriBytes+65536)))
	s.mcpResponseCount++
	s.mcpResponseBytes.Add(&s.mcpResponseBytes, reserved)
	return func() {
		s.mcpResponseMu.Lock()
		defer s.mcpResponseMu.Unlock()
		s.mcpResponseCount--
		s.mcpResponseBytes.Sub(&s.mcpResponseBytes, reserved)
	}, true
}

// The same fixed deadline covers Agent collection and the external response.
// Cancel interrupts a blocked net/http write, rather than only the Agent reader.
func mcpDeliveryContext(w http.ResponseWriter, r *http.Request, d time.Duration) (context.Context, func()) {
	ctx, cancel := context.WithTimeout(r.Context(), d)
	controller := http.NewResponseController(w)
	deadline, _ := ctx.Deadline()
	_ = controller.SetWriteDeadline(deadline)
	stopped := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = controller.SetWriteDeadline(time.Now()); close(stopped) })
	return ctx, func() {
		cancel()
		if !stop() {
			<-stopped
		}
		_ = controller.SetWriteDeadline(time.Time{})
	}
}

func writeMCPDelivery(ctx context.Context, w http.ResponseWriter, id, result any) {
	writeMCPDeliveryEnvelope(ctx, w, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}
func writeMCPErrorDelivery(ctx context.Context, w http.ResponseWriter, id any, rpcCode int, appCode, message string) {
	writeMCPDeliveryEnvelope(ctx, w, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": rpcCode, "message": message, "data": map[string]any{"code": appCode}}})
}
func writeMCPDeliveryEnvelope(ctx context.Context, w http.ResponseWriter, envelope any) {
	if ctx.Err() != nil {
		panic(http.ErrAbortHandler)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if e := json.NewEncoder(w).Encode(envelope); e != nil {
		panic(http.ErrAbortHandler)
	}
	// Flush while the lease/deadline is still held: net/http can buffer the final
	// bytes until the handler returns, after its deadline has otherwise cleared.
	if e := http.NewResponseController(w).Flush(); e != nil && e != http.ErrNotSupported {
		panic(http.ErrAbortHandler)
	}
	if ctx.Err() != nil {
		panic(http.ErrAbortHandler)
	}
}
