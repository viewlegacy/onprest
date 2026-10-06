package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/viewlegacy/onprest/internal/contract"
	"github.com/viewlegacy/onprest/internal/protocol"
)

func testOutputMetadata() *protocol.OutputMetadata {
	return &protocol.OutputMetadata{REST: "reference", MCP: "inline", MaxFileBytes: 32 << 20, MaxTotalBytes: 64 << 20, MaxFiles: 16, InlineBytes: 1 << 20, TTL: "15m", TransferTimeout: "1s", MCPOutputSchema: map[string]any{"type": "object"}}
}
func testFileDescriptor() contract.PublicFileDescriptor {
	d := sha256.Sum256([]byte("abc"))
	return contract.PublicFileDescriptor{ID: "file_1", ArtifactMetadata: contract.ArtifactMetadata{Name: "report.bin", MIMEType: "application/octet-stream", SizeBytes: 3, SHA256: hex.EncodeToString(d[:])}, ExpiresAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)}
}
func TestFileOutputMetadataDescriptorsAndOrigin(t *testing.T) {
	for _, origin := range []string{"https://example.com", "http://127.0.0.1:8080", "http://localhost:8080/", ""} {
		if _, e := normalizeFileOrigin(origin); e != nil {
			t.Fatal(origin, e)
		}
	}
	for _, origin := range []string{"https://example.com/path", "https://user:pass@example.com", "https://example.com?", "https://example.com/#x", "http://example.com", "ftp://example.com", "https://example.com/%2f", "https://example.com:invalid"} {
		if _, e := normalizeFileOrigin(origin); e == nil {
			t.Fatal("invalid origin", origin)
		}
	}
	output := testOutputMetadata()
	f := testFileDescriptor()
	if e := validateOutputMetadata(output); e != nil {
		t.Fatal(e)
	}
	if e := validateFileDescriptors([]contract.PublicFileDescriptor{f}, output); e != nil {
		t.Fatal(e)
	}
	for name, alter := range map[string]func(*contract.PublicFileDescriptor){"path": func(f *contract.PublicFileDescriptor) { f.Name = "a/b" }, "control": func(f *contract.PublicFileDescriptor) { f.Name = "a\r\nb" }, "MIME": func(f *contract.PublicFileDescriptor) { f.MIMEType = "text/plain; charset=utf-8" }, "URI": func(f *contract.PublicFileDescriptor) { f.URI = "https://evil.example" }, "size": func(f *contract.PublicFileDescriptor) { f.SizeBytes = -1 }, "digest": func(f *contract.PublicFileDescriptor) { f.SHA256 = strings.Repeat("A", 64) }, "expiry": func(f *contract.PublicFileDescriptor) {
		f.ExpiresAt = time.Now().Add(-time.Second).Format(time.RFC3339Nano)
	}, "id": func(f *contract.PublicFileDescriptor) { f.ID = "../x" }} {
		t.Run(name, func(t *testing.T) {
			v := f
			alter(&v)
			if validateFileDescriptors([]contract.PublicFileDescriptor{v}, output) == nil {
				t.Fatal("invalid descriptor")
			}
		})
	}
	if validateFileDescriptors([]contract.PublicFileDescriptor{f, f}, output) == nil {
		t.Fatal("duplicate")
	}
	output.MaxFileBytes = 3
	output.MaxTotalBytes = 3
	if validateFileDescriptors([]contract.PublicFileDescriptor{f}, output) != nil {
		t.Fatal("exact byte boundary")
	}
	output.MaxFileBytes = 2
	if validateFileDescriptors([]contract.PublicFileDescriptor{f}, output) == nil {
		t.Fatal("file byte excess")
	}
	server := NewServer(Config{}, io.Discard)
	m := protocol.Metadata{Capabilities: map[string]protocol.CapabilityMetadata{"test": {Output: testOutputMetadata()}}}
	if e := server.validateFileOrigin(m); e == nil {
		t.Fatal("missing origin accepted")
	}
	server.agent = newAgentConn(newDeterministicCancelTransport())
	server.metadataError = "ONPREST_PUBLIC_BASE_URL is required for file reference delivery"
	result := server.callAgent(context.Background(), "test", map[string]any{})
	if result.Code != "DEPENDENCY_UNAVAILABLE" || result.Failure.Outcome != "not_started" || server.openapi != nil {
		t.Fatal("missing origin advertised/called", result)
	}
	output.REST = "direct"
	output.MCP = "inline"
	m.Capabilities["test"] = protocol.CapabilityMetadata{Output: output}
	if e := server.validateFileOrigin(m); e != nil {
		t.Fatal("direct inline requires origin", e)
	}
	m.Capabilities["test"] = protocol.CapabilityMetadata{}
	if server.validateFileOrigin(m) != nil {
		t.Fatal("JSON requires origin")
	}
}
func TestFileReferenceLookupRechecksCredentialCapabilityHashGenerationAndTTL(t *testing.T) {
	key := APIKey{Name: "same-name", KeyHash: "credential1", Capabilities: []string{"test"}}
	server := NewServer(Config{PublicBaseURL: "https://gateway.example"}, io.Discard)
	ac := newAgentConn(nil)
	server.agent = ac
	output := testOutputMetadata()
	meta := protocol.CapabilityMetadata{DefinitionHash: strings.Repeat("a", 64), Output: output}
	server.capabilities = map[string]protocol.CapabilityMetadata{"test": meta}
	f := testFileDescriptor()
	payload, _ := json.Marshal(contract.NewSuccessEnvelope("request", nil, []contract.PublicFileDescriptor{f}))
	result := server.registerFiles(ac, callIdentity{ID: "request", Binding: credentialBinding(key)}, "test", meta, agentCallResult{RequestID: "request", Payload: payload, Status: 200})
	if !result.OK() {
		t.Fatal(result)
	}
	if _, e := server.fileLookup(key, f.ID, true); e != nil {
		t.Fatal(e)
	}
	changed := key
	changed.KeyHash = "credential2"
	if _, e := server.fileLookup(changed, f.ID, true); e == nil {
		t.Fatal("same-name credential rotation")
	}
	if len(server.references) != 1 {
		t.Fatal("denial mutated live reference")
	}
	changed = key
	changed.Capabilities = nil
	if _, e := server.fileLookup(changed, f.ID, true); e == nil {
		t.Fatal("revoked capability")
	}
	server.capabilities["test"] = protocol.CapabilityMetadata{DefinitionHash: strings.Repeat("b", 64)}
	if _, e := server.fileLookup(key, f.ID, true); e == nil {
		t.Fatal("definition change")
	}
	server.capabilities["test"] = meta
	server.references[f.ID] = fileReference{descriptor: f, requestID: "request", binding: credentialBinding(key), capability: "test", hash: meta.DefinitionHash, connection: ac, output: *output, expiry: time.Now().Add(time.Minute)}
	server.agent = newAgentConn(nil)
	if _, e := server.fileLookup(key, f.ID, true); e == nil {
		t.Fatal("connection generation")
	}
	server.agent = ac
	v := server.references[f.ID]
	v.expiry = time.Now().Add(-time.Second)
	server.references[f.ID] = v
	if _, e := server.fileLookup(key, f.ID, true); e == nil {
		t.Fatal("expiry")
	}
}

