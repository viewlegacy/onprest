package gateway

import "net/http"

const (
	errGatewayAuthFailed         = "GATEWAY_AUTH_FAILED"
	errGatewayCapabilityDenied   = "GATEWAY_CAPABILITY_DENIED"
	errGatewayIPDenied           = "GATEWAY_IP_DENIED"
	errGatewayRateLimited        = "GATEWAY_RATE_LIMITED"
	errGatewayInternal           = "GATEWAY_INTERNAL_ERROR"
	errGatewayAgentOffline       = "GATEWAY_AGENT_OFFLINE"
	errGatewayTimeout            = "GATEWAY_TIMEOUT"
	errGatewayInvalidRequest     = "GATEWAY_INVALID_REQUEST"
	errGatewayCapabilityNotFound = "GATEWAY_CAPABILITY_NOT_FOUND"
	errGatewayMethodNotAllowed   = "GATEWAY_METHOD_NOT_ALLOWED"
	errGatewayAgentAlreadyConn   = "GATEWAY_AGENT_ALREADY_CONNECTED"
	errAgentValidationFailed     = "AGENT_VALIDATION_FAILED"
	errAgentQueryFailed          = "AGENT_QUERY_FAILED"
	errAgentQueryTimeout         = "AGENT_QUERY_TIMEOUT"
	errAgentDBUnreachable        = "AGENT_DB_UNREACHABLE"
	errAgentInternal             = "AGENT_INTERNAL_ERROR"
	errJSONRPCParseError         = "PARSE_ERROR"
	errJSONRPCInvalidRequest     = "INVALID_REQUEST"
	errJSONRPCMethodNotFound     = "METHOD_NOT_FOUND"
	errJSONRPCInvalidParams      = "INVALID_PARAMS"
)

func apiError(code, message string) map[string]any {
	return map[string]any{"error": map[string]any{"code": code, "message": message}}
}

func agentErrorStatus(code string) (int, string) {
	switch code {
	case errGatewayAgentOffline:
		return http.StatusServiceUnavailable, errGatewayAgentOffline
	case errGatewayCapabilityNotFound:
		return http.StatusNotFound, errGatewayCapabilityNotFound
	case errAgentValidationFailed:
		return http.StatusBadRequest, errAgentValidationFailed
	case errAgentQueryFailed:
		return http.StatusBadGateway, errAgentQueryFailed
	case errAgentQueryTimeout:
		return http.StatusGatewayTimeout, errAgentQueryTimeout
	case errAgentDBUnreachable:
		return http.StatusBadGateway, errAgentDBUnreachable
	case errAgentInternal:
		return http.StatusBadGateway, errAgentInternal
	default:
		return http.StatusBadGateway, code
	}
}
