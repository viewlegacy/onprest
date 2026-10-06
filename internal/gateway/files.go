package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/viewlegacy/onprest/internal/contract"
	"github.com/viewlegacy/onprest/internal/protocol"
)

const maxFileReferences = 65536

var fileIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)

type fileReference struct {
	descriptor                           contract.PublicFileDescriptor
	requestID, binding, capability, hash string
	connection                           *agentConn
	output                               protocol.OutputMetadata
	expiry                               time.Time
}

func credentialBinding(key APIKey) string {
	v := sha256.Sum256([]byte(key.KeyHash))
	return hex.EncodeToString(v[:])
}
func normalizeFileOrigin(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") || (u.Scheme != "http" && u.Scheme != "https") {
		return "", errors.New("ONPREST_PUBLIC_BASE_URL must be an HTTP(S) origin without userinfo, path, query or fragment")
	}
	if u.Scheme == "http" {
		ip := net.ParseIP(u.Hostname())
		if u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return "", errors.New("ONPREST_PUBLIC_BASE_URL requires HTTPS except on loopback")
		}
	}
	if _, e = normalizeOrigin(u.Scheme + "://" + u.Host); e != nil {
		return "", errors.New("ONPREST_PUBLIC_BASE_URL is invalid")
	}
	return u.Scheme + "://" + u.Host, nil
}
func validateOutputMetadata(o *protocol.OutputMetadata) error {
	if o == nil {
		return nil
	}
	ttl, te := time.ParseDuration(o.TTL)
	d, de := time.ParseDuration(o.TransferTimeout)
	if (o.REST != "reference" && o.REST != "direct") || (o.MCP != "reference" && o.MCP != "inline") || o.MaxFileBytes <= 0 || o.MaxTotalBytes < o.MaxFileBytes || o.MaxFiles <= 0 || ttl <= 0 || te != nil || d <= 0 || d > 300*time.Second || de != nil || o.MCPOutputSchema == nil || o.MCPOutputSchema["type"] != "object" || (o.MCP == "inline" && o.InlineBytes <= 0) || (o.MCP != "inline" && o.InlineBytes != 0) {
		return errors.New("invalid file output metadata")
	}
	return nil
}
func (s *Server) validateFileOrigin(m protocol.Metadata) error {
	for _, v := range m.Capabilities {
		if o := v.Output; o != nil && (o.REST == "reference" || o.MCP == "reference") && s.cfg.PublicBaseURL == "" {
			return errors.New("ONPREST_PUBLIC_BASE_URL is required for file reference delivery")
		}
	}
	return nil
}
func validateFileDescriptors(files []contract.PublicFileDescriptor, o *protocol.OutputMetadata) error {
	if o == nil {
		if len(files) != 0 {
			return errors.New("unexpected file descriptors")
		}
		return nil
	}
	if int64(len(files)) > o.MaxFiles || o.REST == "direct" && len(files) != 1 {
		return errors.New("invalid file count")
	}
	seen := map[string]bool{}
	var total int64
	for _, f := range files {
		if !fileIDPattern.MatchString(f.ID) || seen[f.ID] || !utf8.ValidString(f.Name) || len(f.Name) < 1 || len(f.Name) > 255 || f.Name == "." || f.Name == ".." || strings.ContainsAny(f.Name, "/\\") || f.SizeBytes < 0 || f.SizeBytes > o.MaxFileBytes || f.SizeBytes > o.MaxTotalBytes-total || len(f.SHA256) != 64 || f.SHA256 != strings.ToLower(f.SHA256) || f.URI != "" {
			return errors.New("invalid file metadata")
		}
		for _, r := range f.Name {
			if unicode.IsControl(r) {
				return errors.New("invalid file name")
			}
		}
		if _, e := hex.DecodeString(f.SHA256); e != nil {
			return errors.New("invalid file digest")
		}
		mt, p, e := mime.ParseMediaType(f.MIMEType)
		if e != nil || len(p) != 0 || mt != f.MIMEType || mt != strings.ToLower(mt) {
			return errors.New("invalid file MIME")
		}
		expiry, e := time.Parse(time.RFC3339Nano, f.ExpiresAt)
		ttl, _ := time.ParseDuration(o.TTL)
		if e != nil || !time.Now().Before(expiry) || expiry.After(time.Now().Add(ttl+5*time.Second)) {
			return errors.New("invalid file expiry")
		}
		total += f.SizeBytes
		seen[f.ID] = true
	}
	return nil
}
func deliveryFailure(code, message, outcome string) agentCallResult {
	status, _ := agentErrorStatus(code)
	return agentCallResult{Status: status, Code: code, Message: message, Failure: &contract.ErrorValue{Code: code, Message: message, Outcome: outcome}}
}
func decodeSuccess(result agentCallResult) (contract.SuccessEnvelope, error) {
	var envelope contract.SuccessEnvelope
	dec := json.NewDecoder(strings.NewReader(string(result.Payload)))
	dec.UseNumber()
	e := dec.Decode(&envelope)
	return envelope, e
}
func (s *Server) registerFiles(ac *agentConn, identity callIdentity, capability string, meta protocol.CapabilityMetadata, result agentCallResult) agentCallResult {
	envelope, e := decodeSuccess(result)
	if e != nil {
		return invalidAgentResult(result.RequestID)
	}
	s.agentMu.Lock()
	defer s.agentMu.Unlock()
	if s.agent != ac {
		return deliveryFailure("AGENT_OFFLINE", "agent is not connected", "unknown")
	}
	if s.references == nil {
		s.references = map[string]fileReference{}
	}
	now := time.Now()
	for id, v := range s.references {
		if !now.Before(v.expiry) {
			delete(s.references, id)
		}
	}
	if len(s.references)+len(envelope.Result.Files) > maxFileReferences {
		return deliveryFailure("BUSY", "file reference capacity is unavailable", "unknown")
	}
	for _, f := range envelope.Result.Files {
		if _, exists := s.references[f.ID]; exists {
			return invalidAgentResult(result.RequestID)
		}
	}
	for i, f := range envelope.Result.Files {
		expiry, _ := time.Parse(time.RFC3339Nano, f.ExpiresAt)
		s.references[f.ID] = fileReference{descriptor: f, requestID: result.RequestID, binding: identity.Binding, capability: capability, hash: meta.DefinitionHash, connection: ac, output: *meta.Output, expiry: expiry}
		if meta.Output.REST == "reference" || meta.Output.MCP == "reference" {
			envelope.Result.Files[i].URI = s.cfg.PublicBaseURL + "/api/v1/files/" + f.ID
		}
	}
	result.Payload, _ = json.Marshal(envelope)
	return result
}
func (s *Server) fileLookup(key APIKey, id string, reference bool) (fileReference, error) {
	s.agentMu.Lock()
	defer s.agentMu.Unlock()
	v, ok := s.references[id]
	meta, known := s.capabilities[v.capability]
	if !ok || !time.Now().Before(v.expiry) || s.agent != v.connection || !known || meta.DefinitionHash != v.hash {
		if ok {
			delete(s.references, id)
		}
		return v, errors.New("NOT_FOUND")
	}
	if !allowed(key, v.capability) || credentialBinding(key) != v.binding {
		return v, errors.New("CAPABILITY_DENIED")
	}
	if reference && v.output.REST != "reference" && v.output.MCP != "reference" {
		return v, errors.New("NOT_FOUND")
	}
	return v, nil
}
func fileTransferError(e error) agentCallResult {
	code := e.Error()
	switch {
	case errors.Is(e, context.Canceled), errors.Is(e, context.DeadlineExceeded):
		code = "EXECUTION_TIMEOUT"
	case strings.Contains(code, "capacity"):
		code = "BUSY"
	case code == "agent offline" || code == "agent disconnected":
		code = "AGENT_OFFLINE"
	}
	switch code {
	case "CAPABILITY_DENIED", "LIMIT_EXCEEDED", "NOT_FOUND", "BUSY", "RATE_LIMITED", "EXECUTION_TIMEOUT", "AGENT_OFFLINE", "OUTPUT_INVALID":
	default:
		code = "OUTPUT_INVALID"
	}
	return deliveryFailure(code, "file transfer could not be completed", "unknown")
}
func (s *Server) transferFile(ctx context.Context, v fileReference, sink func(context.Context, []byte) (int, error)) error {
	d, _ := time.ParseDuration(v.output.TransferTimeout)
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	s.agentMu.RLock()
	current := s.agent == v.connection
	s.agentMu.RUnlock()
	if !current {
		return errors.New("NOT_FOUND")
	}
	return s.transferOutputOn(ctx, v.connection, v.requestID, v.descriptor.ID, v.descriptor.SizeBytes, v.descriptor.SHA256, sink)
}

