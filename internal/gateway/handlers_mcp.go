package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/viewlegacy/onprest/internal/contract"
	"github.com/viewlegacy/onprest/internal/protocol"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/viewlegacy/onprest/internal/buildinfo"
)

const (
	mcpProtocolVersion20250326 = "2025-03-26"
	mcpProtocolVersion20250618 = "2025-06-18"
	mcpProtocolVersion20251125 = "2025-11-25"
	mcpProtocolHeader          = "MCP-Protocol-Version"
)

var mcpSupportedProtocolVersions = map[string]struct{}{
	mcpProtocolVersion20250326: {},
	mcpProtocolVersion20250618: {},
	mcpProtocolVersion20251125: {},
}

// These are the client-to-server notifications in the official MCP schema,
// keyed by the protocol version that defines them. Notifications sent by the
// server to a client are deliberately not accepted as id-less gateway
// requests. Task status notifications were added only in 2025-11-25.
var mcpClientNotificationMethodsByVersion = map[string]map[string]struct{}{
	mcpProtocolVersion20250326: {
		"notifications/cancelled":          {},
		"notifications/initialized":        {},
		"notifications/progress":           {},
		"notifications/roots/list_changed": {},
	},
	mcpProtocolVersion20250618: {
		"notifications/cancelled":          {},
		"notifications/initialized":        {},
		"notifications/progress":           {},
		"notifications/roots/list_changed": {},
	},
	mcpProtocolVersion20251125: {
		"notifications/cancelled":          {},
		"notifications/initialized":        {},
		"notifications/progress":           {},
		"notifications/roots/list_changed": {},
		"notifications/tasks/status":       {},
	},
}

