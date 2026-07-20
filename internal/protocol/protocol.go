package protocol

import (
	"encoding/json"
)

const AgentAuthVersion = "onprest-agent-v1"

type Request struct {
	ID         string         `json:"id"`
	Capability string         `json:"capability"`
	Params     map[string]any `json:"params"`
}

type Response struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

func AgentAuthMessage(path, timestamp, nonce, handshakeKey string) []byte {
	return []byte(AgentAuthVersion + "\n" + path + "\n" + timestamp + "\n" + nonce + "\n" + handshakeKey)
}

func ResultResponse(id string, result any) Response {
	if raw, ok := result.(json.RawMessage); ok {
		return Response{ID: id, Result: raw}
	}
	b, _ := json.Marshal(result)
	return Response{ID: id, Result: b}
}

func MustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