// Rejected canonical lookups still consume the Agent's fixed management
// admission. A fresh synthetic request ID can never resolve a retained owner.
func (s *Server) chargeRejectedFileLookup(r *http.Request, fileID string) {
	s.agentMu.RLock()
	ac := s.agent
	s.agentMu.RUnlock()
	if ac == nil {
		return
	}
	duration := s.cfg.AgentTimeout
	if duration <= 0 || duration > 5*time.Second {
		duration = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), duration)
	defer cancel()
	empty := sha256.Sum256(nil)
	_ = s.transferOutputOn(ctx, ac, authRequestID(r), fileID, 0, hex.EncodeToString(empty[:]), func(context.Context, []byte) (int, error) { return 0, errors.New("OUTPUT_INVALID") })
}
func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	key, status := s.authenticate(w, r)
	if status != 0 {
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, 405, requestAPIError(r, "INVALID_REQUEST", "use GET"))
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/v1/files/")
	if !fileIDPattern.MatchString(id) || r.URL.RawPath != "" || r.URL.RawQuery != "" || r.URL.ForceQuery {
		writeJSON(w, 404, requestAPIError(r, "NOT_FOUND", "file reference is unavailable"))
		return
	}
	v, e := s.fileLookup(key, id, true)
	if e != nil {
		s.chargeRejectedFileLookup(r, id)
		failure := fileTransferError(e)
		if e.Error() == "CAPABILITY_DENIED" {
			failure = deliveryFailure("CAPABILITY_DENIED", "file reference is not allowed", "not_started")
		}
		writeJSON(w, failure.Status, resultError(authRequestID(r), failure))
		return
	}
	if r.Header.Get("Range") != "" {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", v.descriptor.SizeBytes))
		writeJSON(w, 416, requestAPIError(r, "INVALID_REQUEST", "Range requests are not supported"))
		return
	}
	s.streamFile(w, r, v)
}