type mcpRequest struct {
	JSONRPC     string
	ID          any
	HasID       bool
	Method      string
	HasMethod   bool
	MethodValid bool
	Params      json.RawMessage
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, requestAPIError(r, errGatewayMethodNotAllowed, "use POST"))
		return
	}

	key, authStatus := s.authenticate(w, r)
	if authStatus != 0 {
		return
	}
	if !mcpJSONContentType(r.Header.Get("Content-Type")) {
		s.mcpHTTPRejected(http.StatusBadRequest, errGatewayInvalidRequest, "Content-Type must be application/json")
		writeJSON(w, http.StatusBadRequest, requestAPIError(r, errGatewayInvalidRequest, "Content-Type must be application/json"))
		return
	}
	if !mcpAcceptsJSON(r.Header.Values("Accept")) {
		s.mcpHTTPRejected(http.StatusNotAcceptable, errGatewayInvalidRequest, "Accept must allow application/json")
		writeJSON(w, http.StatusNotAcceptable, requestAPIError(r, errGatewayInvalidRequest, "Accept must allow application/json"))
		return
	}

	req, err := decodeMCPRequest(w, r, s.cfg.MaxRequestBodyBytes)
	if err != nil {
		writeMCPError(w, nil, -32700, errJSONRPCParseError, "invalid json rpc")
		return
	}
	if !req.valid() {
		writeMCPError(w, req.invalidID(), -32600, errJSONRPCInvalidRequest, "invalid json rpc request")
		return
	}

	protocolVersion := mcpProtocolVersion20250326
	if req.Method != "initialize" {
		var ok bool
		protocolVersion, ok = mcpRequestProtocolVersion(r)
		if !ok {
			s.mcpHTTPRejected(http.StatusBadRequest, errGatewayInvalidRequest, "unsupported MCP protocol version")
			writeJSON(w, http.StatusBadRequest, requestAPIError(r, errGatewayInvalidRequest, "unsupported MCP protocol version"))
			return
		}
	}

	// A recognized client-to-server notification is notification-only. The
	// JSON-RPC request shape is evaluated before its method parameters so an
	// id-bearing notification is always INVALID_REQUEST, even when its params
	// would also be invalid. This keeps the protocol error deterministic and
	// guarantees that no notification can reach the agent.
	if mcpKnownClientNotificationMethod(req.Method) {
		if req.HasID {
			writeMCPError(w, req.ID, -32600, errJSONRPCInvalidRequest, "notification must not include id")
			return
		}
		if !mcpClientNotificationAllowed(protocolVersion, req.Method) {
			s.mcpHTTPRejected(http.StatusBadRequest, errGatewayInvalidRequest, "invalid json rpc notification")
			writeJSON(w, http.StatusBadRequest, requestAPIError(r, errGatewayInvalidRequest, "invalid json rpc notification"))
			return
		}
		if !validMCPNotificationParams(protocolVersion, req.Method, req.Params) {
			s.mcpHTTPRejected(http.StatusBadRequest, errJSONRPCInvalidParams, "invalid notification params")
			writeMCPErrorStatus(w, http.StatusBadRequest, nil, -32602, errJSONRPCInvalidParams, "invalid notification params")
			return
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}

	if !req.HasID {
		s.mcpHTTPRejected(http.StatusBadRequest, errGatewayInvalidRequest, "invalid json rpc notification")
		writeJSON(w, http.StatusBadRequest, requestAPIError(r, errGatewayInvalidRequest, "invalid json rpc notification"))
		return
	}

	switch req.Method {
	case "initialize":
		requested, ok := initializeProtocolVersion(req.Params)
		if !ok {
			writeMCPError(w, req.ID, -32602, errJSONRPCInvalidParams, "invalid initialize params")
			return
		}
		writeMCP(w, req.ID, map[string]any{
			"protocolVersion": mcpNegotiatedProtocolVersion(requested),
			"serverInfo":      map[string]any{"name": "onprest-gateway", "version": buildinfo.Current()},
			"capabilities":    map[string]any{"tools": map[string]any{}, "resources": map[string]any{"subscribe": false, "listChanged": false}},
		}, nil)
	case "ping":
		if !validMCPMethodParams(protocolVersion, req.Method, req.Params) {
			writeMCPError(w, req.ID, -32602, errJSONRPCInvalidParams, "invalid ping params")
			return
		}
		writeMCP(w, req.ID, map[string]any{}, nil)
	case "tools/list":
		if !validMCPMethodParams(protocolVersion, req.Method, req.Params) {
			writeMCPError(w, req.ID, -32602, errJSONRPCInvalidParams, "invalid tools/list params")
			return
		}
		s.agentMu.RLock()
		doc := cloneMap(s.openapi)
		s.agentMu.RUnlock()
		if doc == nil {
			writeMCPError(w, req.ID, -32000, errGatewayAgentOffline, "agent metadata is not cached yet")
			return
		}
		filterOpenAPI(doc, key.Capabilities)
		tools := toolsFromOpenAPI(doc, protocolVersion)
		if mcpUsesStructuredContent(protocolVersion) {
			s.agentMu.RLock()
			for _, tool := range tools {
				name, _ := tool["name"].(string)
				if o := s.capabilities[name].Output; o != nil {
					tool["outputSchema"] = cloneMap(o.MCPOutputSchema)
				}
			}
			s.agentMu.RUnlock()
		}
		writeMCP(w, req.ID, map[string]any{"tools": tools}, nil)
	case "resources/list":
		if !validMCPMethodParams(protocolVersion, "tools/list", req.Params) {
			writeMCPError(w, req.ID, -32602, errJSONRPCInvalidParams, "invalid resources/list params")
			return
		}
		writeMCP(w, req.ID, map[string]any{"resources": []any{}}, nil)
	case "resources/read":
		s.handleMCPResourceRead(w, r, req.ID, req.Params, key, protocolVersion)
	case "tools/call":
		s.handleMCPToolCall(w, r, req.ID, req.Params, key, protocolVersion, authRequestID(r), time.Now())
	default:
		writeMCPError(w, req.ID, -32601, errJSONRPCMethodNotFound, "unsupported MCP method")
	}
}

func mcpClientNotificationAllowed(protocolVersion, method string) bool {
	methods, ok := mcpClientNotificationMethodsByVersion[protocolVersion]
	if !ok {
		return false
	}
	_, ok = methods[method]
	return ok
}

func mcpKnownClientNotificationMethod(method string) bool {
	for _, methods := range mcpClientNotificationMethodsByVersion {
		if _, ok := methods[method]; ok {
			return true
		}
	}
	return false
}

func decodeMCPRequest(w http.ResponseWriter, r *http.Request, maxBodyBytes int64) (mcpRequest, error) {
	var raw json.RawMessage
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return mcpRequest{}, err
	}
	if err := ensureJSONEOF(dec); err != nil {
		return mcpRequest{}, err
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return mcpRequest{}, nil
	}
	req := mcpRequest{}
	if value, ok := fields["jsonrpc"]; ok {
		_ = json.Unmarshal(value, &req.JSONRPC)
	}
	if value, ok := fields["method"]; ok {
		req.HasMethod = true
		req.MethodValid = mcpJSONStringValue(value, &req.Method)
	}
	if value, ok := fields["params"]; ok {
		req.Params = value
	}
	if value, ok := fields["id"]; ok {
		req.HasID = true
		if err := decodeJSONNumber(value, &req.ID); err != nil {
			return req, nil
		}
	}
	return req, nil
}

