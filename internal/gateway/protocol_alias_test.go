package gateway

import "github.com/viewlegacy/onprest/internal/protocol"

type agentRequest = protocol.Request
type agentResponse = protocol.Response
type wireError = protocol.Error

func agentAuthMessage(path, timestamp, nonce string) []byte {
	return protocol.AgentAuthMessage(path, timestamp, nonce)
}