// Hold the last byte until transfer_end verifies size and digest. A truncated
// HTTP response therefore cannot have the declared complete Content-Length.
func (s *Server) streamFile(w http.ResponseWriter, r *http.Request, v fileReference) *agentCallResult {
	started := false
	var received int64
	var tail []byte
	headers := func() {
		w.Header().Set("Content-Type", v.descriptor.MIMEType)
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": v.descriptor.Name}))
		w.Header().Set("Content-Length", strconv.FormatInt(v.descriptor.SizeBytes, 10))
		w.Header().Set("X-Request-ID", v.requestID)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
	}
	controller := http.NewResponseController(w)
	d, _ := time.ParseDuration(v.output.TransferTimeout)
	_ = controller.SetWriteDeadline(time.Now().Add(d))
	defer controller.SetWriteDeadline(time.Time{})
	e := s.transferFile(r.Context(), v, func(ctx context.Context, b []byte) (int, error) {
		if e := ctx.Err(); e != nil {
			return 0, e
		}
		received += int64(len(b))
		payload := b
		if received == v.descriptor.SizeBytes && len(b) > 0 {
			tail = append([]byte(nil), b[len(b)-1])
			payload = b[:len(b)-1]
		}
		if len(payload) > 0 {
			if !started {
				headers()
				started = true
			}
			n, e := w.Write(payload)
			if e != nil {
				return n, e
			}
			if n != len(payload) {
				return n, io.ErrShortWrite
			}
		}
		return len(b), nil
	})
	if e != nil {
		failure := fileTransferError(e)
		if started {
			panic(http.ErrAbortHandler)
		}
		writeJSON(w, failure.Status, resultError(authRequestID(r), failure))
		return &failure
	}
	if !started {
		headers()
	}
	if len(tail) > 0 {
		if n, e := w.Write(tail); e != nil || n != len(tail) {
			panic(http.ErrAbortHandler)
		}
	} else {
		w.WriteHeader(200)
	}
	return nil
}