func TestFileHTTPCompletionWaitsForVerifiedTransferEnd(t *testing.T) {
	for _, mode := range []string{"success", "digest", "size", "cancel", "zero"} {
		t.Run(mode, func(t *testing.T) {
			conn := newDeterministicCancelTransport()
			ac := newAgentConn(conn)
			ac.initializeTransfers()
			server := NewServer(Config{}, io.Discard)
			server.agent = ac
			go ac.scheduler.Run()
			defer func() { conn.Close(); ac.disconnectPending() }()
			descriptor := testFileDescriptor()
			if mode == "zero" {
				sum := sha256.Sum256(nil)
				descriptor.SizeBytes = 0
				descriptor.SHA256 = hex.EncodeToString(sum[:])
			}
			v := fileReference{descriptor: descriptor, requestID: "request", connection: ac, output: *testOutputMetadata()}
			peerDone := make(chan struct{})
			go func() {
				defer close(peerDone)
				raw := awaitTransportWrite(t, conn)
				var f protocol.Frame
				if json.Unmarshal(raw, &f) != nil {
					t.Error("open")
				}
				if mode != "zero" {
					ac.handleTransfer(protocol.MustJSON(protocol.Frame{Type: "transfer_chunk", ID: f.ID, TransferID: f.TransferID, Sequence: 0, DataBase64: base64.StdEncoding.EncodeToString([]byte("abc"))}))
					awaitTransportWrite(t, conn)
				}
				if mode == "cancel" {
					ac.transferCancel()
					return
				}
				digest := descriptor.SHA256
				size := descriptor.SizeBytes
				if mode == "digest" {
					digest = strings.Repeat("0", 64)
				}
				if mode == "size" {
					size++
				}
				ac.handleTransfer(protocol.MustJSON(protocol.Frame{Type: "transfer_end", ID: f.ID, TransferID: f.TransferID, Size: size, SHA256: digest}))
			}()
			recorder := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "http://gateway/file", nil)
			aborted := false
			func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						if recovered != http.ErrAbortHandler {
							panic(recovered)
						}
						aborted = true
					}
				}()
				server.streamFile(recorder, req, v)
			}()
			<-peerDone
			if mode == "success" {
				if aborted || recorder.Body.String() != "abc" || recorder.Header().Get("Content-Length") != "3" {
					t.Fatal("valid completion", recorder.Body.String())
				}
			} else if mode == "zero" {
				if aborted || recorder.Body.Len() != 0 || recorder.Header().Get("Content-Length") != "0" {
					t.Fatal("zero completion")
				}
			} else if !aborted || recorder.Body.String() != "ab" {
				t.Fatal("invalid transfer appeared complete", aborted, recorder.Body.String())
			}
			if len(ac.transferCalls) != 0 {
				t.Fatal("exchange leaked")
			}
		})
	}
}
func TestFileConcurrentSameRequestRefusesWithoutQueue(t *testing.T) {
	ac := newAgentConn(nil)
	ac.initializeTransfers()
	defer ac.transferCancel()
	defer ac.transfers.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, release, e := ac.exchange(deadlineOf(ctx), "request")
	if e != nil {
		t.Fatal(e)
	}
	defer release()
	if _, _, e = ac.exchange(deadlineOf(ctx), "request"); e == nil {
		t.Fatal("concurrent request queued")
	}
	if len(ac.transferCalls) != 1 {
		t.Fatal("state replaced")
	}
}

