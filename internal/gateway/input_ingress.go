package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/viewlegacy/onprest/internal/contract"
	"github.com/viewlegacy/onprest/internal/protocol"
)

type ingressError struct {
	status int
	code   string
}

func (e *ingressError) Error() string { return e.code }
func invalidInput() error             { return &ingressError{400, "INVALID_REQUEST"} }
func inputLimit(status int) error     { return &ingressError{status, "LIMIT_EXCEEDED"} }
func inputResult(e error) agentCallResult {
	var execution inputExecutionError
	if errors.As(e, &execution) {
		if errors.Is(e, context.Canceled) || errors.Is(e, context.DeadlineExceeded) {
			return deliveryFailure("EXECUTION_TIMEOUT", "execution deadline exceeded", "unknown")
		}
		return deliveryFailure("AGENT_OFFLINE", "agent is not connected", "unknown")
	}
	var reject *ingressError
	if errors.As(e, &reject) {
		r := deliveryFailure(reject.code, "input could not be accepted", "not_started")
		r.Status = reject.status
		return r
	}
	if errors.Is(e, context.Canceled) || errors.Is(e, context.DeadlineExceeded) {
		return deliveryFailure("EXECUTION_TIMEOUT", "input deadline exceeded", "not_started")
	}
	code := e.Error()
	if code == "agent offline" || code == "agent disconnected" || code == "OPENAPI_NOT_READY" {
		code = "AGENT_OFFLINE"
	}
	if code == "transfer capacity unavailable" {
		code = "BUSY"
	}
	switch code {
	case "BUSY", "CONFLICT", "NOT_FOUND", "RATE_LIMITED", "VALIDATION_FAILED", "LIMIT_EXCEEDED", "AGENT_OFFLINE", "INVALID_REQUEST", "EXECUTION_TIMEOUT", "DEPENDENCY_UNAVAILABLE", "PERMISSION_DENIED":
	default:
		code = "EXECUTION_FAILED"
	}
	message := "input could not be accepted"
	if code == "AGENT_OFFLINE" {
		message = "agent is not connected"
	}
	if code == "NOT_FOUND" {
		message = "capability is not defined"
	}
	result := deliveryFailure(code, message, "not_started")
	return result
}

type bodyLease struct {
	mu      sync.Mutex
	refs    int
	closed  bool
	release func()
}

func (l *bodyLease) Close() {
	if l != nil {
		l.mu.Lock()
		if !l.closed {
			l.closed = true
			l.refs--
			if l.refs == 0 {
				l.release()
			}
		}
		l.mu.Unlock()
	}
}
func (l *bodyLease) hold() func() {
	l.mu.Lock()
	l.refs++
	l.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			l.refs--
			if l.refs == 0 {
				l.release()
			}
			l.mu.Unlock()
		})
	}
}

type bodyLeaseKey struct{}
type bodyStartKey struct{}
type inputConsumedKey struct{}

func inputConsumed(ctx context.Context) {
	if f, _ := ctx.Value(inputConsumedKey{}).(func()); f != nil {
		f()
	}
}
func (s *Server) acquireBody(r *http.Request) (*http.Request, *bodyLease, bool) {
	select {
	case s.bodySlots <- struct{}{}:
	default:
		return r, nil, false
	}
	lease := &bodyLease{refs: 1, release: func() { <-s.bodySlots }}
	ctx := context.WithValue(r.Context(), bodyLeaseKey{}, lease)
	ctx = context.WithValue(ctx, bodyStartKey{}, time.Now())
	return r.WithContext(ctx), lease, true
}
func releaseBody(ctx context.Context) {
	inputConsumed(ctx)
	if l, _ := ctx.Value(bodyLeaseKey{}).(*bodyLease); l != nil {
		l.Close()
	}
}

