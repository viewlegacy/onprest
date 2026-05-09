package gateway

import (
	"encoding/json"
	"net/http"

	"github.com/viewlegacy/onprest/internal/protocol"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeMCP(w http.ResponseWriter, id any, result any, e *protocol.Error) {
	resp := map[string]any{"jsonrpc": "2.0", "id": id}
	if e != nil {
		resp["error"] = e
	} else {
		resp["result"] = result
	}
	writeJSON(w, http.StatusOK, resp)
}

func writeMCPError(w http.ResponseWriter, id any, rpcCode int, appCode, message string) {
	writeJSON(w, http.StatusOK, map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error": map[string]any{
			"code":    rpcCode,
			"message": message,
			"data":    map[string]any{"code": appCode},
		},
	})
}