func (r mcpRequest) valid() bool {
	if r.JSONRPC != "2.0" || !r.HasMethod || !r.MethodValid {
		return false
	}
	if !r.HasID {
		return true
	}
	switch r.ID.(type) {
	case string, json.Number:
		return true
	default:
		return false
	}
}

func (r mcpRequest) invalidID() any {
	if !r.HasID {
		return nil
	}
	switch r.ID.(type) {
	case string, json.Number:
		return r.ID
	default:
		return nil
	}
}

func mcpRequestProtocolVersion(r *http.Request) (string, bool) {
	// Header names are case-insensitive on the wire, but tests and custom
	// handlers can construct http.Header maps without canonicalizing keys.
	// Collect every matching key so duplicate values remain distinguishable.
	var values []string
	present := false
	for name, headerValues := range r.Header {
		if strings.EqualFold(name, mcpProtocolHeader) {
			present = true
			values = append(values, headerValues...)
		}
	}
	if !present {
		return mcpProtocolVersion20250326, true
	}
	// An explicitly empty/whitespace value, repeated header (even when the
	// values are identical), or a comma-joined representation is ambiguous and
	// must not silently select a protocol version.
	if len(values) != 1 || values[0] == "" || strings.TrimSpace(values[0]) != values[0] || strings.Contains(values[0], ",") {
		return "", false
	}
	version := values[0]
	_, ok := mcpSupportedProtocolVersions[version]
	return version, ok
}

func initializeProtocolVersion(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var params map[string]json.RawMessage
	if err := json.Unmarshal(raw, &params); err != nil || params == nil {
		return "", false
	}
	versionRaw, ok := params["protocolVersion"]
	if !ok {
		return "", false
	}
	var version string
	if !mcpJSONStringValue(versionRaw, &version) {
		return "", false
	}
	if !mcpOptionalMethodMetaIsObject(version, "initialize", params) {
		return "", false
	}
	capabilitiesRaw, ok := params["capabilities"]
	if !ok || !validMCPClientCapabilities(version, capabilitiesRaw) {
		return "", false
	}
	clientInfoRaw, ok := params["clientInfo"]
	if !ok {
		return "", false
	}
	clientInfo, ok := mcpJSONObjectFields(clientInfoRaw)
	if !ok {
		return "", false
	}
	if !validMCPClientInfo(version, clientInfo) {
		return "", false
	}
	return version, true
}

// validMCPClientInfo follows the versioned Implementation schema. Fields that
// are not defined by that schema remain open extension properties and are not
// interpreted by the gateway.
func validMCPClientInfo(protocolVersion string, fields map[string]json.RawMessage) bool {
	for _, field := range []string{"name", "version"} {
		value, present := fields[field]
		if !present {
			return false
		}
		var text string
		if !mcpJSONStringValue(value, &text) {
			return false
		}
	}
	switch protocolVersion {
	case mcpProtocolVersion20250618:
		return mcpOptionalString(fields, "title")
	case mcpProtocolVersion20251125:
		if !mcpOptionalString(fields, "title") || !mcpOptionalString(fields, "description") || !mcpOptionalString(fields, "websiteUrl") {
			return false
		}
		if icons, present := fields["icons"]; present && !validMCPIcons(icons) {
			return false
		}
	}
	return true
}

// validMCPMethodParams applies the method-specific request parameter shapes
// from the official schemas. Request params are open to extension fields, but
// known fields and the common _meta field retain their declared types.
func validMCPMethodParams(protocolVersion, method string, raw json.RawMessage) bool {
	switch method {
	case "initialize":
		_, ok := initializeProtocolVersion(raw)
		return ok
	case "ping":
		return validMCPOptionalRequestParams(protocolVersion, method, raw, func(map[string]json.RawMessage) bool { return true })
	case "tools/list":
		return validMCPOptionalRequestParams(protocolVersion, method, raw, func(fields map[string]json.RawMessage) bool {
			if cursor, present := fields["cursor"]; present {
				var value string
				return mcpJSONStringValue(cursor, &value)
			}
			return true
		})
	case "tools/call":
		_, _, ok := decodeMCPToolCallParams(raw, protocolVersion)
		return ok
	default:
		return true
	}
}

func validMCPOptionalRequestParams(protocolVersion, method string, raw json.RawMessage, validate func(map[string]json.RawMessage) bool) bool {
	if len(bytes.TrimSpace(raw)) == 0 {
		return true
	}
	fields, ok := mcpJSONObjectFields(raw)
	if !ok || !mcpOptionalMethodMetaIsObject(protocolVersion, method, fields) {
		return false
	}
	return validate(fields)
}