func inputSchema(meta protocol.CapabilityMetadata) (*contract.Schema, contract.Limits, error) {
	raw, e := contract.CanonicalJSON(meta.InputSchema)
	if e != nil {
		return nil, contract.Limits{}, e
	}
	schema, e := contract.CompileSchema(raw, true)
	l := meta.InputLimits
	return schema, contract.Limits{MaxBytes: l["max_input_bytes"], MaxDepth: l["max_json_depth"], MaxItems: l["max_json_items"], MaxFileBytes: l["max_input_file_bytes"], MaxTotalFileBytes: l["max_total_input_file_bytes"], MaxFiles: l["max_input_files"]}, e
}
func (s *Server) ingressMetadata(name string) (protocol.CapabilityMetadata, error) {
	s.agentMu.RLock()
	defer s.agentMu.RUnlock()
	if s.agent == nil || s.openapi == nil || s.capabilities == nil {
		return protocol.CapabilityMetadata{}, errors.New("AGENT_OFFLINE")
	}
	m, ok := s.capabilities[name]
	if !ok {
		return m, errors.New("NOT_FOUND")
	}
	return m, nil
}
func normalizeMIME(raw string) (string, error) {
	media, _, e := mime.ParseMediaType(raw)
	if e != nil || !strings.Contains(media, "/") || strings.Contains(media, "*") {
		return "", invalidInput()
	}
	return strings.ToLower(media), nil
}

type inlineReader struct {
	mu      sync.Mutex
	decoder io.Reader
}