func TestFileHTTPRealClientRejectsUnverifiedCompletion(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(strconv.FormatBool(valid), func(t *testing.T) {
			conn := newDeterministicCancelTransport()
			ac := newAgentConn(conn)
			ac.initializeTransfers()
			go ac.scheduler.Run()
			defer func() { conn.Close(); ac.disconnectPending() }()
			s := NewServer(Config{}, io.Discard)
			s.agent = ac
			body := bytes.Repeat([]byte("a"), 8192)
			sum := sha256.Sum256(body)
			descriptor := testFileDescriptor()
			descriptor.SizeBytes = int64(len(body))
			descriptor.SHA256 = hex.EncodeToString(sum[:])
			v := fileReference{descriptor: descriptor, requestID: "request", connection: ac, output: *testOutputMetadata()}
			server := httptest.NewServer(s.withRecovery(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.streamFile(w, r, v) })))
			defer server.Close()
			peerDone := make(chan struct{})
			go func() {
				defer close(peerDone)
				var f protocol.Frame
				json.Unmarshal(awaitTransportWrite(t, conn), &f)
				ac.handleTransfer(protocol.MustJSON(protocol.Frame{Type: "transfer_chunk", ID: f.ID, TransferID: f.TransferID, Sequence: 0, DataBase64: base64.StdEncoding.EncodeToString(body)}))
				awaitTransportWrite(t, conn)
				digest := descriptor.SHA256
				if !valid {
					digest = strings.Repeat("0", 64)
				}
				ac.handleTransfer(protocol.MustJSON(protocol.Frame{Type: "transfer_end", ID: f.ID, TransferID: f.TransferID, Size: descriptor.SizeBytes, SHA256: digest}))
			}()
			response, e := http.Get(server.URL)
			if e != nil {
				t.Fatal(e)
			}
			defer response.Body.Close()
			received, e := io.ReadAll(response.Body)
			<-peerDone
			if valid {
				if e != nil || !bytes.Equal(received, body) {
					t.Fatal("valid transfer", e)
				}
			} else if e == nil || len(received) >= len(body) {
				t.Fatal("invalid terminal completed", len(received), e)
			}
		})
	}
}

func TestFileMCPInlineEncodingBudgetMatchesHTTP(t *testing.T) {
	s := NewServer(Config{}, io.Discard)
	output := testOutputMetadata()
	s.capabilities = map[string]protocol.CapabilityMetadata{"test": {Output: output}}
	id := "request <>& 日本\u2028\u2029"
	payload, _ := json.Marshal(contract.NewSuccessEnvelope("request", map[string]any{"value": "<>& 日本\u2028\u2029"}, []contract.PublicFileDescriptor{}))
	result := agentCallResult{Payload: payload, Status: 200, RequestID: "request"}
	tool, failure := s.renderMCPFiles(context.Background(), id, APIKey{}, "test", mcpProtocolVersion20251125, result)
	if failure != nil {
		t.Fatal(failure)
	}
	recorder := httptest.NewRecorder()
	writeMCP(recorder, id, tool, nil)
	exact := int64(recorder.Body.Len())
	output.InlineBytes = exact
	if _, failure := s.renderMCPFiles(context.Background(), id, APIKey{}, "test", mcpProtocolVersion20251125, result); failure != nil {
		t.Fatal("exact encoded boundary", failure)
	}
	output.InlineBytes = exact - 1
	if _, failure := s.renderMCPFiles(context.Background(), id, APIKey{}, "test", mcpProtocolVersion20251125, result); failure == nil || failure.Code != "LIMIT_EXCEEDED" {
		t.Fatal("encoded envelope exceeded budget", failure)
	}
}