func validMCPClientCapabilities(protocolVersion string, raw json.RawMessage) bool {
	fields, ok := mcpJSONObjectFields(raw)
	if !ok {
		return false
	}
	if experimental, present := fields["experimental"]; present && !validMCPObjectMap(experimental) {
		return false
	}
	if roots, present := fields["roots"]; present && !validMCPRootsCapability(roots) {
		return false
	}
	if sampling, present := fields["sampling"]; present {
		if !mcpJSONObject(sampling) {
			return false
		}
		if protocolVersion == mcpProtocolVersion20251125 {
			if !validMCPOptionalObjectField(sampling, "context") || !validMCPOptionalObjectField(sampling, "tools") {
				return false
			}
		}
	}
	if elicitation, present := fields["elicitation"]; present {
		if protocolVersion == mcpProtocolVersion20250618 || protocolVersion == mcpProtocolVersion20251125 {
			if !mcpJSONObject(elicitation) {
				return false
			}
			if protocolVersion == mcpProtocolVersion20251125 &&
				(!validMCPOptionalObjectField(elicitation, "form") || !validMCPOptionalObjectField(elicitation, "url")) {
				return false
			}
		}
	}
	if protocolVersion == mcpProtocolVersion20251125 {
		if tasks, present := fields["tasks"]; present && !validMCPTaskCapabilities(tasks) {
			return false
		}
	}
	return true
}

func validMCPObjectMap(raw json.RawMessage) bool {
	fields, ok := mcpJSONObjectFields(raw)
	if !ok {
		return false
	}
	for _, value := range fields {
		if !mcpJSONObject(value) {
			return false
		}
	}
	return true
}

func validMCPRootsCapability(raw json.RawMessage) bool {
	fields, ok := mcpJSONObjectFields(raw)
	if !ok {
		return false
	}
	value, present := fields["listChanged"]
	if !present {
		return true
	}
	return mcpJSONBoolValue(value)
}

func validMCPOptionalObjectField(raw json.RawMessage, name string) bool {
	fields, ok := mcpJSONObjectFields(raw)
	if !ok {
		return false
	}
	value, present := fields[name]
	return !present || mcpJSONObject(value)
}

func validMCPTaskCapabilities(raw json.RawMessage) bool {
	fields, ok := mcpJSONObjectFields(raw)
	if !ok {
		return false
	}
	for _, name := range []string{"list", "cancel"} {
		if value, present := fields[name]; present && !mcpJSONObject(value) {
			return false
		}
	}
	requests, present := fields["requests"]
	if !present {
		return true
	}
	requestFields, ok := mcpJSONObjectFields(requests)
	if !ok {
		return false
	}
	for _, name := range []string{"sampling", "elicitation"} {
		value, present := requestFields[name]
		if !present {
			continue
		}
		nested, ok := mcpJSONObjectFields(value)
		if !ok {
			return false
		}
		field := "createMessage"
		if name == "elicitation" {
			field = "create"
		}
		if value, present := nested[field]; present && !mcpJSONObject(value) {
			return false
		}
	}
	return true
}

func validMCPIcons(raw json.RawMessage) bool {
	var icons []json.RawMessage
	if err := json.Unmarshal(raw, &icons); err != nil || icons == nil {
		return false
	}
	for _, rawIcon := range icons {
		fields, ok := mcpJSONObjectFields(rawIcon)
		if !ok {
			return false
		}
		src, present := fields["src"]
		var text string
		if !present || !mcpJSONStringValue(src, &text) {
			return false
		}
		if value, present := fields["mimeType"]; present && !mcpJSONStringValue(value, &text) {
			return false
		}
		if value, present := fields["theme"]; present {
			if !mcpJSONStringValue(value, &text) || (text != "light" && text != "dark") {
				return false
			}
		}
		if sizes, present := fields["sizes"]; present {
			var values []json.RawMessage
			if err := json.Unmarshal(sizes, &values); err != nil || values == nil {
				return false
			}
			for _, value := range values {
				if !mcpJSONStringValue(value, &text) {
					return false
				}
			}
		}
	}
	return true
}

func mcpJSONObject(raw json.RawMessage) bool {
	_, ok := mcpJSONObjectFields(raw)
	return ok
}

func mcpJSONObjectFields(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, false
	}
	return fields, true
}