func (r *inlineReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.decoder == nil {
		return 0, io.ErrClosedPipe
	}
	return r.decoder.Read(p)
}
func (r *inlineReader) Close() error { r.mu.Lock(); defer r.mu.Unlock(); r.decoder = nil; return nil }
func inlineFile(schema *contract.Schema, raw any, pointer, id string) (contract.FileValue, InputStream, error) {
	object, ok := raw.(map[string]any)
	if !ok || len(object) != 3 {
		return contract.FileValue{}, InputStream{}, invalidInput()
	}
	name, nok := object["name"].(string)
	media, mok := object["mime_type"].(string)
	data, dok := object["data"].(string)
	if !nok || !mok || !dok || contract.ValidateBasename(name) != nil {
		return contract.FileValue{}, InputStream{}, invalidInput()
	}
	media, e := normalizeMIME(media)
	if e != nil {
		return contract.FileValue{}, InputStream{}, e
	}
	// Reject whitespace and URL alphabets before the standard decoder (which
	// otherwise ignores CR/LF). Strict checks canonical trailing bits/padding.
	if len(data)%4 != 0 || strings.IndexFunc(data, func(r rune) bool {
		return !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '+' || r == '/' || r == '=')
	}) >= 0 {
		return contract.FileValue{}, InputStream{}, invalidInput()
	}
	hash := sha256.New()
	decoder := base64.NewDecoder(base64.StdEncoding.Strict(), strings.NewReader(data))
	var buffer [protocol.ChunkBytes]byte
	var size int64
	for {
		n, err := decoder.Read(buffer[:])
		size += int64(n)
		hash.Write(buffer[:n])
		if size > schema.File.MaxBytes {
			return contract.FileValue{}, InputStream{}, inputLimit(422)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return contract.FileValue{}, InputStream{}, invalidInput()
		}
	}
	value := contract.FileValue{ID: "wire", RequestID: id, Pointer: pointer, Name: name, MIMEType: media, SizeBytes: size, SHA256: hex.EncodeToString(hash.Sum(nil))}
	if e := schema.File.Validate(value); e != nil {
		return value, InputStream{}, &ingressError{400, "VALIDATION_FAILED"}
	}
	reader := &inlineReader{decoder: base64.NewDecoder(base64.StdEncoding.Strict(), strings.NewReader(data))}
	return value, InputStream{Pointer: pointer, Name: name, MIMEType: media, Reader: reader}, nil
}
func pointerKey(k string) string {
	return strings.ReplaceAll(strings.ReplaceAll(k, "~", "~0"), "/", "~1")
}
func normalizeInline(schema *contract.Schema, v any, pointer, id string, streams *[]InputStream) (any, error) {
	if schema == nil {
		return v, nil
	}
	if schema.File != nil {
		value, stream, e := inlineFile(schema, v, pointer, id)
		if e == nil {
			*streams = append(*streams, stream)
		}
		return value, e
	}
	switch x := v.(type) {
	case map[string]any:
		for _, k := range schema.PropertyOrder {
			if raw, ok := x[k]; ok {
				n, e := normalizeInline(schema.Properties[k], raw, pointer+"/"+pointerKey(k), id, streams)
				if e != nil {
					return nil, e
				}
				x[k] = n
			}
		}
	case []any:
		for i, raw := range x {
			n, e := normalizeInline(schema.Items, raw, pointer+"/"+strconv.Itoa(i), id, streams)
			if e != nil {
				return nil, e
			}
			x[i] = n
		}
	}
	return v, nil
}
func placeholders(v any) any {
	switch x := v.(type) {
	case contract.FileValue:
		return nil
	case map[string]any:
		for k, raw := range x {
			x[k] = placeholders(raw)
		}
	case []any:
		for i, raw := range x {
			x[i] = placeholders(raw)
		}
	}
	return v
}
func closeStreams(streams []InputStream) {
	for i := range streams {
		if streams[i].Reader != nil {
			streams[i].Reader.Close()
			streams[i].Reader = nil
		}
	}
}
func validateIngress(schema *contract.Schema, params map[string]any, l contract.Limits) (map[string]any, error) {
	p, _, e := contract.ValidateInput(schema, params, l)
	if e != nil {
		if strings.Contains(e.Error(), "limit exceeded") {
			return nil, inputLimit(422)
		}
		return nil, &ingressError{400, "VALIDATION_FAILED"}
	}
	return p, nil
}
func (s *Server) callInput(ctx context.Context, name string, params map[string]any) agentCallResult {
	meta, e := s.ingressMetadata(name)
	if e != nil {
		return inputResult(e)
	}
	schema, limits, e := inputSchema(meta)
	if e != nil {
		s.log("input_schema_rejected", map[string]any{"reason": e.Error()})
		return invalidAgentResult("")
	}
	if meta.InputTimeout == "" {
		normalized, e := validateIngress(schema, params, limits)
		params = nil
		if e != nil {
			return inputResult(e)
		}
		return s.callAgent(ctx, name, normalized)
	}
	identity, _ := ctx.Value(callIdentityKey{}).(callIdentity)
	streams := []InputStream{}
	defer func() { closeStreams(streams) }()
	_, e = normalizeInline(schema, params, "", identity.ID, &streams)
	if e != nil {
		return inputResult(e)
	}
	params, e = validateIngress(schema, params, limits)
	if e != nil {
		return inputResult(e)
	}
	params = placeholders(params).(map[string]any)
	var connection *agentConn
	response, e := s.transferInput(ctx, inputBegin(ctx, name, meta), func(send func(InputStream) (protocol.Frame, error)) (map[string]any, error) {
		for i := range streams {
			if _, e := send(streams[i]); e != nil {
				return nil, e
			}
			streams[i].Reader = nil
		}
		streams = nil
		p := params
		params = nil
		return p, nil
	}, func(ac *agentConn) { connection = ac; params = nil; streams = nil; releaseBody(ctx) })
	if e != nil {
		closeStreams(streams)
		streams = nil
		params = nil
		result := inputResult(e)
		if response.Metrics != nil {
			if !validInputErrorMetrics(response.Metrics, meta) {
				return invalidAgentResult(identityID(ctx))
			}
			result.Metrics = response.Metrics
		}
		return result
	}
	return s.inputResponse(ctx, connection, name, meta, response)
}
func inputBegin(ctx context.Context, name string, meta protocol.CapabilityMetadata) protocol.Frame {
	identity, _ := ctx.Value(callIdentityKey{}).(callIdentity)
	start, _ := ctx.Value(bodyStartKey{}).(time.Time)
	if start.IsZero() {
		start = time.Now()
	}
	d, _ := time.ParseDuration(meta.InputTimeout)
	return protocol.Frame{Type: "request_begin", ID: identity.ID, Capability: name, CallerBinding: identity.Binding, DefinitionHash: meta.DefinitionHash, InputDeadline: start.Add(d)}
}
func (s *Server) inputResponse(ctx context.Context, ac *agentConn, name string, meta protocol.CapabilityMetadata, response protocol.Response) agentCallResult {
	identity, _ := ctx.Value(callIdentityKey{}).(callIdentity)
	if response.Error != nil {
		if response.Error.Validate() != nil || response.Metrics != nil && !validInputErrorMetrics(response.Metrics, meta) {
			return invalidAgentResult(identity.ID)
		}
		status, code := agentErrorStatus(response.Error.Code)
		return agentCallResult{RequestID: identity.ID, Status: status, Code: code, Message: response.Error.Message, Failure: response.Error, Metrics: response.Metrics}
	}
	payload, metrics, e := validateAgentResult(identity.ID, response, meta, true)
	if e != nil {
		s.log("agent_output_rejected", map[string]any{"request_id": identity.ID, "reason": e.Error()})
		return invalidAgentResult(identity.ID)
	}
	result := agentCallResult{RequestID: identity.ID, Status: 200, Payload: payload, Metrics: metrics}
	return s.registerFiles(ac, identity, name, meta, result)
}

