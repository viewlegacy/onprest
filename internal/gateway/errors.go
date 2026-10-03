package gateway

import (
	"net/http"
	"strings"
)

const (
	errGatewayAuthFailed              = "AUTH_FAILED"
	errGatewayCapabilityDenied        = "CAPABILITY_DENIED"
	errGatewayIPDenied                = "PERMISSION_DENIED"
	errGatewayRateLimited             = "RATE_LIMITED"
	errGatewayInternal                = "INTERNAL_ERROR"
	errGatewayAgentOffline            = "AGENT_OFFLINE"
	errGatewayTimeout                 = "EXECUTION_TIMEOUT"
	errGatewayInvalidRequest          = "INVALID_REQUEST"
	errGatewayCapabilityNotFound      = "NOT_FOUND"
	errGatewayMethodNotAllowed        = "INVALID_REQUEST"
	errGatewayAgentAlreadyConn        = "CONFLICT"
	errAgentValidationFailed          = "VALIDATION_FAILED"
	errAgentQueryFailed               = "EXECUTION_FAILED"
	errAgentQueryTimeout              = "EXECUTION_TIMEOUT"
	errAgentDBUnreachable             = "DEPENDENCY_UNAVAILABLE"
	errAgentInternal                  = "EXECUTION_FAILED"
	errAgentBusy                      = "BUSY"
	errAgentRateLimited               = "RATE_LIMITED"
	errAgentConstraintViolation       = "CONFLICT"
	errAgentAffectedRowsExceeded      = "LIMIT_EXCEEDED"
	errAgentTransactionOutcomeUnknown = "OUTCOME_UNKNOWN"
	errJSONRPCParseError              = "PARSE_ERROR"
	errJSONRPCInvalidRequest          = "INVALID_REQUEST"
	errJSONRPCMethodNotFound          = "METHOD_NOT_FOUND"
	errJSONRPCInvalidParams           = "INVALID_PARAMS"
)

func agentErrorStatus(code string) (int, string) {
	status := http.StatusBadGateway
	switch code {
	case "INVALID_REQUEST", "VALIDATION_FAILED":
		status = 400
	case "AUTH_FAILED":
		status = 401
	case "CAPABILITY_DENIED", "PERMISSION_DENIED":
		status = 403
	case "NOT_FOUND":
		status = 404
	case "SOURCE_CHANGED", "CONFLICT":
		status = 409
	case "LIMIT_EXCEEDED":
		status = 422
	case "RATE_LIMITED":
		status = 429
	case "BUSY", "AGENT_OFFLINE", "DEPENDENCY_UNAVAILABLE":
		status = 503
	case "EXECUTION_TIMEOUT":
		status = 504
	case "INTERNAL_ERROR":
		status = 500
	default:
		if strings.HasPrefix(code, "APP_") {
			status = 422
		}
	}
	return status, code
}