// validMCPNotificationParams mirrors the client-to-server notification
// entries in the official schemas for all supported protocol versions. The
// version argument is kept explicit so a future schema change cannot
// accidentally become a global, version-less acceptance rule.
func validMCPNotificationParams(protocolVersion, method string, raw json.RawMessage) bool {
	if _, ok := mcpSupportedProtocolVersions[protocolVersion]; !ok {
		return false
	}
	if method == "notifications/initialized" || method == "notifications/roots/list_changed" {
		if len(raw) == 0 {
			return true
		}
		fields, ok := mcpJSONObjectFields(raw)
		if !ok {
			return false
		}
		return mcpOptionalMetaIsObject(fields)
	}
	fields, ok := mcpJSONObjectFields(raw)
	if !ok {
		return false
	}
	if !mcpOptionalMetaIsObject(fields) {
		return false
	}
	switch method {
	case "notifications/cancelled":
		requestID, ok := fields["requestId"]
		if !ok {
			return false
		}
		return mcpJSONRPCIDValue(requestID) && mcpOptionalString(fields, "reason")
	case "notifications/progress":
		progressToken, ok := fields["progressToken"]
		if !ok || !mcpJSONRPCIDValue(progressToken) || !mcpJSONNumberValue(fields, "progress", true) {
			return false
		}
		return mcpOptionalString(fields, "message") && mcpJSONNumberValue(fields, "total", false)
	case "notifications/tasks/status":
		return protocolVersion == mcpProtocolVersion20251125 && validMCPTaskStatusNotificationParams(fields)
	default:
		return false
	}
}

func validMCPTaskStatusNotificationParams(fields map[string]json.RawMessage) bool {
	for _, name := range []string{"taskId", "createdAt", "lastUpdatedAt"} {
		if !mcpRequiredString(fields, name) {
			return false
		}
	}
	status, ok := fields["status"]
	if !ok {
		return false
	}
	var statusValue string
	if !mcpJSONStringValue(status, &statusValue) {
		return false
	}
	switch statusValue {
	case "working", "input_required", "completed", "failed", "cancelled":
	default:
		return false
	}
	ttl, ok := fields["ttl"]
	if !ok || (!bytes.Equal(bytes.TrimSpace(ttl), []byte("null")) && !mcpJSONIntegerRawValue(ttl)) {
		return false
	}
	if statusMessage, present := fields["statusMessage"]; present && !mcpJSONStringValue(statusMessage, &statusValue) {
		return false
	}
	if pollInterval, present := fields["pollInterval"]; present && !mcpJSONIntegerRawValue(pollInterval) {
		return false
	}
	return true
}

func mcpRequiredString(fields map[string]json.RawMessage, name string) bool {
	raw, ok := fields[name]
	if !ok {
		return false
	}
	var value string
	return mcpJSONStringValue(raw, &value)
}

func mcpOptionalMetaIsObject(fields map[string]json.RawMessage) bool {
	meta, ok := fields["_meta"]
	// NotificationParams only declares _meta as an open object. Unlike
	// RequestParams, notification metadata is not interpreted by the gateway,
	// so its members (including a member named progressToken) are opaque.
	return !ok || mcpJSONObject(meta)
}

func mcpOptionalRequestMetaIsObject(fields map[string]json.RawMessage) bool {
	meta, ok := fields["_meta"]
	if !ok {
		return true
	}
	metaFields, ok := mcpJSONObjectFields(meta)
	if !ok {
		return false
	}
	progressToken, present := metaFields["progressToken"]
	return !present || mcpJSONRPCIDValue(progressToken)
}

func mcpOptionalMethodMetaIsObject(protocolVersion, method string, fields map[string]json.RawMessage) bool {
	// RequestParams._meta is shared by all supported method schemas and all
	// supported protocol versions. Its optional progressToken is always the
	// JSON-RPC ID union (string or number), not an arbitrary JSON value.
	return mcpOptionalRequestMetaIsObject(fields)
}

func mcpOptionalString(fields map[string]json.RawMessage, name string) bool {
	raw, ok := fields[name]
	if !ok {
		return true
	}
	var value string
	return mcpJSONStringValue(raw, &value)
}

func mcpJSONStringValue(raw json.RawMessage, dst *string) bool {
	var value any
	if err := decodeJSONNumber(raw, &value); err != nil {
		return false
	}
	text, ok := value.(string)
	if !ok {
		return false
	}
	*dst = text
	return true
}

func mcpJSONBoolValue(raw json.RawMessage) bool {
	var value any
	if err := decodeJSONNumber(raw, &value); err != nil {
		return false
	}
	_, ok := value.(bool)
	return ok
}

func mcpJSONRPCIDValue(raw json.RawMessage) bool {
	var value any
	if err := decodeJSONNumber(raw, &value); err != nil {
		return false
	}
	switch value.(type) {
	case string, json.Number:
		return true
	default:
		return false
	}
}

func mcpJSONNumberValue(fields map[string]json.RawMessage, name string, required bool) bool {
	raw, ok := fields[name]
	if !ok {
		return !required
	}
	var value any
	if err := decodeJSONNumber(raw, &value); err != nil {
		return false
	}
	_, ok = value.(json.Number)
	return ok
}