// multipartGuard checks framing before multipart.Reader can accumulate a MIME
// header. File lines retain only a bounded prefix and never a full line/body.
type multipartGuard struct {
	source                 io.Reader
	boundary               string
	prefix                 []byte
	line                   int64
	headerBytes, framing   int64
	headers, closed, first bool
	previous               byte
	failure                error
}

func (g *multipartGuard) Read(p []byte) (int, error) {
	if g.failure != nil {
		return 0, g.failure
	}
	n, e := g.source.Read(p)
	for i, b := range p[:n] {
		if g.closed {
			g.failure = invalidInput()
			return i, g.failure
		}
		g.line++
		if len(g.prefix) < len(g.boundary)+6 {
			g.prefix = append(g.prefix, b)
		}
		if g.headers {
			g.headerBytes++
			g.framing++
			if g.headerBytes > 8192 || g.framing > 1<<20 {
				g.failure = inputLimit(413)
				return i, g.failure
			}
		}
		if b == '\n' {
			line := string(g.prefix)
			if g.line != int64(len(g.prefix)) {
				line = ""
			}
			boundary := "--" + g.boundary
			if g.first {
				if line == boundary+"--\r\n" {
					g.first = false
					g.closed = true
					g.line = 0
					g.prefix = g.prefix[:0]
					g.previous = b
					continue
				}
				if line != boundary+"\r\n" {
					g.failure = invalidInput()
					return i, g.failure
				}
				g.first = false
				g.headers = true
				g.headerBytes = 0
				g.framing += g.line
			} else if g.headers {
				if g.previous != '\r' {
					g.failure = invalidInput()
					return i, g.failure
				}
				if g.line == 2 {
					g.headers = false
				}
			} else if line == boundary+"\r\n" || line == boundary+"--\r\n" {
				g.framing += g.line
				g.headers = line == boundary+"\r\n"
				g.closed = !g.headers
				g.headerBytes = 0
			}
			if g.framing > 1<<20 {
				g.failure = inputLimit(413)
				return i, g.failure
			}
			g.line = 0
			g.prefix = g.prefix[:0]
		}
		g.previous = b
	}
	if e == io.EOF && !g.closed {
		return n, invalidInput()
	}
	return n, e
}

type partReader struct{ io.Reader }