func mcpJSONIntegerRawValue(raw json.RawMessage) bool {
	var value any
	if err := decodeJSONNumber(raw, &value); err != nil {
		return false
	}
	number, ok := value.(json.Number)
	if !ok {
		return false
	}
	rational, ok := new(big.Rat).SetString(number.String())
	return ok && rational.IsInt()
}

func mcpNegotiatedProtocolVersion(requested string) string {
	if _, ok := mcpSupportedProtocolVersions[requested]; ok {
		return requested
	}
	return mcpProtocolVersion20251125
}

func mcpJSONContentType(raw string) bool {
	mediaType := strings.TrimSpace(strings.SplitN(raw, ";", 2)[0])
	return strings.EqualFold(mediaType, "application/json")
}

func mcpAcceptsJSON(values []string) bool {
	if len(values) == 0 {
		return true
	}
	jsonQuality, applicationQuality, wildcardQuality := -1.0, -1.0, -1.0
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			pieces := strings.Split(part, ";")
			mediaType := strings.ToLower(strings.TrimSpace(pieces[0]))
			quality := 1.0
			valid := true
			for _, parameter := range pieces[1:] {
				nameValue := strings.SplitN(strings.TrimSpace(parameter), "=", 2)
				if len(nameValue) != 2 || !strings.EqualFold(strings.TrimSpace(nameValue[0]), "q") {
					continue
				}
				parsed, err := strconv.ParseFloat(strings.TrimSpace(nameValue[1]), 64)
				if err != nil || parsed != parsed || parsed < 0 || parsed > 1 {
					valid = false
					break
				}
				quality = parsed
			}
			if !valid {
				continue
			}
			switch mediaType {
			case "application/json":
				if quality > jsonQuality {
					jsonQuality = quality
				}
			case "application/*":
				if quality > applicationQuality {
					applicationQuality = quality
				}
			case "*/*":
				if quality > wildcardQuality {
					wildcardQuality = quality
				}
			}
		}
	}
	if jsonQuality >= 0 {
		return jsonQuality > 0
	}
	if applicationQuality >= 0 {
		return applicationQuality > 0
	}
	return wildcardQuality > 0
}

func (s *Server) handleMCPToolCall(w http.ResponseWriter, r *http.Request, id any, rawParams json.RawMessage, key APIKey, protocolVersion, requestID string, start time.Time) {
	name, arguments, ok := decodeMCPToolCallParams(rawParams, protocolVersion)
	if !ok {
		s.accessLogProtocol("mcp", requestID, key.Name, "", http.StatusOK, errJSONRPCInvalidParams, "invalid tools/call params", start)
		writeMCPError(w, id, -32602, errJSONRPCInvalidParams, "invalid tools/call params")
		return
	}
	if !allowed(key, name) {
		s.accessLogProtocol("mcp", requestID, key.Name, name, http.StatusForbidden, errGatewayCapabilityDenied, "capability not allowed", start)
		writeJSON(w, http.StatusForbidden, commonAPIError(requestID, errGatewayCapabilityDenied, "capability not allowed", "not_started"))
		return
	}
	s.agentMu.RLock()
	output := s.capabilities[name].Output
	s.agentMu.RUnlock()
	deliveryCtx := r.Context()
	if output != nil {
		bytes := int64(0)
		if output.MCP == "inline" {
			bytes = output.InlineBytes
		}
		release, ok := s.reserveMCPResponse(bytes, id, 0)
		if !ok {
			_, finish := mcpDeliveryContext(w, r, time.Second)
			defer finish()
			writeJSON(w, http.StatusServiceUnavailable, commonAPIError(requestID, "BUSY", "file response capacity is unavailable", "not_started"))
			return
		}
		defer release()
	}
	fingerprint := sha256.Sum256([]byte(key.KeyHash))
	ctx := context.WithValue(deliveryCtx, callIdentityKey{}, callIdentity{ID: requestID, Binding: hex.EncodeToString(fingerprint[:])})
	result := s.callAgent(ctx, name, arguments)
	if output != nil {
		duration, _ := time.ParseDuration(output.TransferTimeout)
		var finish func()
		deliveryCtx, finish = mcpDeliveryContext(w, r, duration)
		defer finish()
	}
	if !result.OK() {
		logged := result
		logged.Status = http.StatusOK
		s.accessExecutionLog("mcp", requestID, key.Name, name, logged, start)
		toolResult := map[string]any{"content": []any{map[string]any{"type": "text", "text": string(protocol.MustJSON(resultError(requestID, result)))}}, "isError": true}
		if mcpUsesStructuredContent(protocolVersion) {
			toolResult["structuredContent"] = resultError(requestID, result)
		}
		if output != nil {
			writeMCPDelivery(deliveryCtx, w, id, toolResult)
		} else {
			writeMCP(w, id, toolResult, nil)
		}
		return
	}
	toolResult, failure := s.renderMCPFiles(deliveryCtx, id, key, name, protocolVersion, result)
	if failure != nil {
		result = *failure
		toolResult = map[string]any{"content": []any{map[string]any{"type": "text", "text": string(protocol.MustJSON(resultError(requestID, result)))}}, "isError": true}
		if mcpUsesStructuredContent(protocolVersion) {
			toolResult["structuredContent"] = resultError(requestID, result)
		}
	}
	logged := deliveryFailure("EXECUTION_TIMEOUT", "file response could not be completed", "unknown")
	logged.RequestID = requestID
	defer func() {
		logged.Status = http.StatusOK
		s.accessExecutionLog("mcp", requestID, key.Name, name, logged, start)
	}()
	if output != nil {
		writeMCPDelivery(deliveryCtx, w, id, toolResult)
	} else {
		writeMCP(w, id, toolResult, nil)
	}
	logged = result
}

func decodeMCPToolCallParams(raw json.RawMessage, protocolVersion string) (string, map[string]any, bool) {
	fields, ok := mcpJSONObjectFields(raw)
	if !ok {
		return "", nil, false
	}
	if !mcpOptionalMethodMetaIsObject(protocolVersion, "tools/call", fields) {
		return "", nil, false
	}
	// The gateway does not advertise or implement the 2025-11-25 task
	// capability. Refuse the task field explicitly instead of silently
	// treating a task request as an ordinary tool call.
	if protocolVersion == mcpProtocolVersion20251125 {
		if _, present := fields["task"]; present {
			return "", nil, false
		}
	}
	nameRaw, ok := fields["name"]
	if !ok {
		return "", nil, false
	}
	var name string
	if !mcpJSONStringValue(nameRaw, &name) {
		return "", nil, false
	}
	arguments := map[string]any{}
	argumentsRaw, present := fields["arguments"]
	if !present {
		return name, arguments, true
	}
	if _, ok := mcpJSONObjectFields(argumentsRaw); !ok {
		return "", nil, false
	}
	value, err := contract.DecodeJSON(argumentsRaw, contract.Limits{MaxBytes: 16 << 20, MaxDepth: 32, MaxItems: 100000})
	if err != nil {
		return "", nil, false
	}
	arguments, ok = value.(map[string]any)
	if !ok || arguments == nil {
		return "", nil, false
	}
	return name, arguments, true
}

func mcpUsesStructuredContent(protocolVersion string) bool {
	return protocolVersion == mcpProtocolVersion20250618 || protocolVersion == mcpProtocolVersion20251125
}

func decodeJSONNumber(raw []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	return ensureJSONEOF(dec)
}

func ensureJSONEOF(dec *json.Decoder) error {
	var extra any
	err := dec.Decode(&extra)
	if err == io.EOF {
		return nil
	}
	if err == nil {
		return io.ErrUnexpectedEOF
	}
	return err
}