func (r partReader) Close() error { return nil } // Part.Close drains; cancellation must not.
func (s *Server) callMultipart(w http.ResponseWriter, r *http.Request, name string, meta protocol.CapabilityMetadata, boundary string) agentCallResult {
	schema, limits, e := inputSchema(meta)
	if e != nil {
		s.log("input_schema_rejected", map[string]any{"reason": e.Error()})
		return invalidAgentResult("")
	}
	capability := &contract.Capability{Input: schema}
	if meta.InputTimeout == "" || capability.InputMediaSchemas().Multipart == nil {
		return inputResult(&ingressError{415, "INVALID_REQUEST"})
	}
	ctx := r.Context()
	begin := inputBegin(ctx, name, meta)
	_ = http.NewResponseController(w).SetReadDeadline(begin.InputDeadline)
	phaseCtx, phaseCancel := context.WithDeadline(ctx, begin.InputDeadline)
	defer phaseCancel()
	stopPhase := context.AfterFunc(phaseCtx, func() { r.Body.Close() })
	defer stopPhase()
	// Closing HTTP input also stops a sender blocked in Read during cancellation.
	stop := context.AfterFunc(ctx, func() { r.Body.Close() })
	defer stop()
	guard := &multipartGuard{source: http.MaxBytesReader(w, r.Body, s.cfg.MaxFileRequestBodyBytes), boundary: boundary, first: true}
	reader := multipart.NewReader(guard, boundary)
	params := map[string]any{}
	var jsonBytes int64
	partCount := int64(0)
	inputFinished := false
	var connection *agentConn
	response, e := s.transferInput(ctx, begin, func(send func(InputStream) (protocol.Frame, error)) (map[string]any, error) {
		for {
			part, e := reader.NextRawPart()
			if guard.failure != nil {
				return nil, guard.failure
			}
			if e == io.EOF {
				break
			}
			if e != nil {
				var wireLimit *http.MaxBytesError
				if errors.As(e, &wireLimit) {
					return nil, e
				}
				return nil, invalidInput()
			}
			partCount++
			if partCount > meta.InputLimits["max_multipart_parts"] {
				return nil, inputLimit(422)
			}
			for _, values := range part.Header {
				if len(values) != 1 {
					return nil, invalidInput()
				}
			}
			if part.Header["Content-Transfer-Encoding"] != nil {
				return nil, invalidInput()
			}
			kind, attributes, e := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
			if e != nil || kind != "form-data" {
				return nil, invalidInput()
			}
			key, ok := attributes["name"]
			node := schema.Properties[key]
			if !ok || node == nil {
				return nil, invalidInput()
			}
			leaf := node
			array := node.File == nil && node.Items != nil && node.Items.File != nil
			if array {
				leaf = node.Items
			}
			media, e := normalizeMIME(part.Header.Get("Content-Type"))
			if e != nil {
				return nil, e
			}
			if leaf.File != nil {
				filename, ok := attributes["filename"]
				if !ok || contract.ValidateBasename(filename) != nil {
					return nil, invalidInput()
				}
				pointer := "/" + pointerKey(key)
				if array {
					a, _ := params[key].([]any)
					if maximum, ok := node.Constraints["maxItems"].(int64); ok && int64(len(a)) >= maximum {
						return nil, &ingressError{400, "VALIDATION_FAILED"}
					}
					pointer += "/" + strconv.Itoa(len(a))
				} else if _, exists := params[key]; exists {
					return nil, invalidInput()
				}
				ready, e := send(InputStream{Pointer: pointer, Name: filename, MIMEType: media, Reader: partReader{part}})
				if e != nil {
					return nil, e
				}
				value := contract.FileValue{Pointer: pointer, ID: ready.FileID, RequestID: begin.ID, Name: filename, MIMEType: media, SizeBytes: ready.SizeBytes, SHA256: ready.SHA256}
				if array {
					a, _ := params[key].([]any)
					params[key] = append(a, value)
				} else {
					params[key] = value
				}
			} else {
				if _, exists := params[key]; exists {
					return nil, invalidInput()
				}
				if media != "application/json" {
					return nil, invalidInput()
				}
				raw, e := io.ReadAll(io.LimitReader(part, s.cfg.MaxRequestBodyBytes-jsonBytes+1))
				if e != nil {
					return nil, e
				}
				jsonBytes += int64(len(raw))
				if jsonBytes > s.cfg.MaxRequestBodyBytes {
					return nil, inputLimit(413)
				}
				value, e := contract.DecodeWireJSON(raw, s.cfg.MaxRequestBodyBytes, limits)
				raw = nil
				if e != nil {
					return nil, invalidInput()
				}
				streams := []InputStream{}
				value, e = normalizeInline(node, value, "/"+pointerKey(key), begin.ID, &streams)
				if e != nil {
					closeStreams(streams)
					return nil, e
				}
				for i := range streams {
					if _, e := send(streams[i]); e != nil {
						closeStreams(streams)
						return nil, e
					}
					streams[i].Reader = nil
				}
				streams = nil
				params[key] = value
			}
		}
		// Reader buffers cannot conceal an epilogue: the raw guard observes all bytes.
		if _, e := io.Copy(io.Discard, guard); e != nil {
			return nil, e
		}
		p, e := validateIngress(schema, params, limits)
		params = nil
		if e != nil {
			return nil, e
		}
		return placeholders(p).(map[string]any), nil
	}, func(ac *agentConn) {
		connection = ac
		inputFinished = true
		params = nil
		reader = nil
		guard = nil
		stopPhase()
		phaseCancel()
		_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
		releaseBody(ctx)
	})
	if e != nil {
		if !inputFinished && phaseCtx.Err() != nil {
			e = phaseCtx.Err()
		}
		var tooLarge *http.MaxBytesError
		if errors.As(e, &tooLarge) {
			e = inputLimit(413)
		}
		if _, ok := e.(*ingressError); !ok {
			var rejected inputRejected
			if !errors.As(e, &rejected) && !errors.Is(e, context.DeadlineExceeded) && !errors.Is(e, context.Canceled) && strings.Contains(e.Error(), "multipart") {
				e = invalidInput()
			}
		}
		result := inputResult(e)
		if response.Metrics != nil {
			if !validInputErrorMetrics(response.Metrics, meta) {
				return invalidAgentResult(identityID(ctx))
			}
			result.Metrics = response.Metrics
		}
		return result
	}
	return s.inputResponse(ctx, connection, name, meta, response)
}