func embeddedFile(uri string, f contract.PublicFileDescriptor, body []byte) map[string]any {
	resource := map[string]any{"uri": uri, "mimeType": f.MIMEType}
	if strings.HasPrefix(f.MIMEType, "text/") && utf8.Valid(body) {
		resource["text"] = string(body)
	} else {
		resource["blob"] = base64.StdEncoding.EncodeToString(body)
	}
	return resource
}
func (s *Server) collectFile(ctx context.Context, v fileReference, limit int64) ([]byte, error) {
	if v.descriptor.SizeBytes > limit {
		return nil, errors.New("LIMIT_EXCEEDED")
	}
	var buffer bytes.Buffer
	e := s.transferFile(ctx, v, func(ctx context.Context, b []byte) (int, error) {
		if e := ctx.Err(); e != nil {
			return 0, e
		}
		if int64(buffer.Len())+int64(len(b)) > limit {
			return 0, errors.New("LIMIT_EXCEEDED")
		}
		return buffer.Write(b)
	})
	if e != nil {
		return nil, e
	}
	return buffer.Bytes(), nil
}
func (s *Server) renderMCPFiles(ctx context.Context, id any, key APIKey, name, version string, result agentCallResult) (map[string]any, *agentCallResult) {
	envelope, e := decodeSuccess(result)
	if e != nil {
		failure := invalidAgentResult(result.RequestID)
		return nil, &failure
	}
	s.agentMu.RLock()
	output := s.capabilities[name].Output
	s.agentMu.RUnlock()
	content := []any{}
	if output != nil && output.MCP == "inline" {
		duration, _ := time.ParseDuration(output.TransferTimeout)
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, duration)
		defer cancel()
	}
	var collected int64
	for i, f := range envelope.Result.Files {
		if output == nil {
			failure := invalidAgentResult(result.RequestID)
			return nil, &failure
		}
		v, e := s.fileLookup(key, f.ID, false)
		if e != nil {
			failure := fileTransferError(e)
			return nil, &failure
		}
		if output.MCP == "inline" {
			// Base64 cannot fit when raw bytes already exceed the whole-envelope cap.
			body, e := s.collectFile(ctx, v, output.InlineBytes-collected)
			if e != nil {
				failure := fileTransferError(e)
				if e.Error() == "LIMIT_EXCEEDED" {
					failure = deliveryFailure("LIMIT_EXCEEDED", "MCP inline result exceeds configured limit", "unknown")
				}
				return nil, &failure
			}
			collected += int64(len(body))
			envelope.Result.Files[i].URI = ""
			envelope.Result.Files[i].ExpiresAt = ""
			content = append(content, map[string]any{"type": "resource", "resource": embeddedFile("onprest-file://"+f.ID, f, body)})
		} else if mcpUsesStructuredContent(version) {
			content = append(content, map[string]any{"type": "resource_link", "uri": f.URI, "name": f.Name, "mimeType": f.MIMEType, "size": f.SizeBytes})
		}
	}
	payload, e := json.Marshal(envelope)
	if e != nil {
		failure := invalidAgentResult(result.RequestID)
		return nil, &failure
	}
	content = append([]any{map[string]any{"type": "text", "text": string(payload)}}, content...)
	tool := map[string]any{"content": content}
	if mcpUsesStructuredContent(version) {
		tool["structuredContent"] = envelope
	}
	if output != nil && output.MCP == "inline" {
		wire, e := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": tool})
		if e != nil || int64(len(wire)+1) > output.InlineBytes {
			failure := deliveryFailure("LIMIT_EXCEEDED", "MCP inline result exceeds configured limit", "unknown")
			return nil, &failure
		}
	}
	return tool, nil
}
func (s *Server) handleMCPResourceRead(w http.ResponseWriter, r *http.Request, id any, raw json.RawMessage, key APIKey, version string) {
	// Reserve before parsing/lookup as well: error responses can echo a large
	// JSON-RPC ID and must share the same fixed response bound.
	var maxFileBytes int64
	s.agentMu.RLock()
	for _, meta := range s.capabilities {
		if meta.Output != nil && meta.Output.MaxFileBytes > maxFileBytes {
			maxFileBytes = meta.Output.MaxFileBytes
		}
	}
	s.agentMu.RUnlock()
	release, available := s.reserveMCPResponse(maxFileBytes, id, len(raw))
	if !available {
		_, finish := mcpDeliveryContext(w, r, time.Second)
		defer finish()
		writeJSON(w, http.StatusServiceUnavailable, requestAPIError(r, "BUSY", "file response capacity is unavailable"))
		return
	}
	defer release()
	reject := func(rpcCode int, code, message string) {
		ctx, finish := mcpDeliveryContext(w, r, time.Second)
		defer finish()
		writeMCPErrorDelivery(ctx, w, id, rpcCode, code, message)
	}

	fields, ok := mcpJSONObjectFields(raw)
	var uri string
	if !ok || !mcpOptionalMethodMetaIsObject(version, "resources/read", fields) || !mcpJSONStringValue(fields["uri"], &uri) {
		reject(-32602, errJSONRPCInvalidParams, "invalid resources/read params")
		return
	}
	fileID := strings.TrimPrefix(uri, s.cfg.PublicBaseURL+"/api/v1/files/")
	if s.cfg.PublicBaseURL == "" || fileID == uri || !fileIDPattern.MatchString(fileID) || uri != s.cfg.PublicBaseURL+"/api/v1/files/"+fileID {
		reject(-32002, "NOT_FOUND", "file reference is unavailable")
		return
	}
	v, e := s.fileLookup(key, fileID, true)
	if e != nil {
		s.chargeRejectedFileLookup(r, fileID)
		reject(-32002, "NOT_FOUND", "file reference is unavailable")
		return
	}
	duration, _ := time.ParseDuration(v.output.TransferTimeout)
	ctx, finish := mcpDeliveryContext(w, r, duration)
	defer finish()
	body, e := s.collectFile(ctx, v, v.output.MaxFileBytes)
	if e != nil {
		failure := fileTransferError(e)
		writeMCPErrorDelivery(ctx, w, id, -32000, failure.Code, failure.Message)
		return
	}
	writeMCPDelivery(ctx, w, id, map[string]any{"contents": []any{embeddedFile(uri, v.descriptor, body)}})
}