func identityID(ctx context.Context) string {
	identity, _ := ctx.Value(callIdentityKey{}).(callIdentity)
	return identity.ID
}
func validInputErrorMetrics(m *protocol.Metrics, meta protocol.CapabilityMetadata) bool {
	return validInputMetrics(m, meta) && m.ExecutorKind == meta.ExecutorKind && m.FileCount == 0 && m.ResponseBytes == 0 && m.ReturnedRows == nil && m.AffectedRows == nil
}

// MCP cannot select an input definition before parsing the complete request.
// Its read phase uses the largest authorized file budget in the current
// metadata generation, bounded by the ordinary HTTP body budget. Once selected,
// inputBegin still uses that definition's deadline from the same admission time.
func (s *Server) inputBodyReadBudget(name string, key *APIKey) time.Duration {
	budget := s.cfg.BodyReadTimeout
	s.agentMu.RLock()
	defer s.agentMu.RUnlock()
	var input time.Duration
	if key == nil {
		input, _ = time.ParseDuration(s.capabilities[name].InputTimeout)
	} else {
		for candidate, meta := range s.capabilities {
			if allowed(*key, candidate) {
				d, _ := time.ParseDuration(meta.InputTimeout)
				if meta.InputTimeout == "" {
					d = budget
				}
				if d > input {
					input = d
				}
			}
		}
	}
	if input > 0 && input < budget {
		budget = input
	}
	return budget
}

func startInputBodyRead(w http.ResponseWriter, r *http.Request, budget time.Duration) func() error {
	start, _ := r.Context().Value(bodyStartKey{}).(time.Time)
	if start.IsZero() {
		start = time.Now()
	}
	deadline := start.Add(budget)
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(deadline)
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	stop := context.AfterFunc(ctx, func() { r.Body.Close() })
	return func() error {
		e := ctx.Err()
		if e == nil && !time.Now().Before(deadline) {
			e = context.DeadlineExceeded
		}
		stop()
		cancel()
		// Keep an expired deadline while net/http finishes an incomplete body;
		// clearing it would allow response flushing to block on body draining.
		if e == nil {
			_ = controller.SetReadDeadline(time.Time{})
		}
		return e
	}
}

func (s *Server) inputDecodeLimits(name string, key *APIKey) contract.Limits {
	l := contract.Limits{MaxDepth: 32, MaxItems: 100000, MaxFiles: 16}
	s.agentMu.RLock()
	defer s.agentMu.RUnlock()
	if key == nil {
		if m, ok := s.capabilities[name]; ok {
			return contract.Limits{MaxDepth: m.InputLimits["max_json_depth"], MaxItems: m.InputLimits["max_json_items"], MaxFiles: m.InputLimits["max_input_files"]}
		}
	} else {
		for candidate, m := range s.capabilities {
			if allowed(*key, candidate) {
				if d := m.InputLimits["max_json_depth"]; d > l.MaxDepth {
					l.MaxDepth = d
				}
				if items := m.InputLimits["max_json_items"]; items > l.MaxItems {
					l.MaxItems = items
				}
				if files := m.InputLimits["max_input_files"]; files > l.MaxFiles {
					l.MaxFiles = files
				}
			}
		}
	}
	return l
}
