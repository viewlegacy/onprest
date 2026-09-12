package gateway

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/viewlegacy/onprest/internal/agent"
	"github.com/viewlegacy/onprest/internal/protocol"
	"github.com/viewlegacy/onprest/internal/ws"
)

func TestAgentDisconnectImmediatelyReleasesInflightRequest(t *testing.T) {
	s, _, _ := testServer(t)
	conn := newControlledAgentConn()
	ac := newAgentConn(conn)
	s.agent = ac
	s.responseKinds = map[string]responseKind{"old": responseKindMutation}
	go s.writeAgent(ac)
	readDone := make(chan struct{})
	go func() {
		s.readAgent(ac)
		close(readDone)
	}()

	resultCh := make(chan agentCallResult, 1)
	go func() { resultCh <- s.callAgent(context.Background(), "slow", nil) }()
	select {
	case <-conn.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("agent request was not written")
	}
	start := time.Now()
	_ = conn.Close()
	select {
	case result := <-resultCh:
		if result.Status != http.StatusServiceUnavailable || result.Code != errGatewayAgentOffline {
			t.Fatalf("disconnect result = %#v", result)
		}
		if time.Since(start) > 500*time.Millisecond {
			t.Fatalf("disconnect release took %s", time.Since(start))
		}
	case <-time.After(time.Second):
		t.Fatal("in-flight request was not released on disconnect")
	}
	<-readDone
	s.agentMu.RLock()
	kindCount := len(s.responseKinds)
	s.agentMu.RUnlock()
	if kindCount != 0 {
		t.Fatalf("response kind cache survived agent disconnect: %v", s.responseKinds)
	}
}

func TestBlockedAgentWriteDoesNotHoldPendingLockOrLeakEntries(t *testing.T) {
	s, _, _ := testServer(t)
	s.cfg.AgentTimeout = 40 * time.Millisecond
	conn := newControlledAgentConn()
	conn.blockWrites = make(chan struct{})
	ac := newAgentConn(conn)
	s.agent = ac
	go s.writeAgent(ac)

	results := make(chan agentCallResult, 2)
	go func() { results <- s.callAgent(context.Background(), "first", nil) }()
	select {
	case <-conn.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("first write did not start")
	}
	go func() { results <- s.callAgent(context.Background(), "second", nil) }()
	for i := 0; i < 2; i++ {
		select {
		case result := <-results:
			if result.Status != http.StatusGatewayTimeout {
				t.Fatalf("blocked write result = %#v", result)
			}
		case <-time.After(time.Second):
			t.Fatal("callAgent remained blocked behind network write")
		}
	}
	ac.mu.Lock()
	pending := len(ac.pending)
	sendStates := len(ac.sendStates)
	ac.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending entries after write timeout = %d", pending)
	}
	if sendStates != 1 {
		t.Fatalf("send state entries while the first frame is still writing = %d, want 1", sendStates)
	}
	close(conn.blockWrites)
	waitForCondition(t, time.Second, func() bool {
		ac.mu.Lock()
		defer ac.mu.Unlock()
		return len(ac.sendStates) == 0
	}, "send state cleanup after request/cancel writes")
	conn.writesMu.Lock()
	writes := conn.writes
	conn.writesMu.Unlock()
	if writes != 2 {
		t.Fatalf("network writes = %d, want request followed by cancel; queued request must be discarded", writes)
	}
	_ = conn.Close()
	ac.disconnectPending()
}

func TestWriteFailureRemovesPendingEntry(t *testing.T) {
	s, _, _ := testServer(t)
	conn := &errorWriteAgentConn{err: errors.New("broken pipe")}
	ac := newAgentConn(conn)
	s.agent = ac
	go s.writeAgent(ac)
	result := s.callAgent(context.Background(), "capability", nil)
	if result.Status != http.StatusServiceUnavailable {
		t.Fatalf("result = %#v", result)
	}
	ac.mu.Lock()
	pending := len(ac.pending)
	sendStates := len(ac.sendStates)
	ac.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending entries after failed write = %d", pending)
	}
	if sendStates != 0 {
		t.Fatalf("send state entries after failed write = %d", sendStates)
	}
	ac.disconnectPending()
}

func TestSentRequestTimeoutWritesSameIDCancelAndCleansState(t *testing.T) {
	s, _, _ := testServer(t)
	s.cfg.AgentTimeout = 30 * time.Millisecond
	conn := newControlledAgentConn()
	ac := newAgentConn(conn)
	s.agent = ac
	go s.writeAgent(ac)

	result := s.callAgent(context.Background(), "mutate", nil)
	if result.Status != http.StatusGatewayTimeout {
		t.Fatalf("result=%+v", result)
	}
	waitForCondition(t, time.Second, func() bool {
		conn.writesMu.Lock()
		defer conn.writesMu.Unlock()
		return len(conn.payloads) == 2
	}, "request and cancel frames")
	conn.writesMu.Lock()
	payloads := append([][]byte(nil), conn.payloads...)
	conn.writesMu.Unlock()
	var request protocol.Request
	if err := json.Unmarshal(payloads[0], &request); err != nil {
		t.Fatal(err)
	}
	var cancelRequest protocol.CancelRequest
	if err := json.Unmarshal(payloads[1], &cancelRequest); err != nil {
		t.Fatal(err)
	}
	if request.ID == "" || cancelRequest.Type != "cancel" || cancelRequest.ID != request.ID {
		t.Fatalf("request=%s cancel=%s", payloads[0], payloads[1])
	}
	waitForCondition(t, time.Second, func() bool {
		ac.mu.Lock()
		defer ac.mu.Unlock()
		return len(ac.pending) == 0 && len(ac.sendStates) == 0
	}, "sent request terminal cleanup")
	ac.disconnectPending()
}

func TestConcurrentTimeoutCleanupLeavesNoPendingOrSendState(t *testing.T) {
	s, _, _ := testServer(t)
	s.cfg.AgentTimeout = 20 * time.Millisecond
	conn := newControlledAgentConn()
	ac := newAgentConn(conn)
	s.agent = ac
	go s.writeAgent(ac)

	const calls = 64
	results := make(chan agentCallResult, calls)
	for range calls {
		go func() { results <- s.callAgent(context.Background(), "mutate", nil) }()
	}
	for range calls {
		if result := <-results; result.Status != http.StatusGatewayTimeout {
			t.Fatalf("result=%+v", result)
		}
	}
	waitForCondition(t, 2*time.Second, func() bool {
		ac.mu.Lock()
		defer ac.mu.Unlock()
		return len(ac.pending) == 0 && len(ac.sendStates) == 0
	}, "concurrent timeout cleanup")
	ac.disconnectPending()
}

func TestMetaFetchRetriesCurrentConnectionUntilSuccess(t *testing.T) {
	attempts := 0
	s, _, _, cleanup := testServerWithAgent(t, func(req agentRequest) agentResponse {
		attempts++
		if attempts < 3 {
			return agentResponse{ID: req.ID, Error: &wireError{Code: errAgentInternal, Message: "temporary"}}
		}
		return agentResponse{ID: req.ID, Result: json.RawMessage(`{"data":{"openapi":"3.1.0","paths":{}},"response_kinds":{"hidden":"mutation","read":"select"}}`)}
	})
	defer cleanup()
	s.fetchMeta()
	if attempts != 3 {
		t.Fatalf("meta attempts = %d, want 3", attempts)
	}
	s.agentMu.RLock()
	cached := s.openapi != nil
	kinds := s.responseKinds
	s.agentMu.RUnlock()
	if !cached {
		t.Fatal("metadata was not cached after retry success")
	}
	if kinds["hidden"] != responseKindMutation || kinds["read"] != responseKindSelect {
		t.Fatalf("response kind cache=%v", kinds)
	}
}

func TestOpenAPIAIMetadataRefreshesAcrossCacheAndSnapshot(t *testing.T) {
	version := 0
	metadata := func() []byte {
		readOnly, destructive := true, false
		customerID := 1
		if version == 1 {
			readOnly, destructive = false, true
			customerID = 2
		}
		op := map[string]any{
			"x-onprest-capability": "get_customer",
			"description":          "Public customer lookup",
			"x-onprest-annotations": map[string]any{
				"read_only":   readOnly,
				"destructive": destructive,
			},
			"x-onprest-examples": []any{map[string]any{"params": map[string]any{"customer_id": customerID}}},
		}
		doc := map[string]any{
			"openapi": "3.1.0",
			"paths": map[string]any{
				"/api/v1/capabilities/get_customer": map[string]any{"post": op},
			},
		}
		body, err := json.Marshal(map[string]any{"data": doc})
		if err != nil {
			t.Fatalf("marshal metadata fixture: %v", err)
		}
		return body
	}
	s, logs, apiKey, cleanup := testServerWithAgent(t, func(req agentRequest) agentResponse {
		return agentResponse{ID: req.ID, Result: json.RawMessage(metadata())}
	})
	defer cleanup()
	s.cfg.EmitOpenAPISnapshot = true

	for _, want := range []struct {
		version     int
		readOnly    bool
		destructive bool
		customerID  int
	}{
		{version: 0, readOnly: true, destructive: false, customerID: 1},
		{version: 1, readOnly: false, destructive: true, customerID: 2},
	} {
		version = want.version
		s.fetchMeta()
		s.agentMu.RLock()
		cached := cloneMap(s.openapi)
		s.agentMu.RUnlock()
		post := cached["paths"].(map[string]any)["/api/v1/capabilities/get_customer"].(map[string]any)["post"].(map[string]any)
		annotations := post["x-onprest-annotations"].(map[string]any)
		if annotations["read_only"] != want.readOnly || annotations["destructive"] != want.destructive {
			t.Fatalf("cached annotations=%#v, want read_only=%t destructive=%t", annotations, want.readOnly, want.destructive)
		}
		example := post["x-onprest-examples"].([]any)[0].(map[string]any)["params"].(map[string]any)
		if example["customer_id"] != json.Number(strconv.Itoa(want.customerID)) {
			t.Fatalf("cached example=%#v, want customer_id=%d", example, want.customerID)
		}
	}

	req := newMCPCompatibilityRequest(http.MethodPost, "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/list\"}")
	rec := serveMCPCompatibility(s, apiKey, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("tools/list status=%d body=%s", rec.Code, rec.Body.String())
	}
	result := decodeMCPCompatibilityResponse(t, rec)["result"].(map[string]any)
	tool := mcpCompatibilityTool(t, result, "get_customer")
	hints := tool["annotations"].(map[string]any)
	if hints["readOnlyHint"] != false || hints["destructiveHint"] != true {
		t.Fatalf("latest MCP hints=%#v", hints)
	}
	if !strings.Contains(logs.String(), "\"customer_id\":2") || !strings.Contains(logs.String(), "\"x-onprest-annotations\"") {
		t.Fatalf("snapshot log does not contain refreshed public metadata: %s", logs.String())
	}
}

func TestOpenAPIMetadataIgnoresStaleAgentGenerationAndFiltersSameFixture(t *testing.T) {
	const (
		oldPublicDescription = "OLD_PUBLIC_METADATA_SENTINEL"
		newPublicDescription = "NEW_PUBLIC_METADATA_SENTINEL"
		hiddenName           = "hidden_customer"
		hiddenDescription    = "HIDDEN_DESCRIPTION_SENTINEL"
		hiddenExample        = "HIDDEN_EXAMPLE_SENTINEL"
	)
	metadata := func(description string, customerID int) []byte {
		readOnly, destructive, idempotent, openWorld := true, false, true, false
		public := agent.CapabilityDef{
			Description: description,
			Policy:      agent.PolicyDef{ExposeInOpenAPI: boolPtr(true)},
			Annotations: &agent.CapabilityAnnotations{
				ReadOnly: &readOnly, Destructive: &destructive,
				Idempotent: &idempotent, OpenWorld: &openWorld,
			},
			Examples: []agent.CapabilityExample{{Params: map[string]any{"customer_id": customerID}}},
		}
		hidden := agent.CapabilityDef{
			Description: hiddenDescription,
			Policy:      agent.PolicyDef{ExposeInOpenAPI: boolPtr(false)},
			Examples:    []agent.CapabilityExample{{Params: map[string]any{"note": hiddenExample}}},
		}
		doc := agent.BuildOpenAPI(&agent.CapabilityFile{
			Service:      agent.ServiceDef{Title: "Generation fixture"},
			Capabilities: map[string]agent.CapabilityDef{"get_customer": public, hiddenName: hidden},
		})
		body, err := json.Marshal(map[string]any{"data": doc})
		if err != nil {
			t.Fatalf("marshal generation metadata fixture: %v", err)
		}
		return body
	}

	s, logs, apiKey := testServer(t)
	s.cfg.EmitOpenAPISnapshot = true
	oldRelease := make(chan struct{})
	var releaseOldOnce sync.Once
	releaseOld := func() { releaseOldOnce.Do(func() { close(oldRelease) }) }
	oldConn := &generationMetadataAgentConn{response: metadata(oldPublicDescription, 1), started: make(chan struct{}), release: oldRelease}
	oldAC := newAgentConn(oldConn)
	oldConn.ac = oldAC
	var newAC *agentConn
	t.Cleanup(func() {
		releaseOld()
		oldAC.disconnectPending()
		if newAC != nil {
			newAC.disconnectPending()
		}
	})
	s.agentMu.Lock()
	s.agent = oldAC
	s.agentMu.Unlock()
	go s.writeAgent(oldAC)
	oldDone := make(chan struct{})
	go func() {
		s.fetchMetaFor(oldAC)
		close(oldDone)
	}()
	select {
	case <-oldConn.started:
	case <-time.After(time.Second):
		t.Fatal("old agent generation did not start metadata request")
	}

	newConn := &generationMetadataAgentConn{response: metadata(newPublicDescription, 2), started: make(chan struct{})}
	newAC = newAgentConn(newConn)
	newConn.ac = newAC
	s.agentMu.Lock()
	s.agent = newAC
	s.agentMu.Unlock()
	go s.writeAgent(newAC)
	s.fetchMetaFor(newAC)
	releaseOld()
	select {
	case <-oldDone:
	case <-time.After(time.Second):
		t.Fatal("stale agent generation did not finish")
	}

	s.agentMu.RLock()
	cached := cloneMap(s.openapi)
	s.agentMu.RUnlock()
	if cached == nil {
		t.Fatal("new agent metadata was not cached")
	}
	openAPIJSON, err := json.Marshal(cached)
	if err != nil {
		t.Fatal(err)
	}
	assertGenerationMetadataVisibility(t, string(openAPIJSON), newPublicDescription, oldPublicDescription, hiddenName, hiddenDescription, hiddenExample)

	openAPIRequest := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	openAPIRequest.Header.Set("Authorization", "Bearer "+apiKey)
	openAPIResponse := httptest.NewRecorder()
	s.httpSrv.Handler.ServeHTTP(openAPIResponse, openAPIRequest)
	if openAPIResponse.Code != http.StatusOK {
		t.Fatalf("filtered OpenAPI status=%d body=%s", openAPIResponse.Code, openAPIResponse.Body.String())
	}
	assertGenerationMetadataVisibility(t, openAPIResponse.Body.String(), newPublicDescription, oldPublicDescription, hiddenName, hiddenDescription, hiddenExample)

	mcpRequest := newMCPCompatibilityRequest(http.MethodPost, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	mcpRequest.Header.Set(mcpProtocolHeader, mcpProtocolVersion20251125)
	mcpResponse := serveMCPCompatibility(s, apiKey, mcpRequest)
	if mcpResponse.Code != http.StatusOK {
		t.Fatalf("filtered tools/list status=%d body=%s", mcpResponse.Code, mcpResponse.Body.String())
	}
	mcpBody := decodeMCPCompatibilityResponse(t, mcpResponse)
	tool := mcpCompatibilityTool(t, mcpBody["result"].(map[string]any), "get_customer")
	if tool["description"] != newPublicDescription {
		t.Fatalf("tools/list description=%#v, want new generation", tool["description"])
	}
	assertGenerationMetadataVisibility(t, mcpResponse.Body.String(), newPublicDescription, oldPublicDescription, hiddenName, hiddenDescription, hiddenExample)
	if _, ok := tool["examples"]; ok {
		t.Fatalf("MCP tool copied OpenAPI examples: %#v", tool)
	}

	logText := logs.String()
	assertGenerationMetadataVisibility(t, logText, newPublicDescription, oldPublicDescription, hiddenName, hiddenDescription, hiddenExample)
	if !strings.Contains(logText, `"customer_id":2`) || strings.Contains(logText, `"customer_id":1`) {
		t.Fatalf("snapshot log contains stale/current example mismatch: %s", logText)
	}

	oldAC.disconnectPending()
	newAC.disconnectPending()
}

func TestOpenAPIMetadataPreservesInt64ExamplesAndConstraintsAcrossRoutes(t *testing.T) {
	const (
		boundaryValue = int64(9007199254740993)
		maximumValue  = int64(9223372036854775807)
	)
	capability := agent.CapabilityDef{
		Name:        "precision_lookup",
		Description: "Lossless integer metadata fixture",
		SQL:         "select :boundary as boundary, :maximum as maximum",
		Params: map[string]agent.ParamDef{
			"boundary": {Type: "integer", Required: true, Minimum: int64Ptr(boundaryValue), Maximum: int64Ptr(maximumValue)},
			"maximum":  {Type: "integer", Required: true, Minimum: int64Ptr(boundaryValue), Maximum: int64Ptr(maximumValue)},
		},
		Result: agent.ResultDef{
			"boundary": {Type: "integer"},
			"maximum":  {Type: "integer"},
		},
		Examples: []agent.CapabilityExample{{Params: map[string]any{
			"boundary": boundaryValue,
			"maximum":  maximumValue,
		}}},
		Policy: agent.PolicyDef{ExposeInOpenAPI: boolPtr(true)},
	}
	cf := &agent.CapabilityFile{
		Service: agent.ServiceDef{Title: "Lossless metadata fixture", Version: "1.2.5"},
		Gateway: agent.GatewayDef{URL: "ws://127.0.0.1:8080/ws/agent", AgentPrivateKey: testAgentPrivateKey},
		Database: agent.DatabaseDef{
			Driver: "postgres", Host: "localhost", Port: 5432, Name: "fixture", User: "fixture", Password: "fixture",
		},
		Capabilities: map[string]agent.CapabilityDef{"precision_lookup": capability},
	}
	if err := cf.Lint(); err != nil {
		t.Fatalf("lint precision metadata fixture: %v", err)
	}
	doc := agent.BuildOpenAPI(cf)
	agentJSON, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	assertLosslessPrecisionJSON(t, string(agentJSON), "Agent OpenAPI")
	metadata, err := json.Marshal(map[string]any{
		"data":           doc,
		"response_kinds": map[string]string{"precision_lookup": "select"},
	})
	if err != nil {
		t.Fatal(err)
	}
	s, logs, apiKey, cleanup := testServerWithAgent(t, func(req agentRequest) agentResponse {
		if req.Capability != "meta" {
			t.Fatalf("metadata request=%#v", req)
		}
		return agentResponse{ID: req.ID, Result: json.RawMessage(metadata)}
	})
	defer cleanup()
	s.cfg.APIKeys[0].Capabilities = capabilities{"precision_lookup"}
	s.cfg.EmitOpenAPISnapshot = true
	s.fetchMeta()

	s.agentMu.RLock()
	cached := cloneMap(s.openapi)
	s.agentMu.RUnlock()
	if cached == nil {
		t.Fatal("metadata was not cached")
	}
	assertLosslessPrecisionDocument(t, cached, "Gateway cache")

	request := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	request.Header.Set("Authorization", "Bearer "+apiKey)
	response := httptest.NewRecorder()
	s.httpSrv.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("OpenAPI status=%d body=%s", response.Code, response.Body.String())
	}
	assertLosslessPrecisionJSON(t, response.Body.String(), "Gateway /openapi.json")

	logText := logs.String()
	if !strings.Contains(logText, "openapi_snapshot") {
		t.Fatalf("metadata snapshot log missing: %s", logText)
	}
	assertLosslessPrecisionJSON(t, logText, "Gateway snapshot")
}

func assertLosslessPrecisionDocument(t *testing.T, doc map[string]any, source string) {
	t.Helper()
	paths, ok := doc["paths"].(map[string]any)
	if !ok {
		t.Fatalf("%s paths=%#v", source, doc["paths"])
	}
	post := paths["/api/v1/capabilities/precision_lookup"].(map[string]any)["post"].(map[string]any)
	requestBody := post["requestBody"].(map[string]any)
	requestSchema := requestBody["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
	params := requestSchema["properties"].(map[string]any)
	for _, name := range []string{"boundary", "maximum"} {
		param := params[name].(map[string]any)
		if got := param["minimum"]; got != json.Number("9007199254740993") {
			t.Fatalf("%s %s.minimum=%#v (%T), want lossless json.Number", source, name, got, got)
		}
		if got := param["maximum"]; got != json.Number("9223372036854775807") {
			t.Fatalf("%s %s.maximum=%#v (%T), want lossless json.Number", source, name, got, got)
		}
	}
	examples := post["x-onprest-examples"].([]any)
	exampleParams := examples[0].(map[string]any)["params"].(map[string]any)
	if got := exampleParams["boundary"]; got != json.Number("9007199254740993") {
		t.Fatalf("%s boundary example=%#v (%T), want lossless json.Number", source, got, got)
	}
	if got := exampleParams["maximum"]; got != json.Number("9223372036854775807") {
		t.Fatalf("%s maximum example=%#v (%T), want lossless json.Number", source, got, got)
	}
}

func assertLosslessPrecisionJSON(t *testing.T, body, source string) {
	t.Helper()
	for _, want := range []string{
		`"minimum":9007199254740993`,
		`"maximum":9223372036854775807`,
		`"boundary":9007199254740993`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("%s missing exact decimal %s: %s", source, want, body)
		}
	}
	if strings.Contains(body, "9007199254740992") {
		t.Fatalf("%s rounded 2^53+1 through float64: %s", source, body)
	}
}

func boolPtr(value bool) *bool { return &value }

func assertGenerationMetadataVisibility(t *testing.T, body, want, old, hiddenName, hiddenDescription, hiddenExample string) {
	t.Helper()
	if !strings.Contains(body, want) {
		t.Fatalf("metadata missing current generation %q: %s", want, body)
	}
	for _, forbidden := range []string{old, hiddenName, hiddenDescription, hiddenExample} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("metadata leaked %q: %s", forbidden, body)
		}
	}
}

type generationMetadataAgentConn struct {
	ac       *agentConn
	response []byte
	started  chan struct{}
	release  <-chan struct{}
	once     sync.Once
}

func (c *generationMetadataAgentConn) ReadText() ([]byte, error) { return nil, io.EOF }

func (c *generationMetadataAgentConn) WriteText(payload []byte) error {
	var req agentRequest
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if err := decoder.Decode(&req); err != nil {
		return err
	}
	c.once.Do(func() { close(c.started) })
	if c.release != nil {
		<-c.release
	}
	resp := agentResponse{ID: req.ID, Result: json.RawMessage(c.response)}
	c.ac.mu.Lock()
	ch := c.ac.pending[resp.ID]
	delete(c.ac.pending, resp.ID)
	c.ac.mu.Unlock()
	if ch != nil {
		ch <- resp
	}
	return nil
}

func (c *generationMetadataAgentConn) WriteTextWithDeadline(payload []byte, _ time.Duration) error {
	return c.WriteText(payload)
}

func (c *generationMetadataAgentConn) Close() error { return nil }

func TestAPIKeySuccessCacheIsBoundedAndStoresOnlyDigests(t *testing.T) {
	s, _, apiKey := testServer(t)
	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	if _, status := s.authenticate(httptest.NewRecorder(), req); status != 0 {
		t.Fatal("valid key rejected")
	}
	if len(s.apiKeyCache) != 1 {
		t.Fatalf("cache size = %d, want 1", len(s.apiKeyCache))
	}
	for i := 0; i < maxAPIKeyCacheEntries+10; i++ {
		digest := [32]byte{byte(i), byte(i >> 8), byte(i >> 16)}
		s.cacheAPIKey(digest, s.cfg.APIKeys[0])
	}
	if len(s.apiKeyCache) != maxAPIKeyCacheEntries {
		t.Fatalf("bounded cache size = %d, want %d", len(s.apiKeyCache), maxAPIKeyCacheEntries)
	}
}

func TestRateLimitBucketCleanupAndHardCap(t *testing.T) {
	s := NewServer(Config{RateLimit: RateLimitConfig{RequestsPerSecond: 1, Burst: 1}}, nil)
	old := time.Now().Add(-time.Hour)
	for i := 0; i < maxRateLimitBuckets; i++ {
		s.rate[fmt.Sprintf("2001:db8::%x", i)] = &bucket{tokens: 0, last: old}
	}
	s.rateLastCleanup = time.Time{}
	if !s.take("203.0.113.1") {
		t.Fatal("new client was rejected after cleanup")
	}
	if len(s.rate) != 1 {
		t.Fatalf("expired buckets remaining = %d, want 1", len(s.rate))
	}
	for i := 0; i < maxRateLimitBuckets+50; i++ {
		s.take(fmt.Sprintf("198.51.%d.%d", (i/256)%256, i%256))
	}
	if len(s.rate) > maxRateLimitBuckets {
		t.Fatalf("rate bucket map exceeded cap: %d", len(s.rate))
	}
}

func TestInvalidSignatureDoesNotConsumeNonceAndSignatureIsBoundToHandshakeKey(t *testing.T) {
	s := NewServer(Config{AgentPublicKey: testAgentPublicKey}, nil)
	nonce := "nonce-not-consumed"
	good := signedAgentRequest(t, s, "/ws/agent", time.Now().UTC(), nonce)
	bad := good.Clone(good.Context())
	bad.Header = good.Header.Clone()
	bad.Header.Set("X-Agent-Signature", base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)))
	if s.authenticateAgent(bad) {
		t.Fatal("invalid signature authenticated")
	}
	if !s.authenticateAgent(good) {
		t.Fatal("valid signature rejected after invalid attempt reused nonce")
	}

	bound := signedAgentRequest(t, s, "/ws/agent", time.Now().UTC(), "nonce-key-binding")
	bound.Header.Set("Sec-WebSocket-Key", "MDEyMzQ1Njc4OWFiY2RlZg==")
	if s.authenticateAgent(bound) {
		t.Fatal("signature authenticated with a different handshake key")
	}
}

func TestCapturedHandshakeIsRejectedAfterGatewayRestart(t *testing.T) {
	first := NewServer(Config{AgentPublicKey: testAgentPublicKey}, nil)
	captured := signedAgentRequest(t, first, "/ws/agent", time.Now().UTC(), "captured-handshake")
	if !first.authenticateAgent(captured.Clone(captured.Context())) {
		t.Fatal("original handshake did not authenticate")
	}
	if first.authenticateAgent(captured.Clone(captured.Context())) {
		t.Fatal("captured handshake authenticated twice in the issuing gateway")
	}
	second := NewServer(Config{AgentPublicKey: testAgentPublicKey}, nil)
	if second.authenticateAgent(captured.Clone(captured.Context())) {
		t.Fatal("captured handshake authenticated after gateway restart")
	}
}

func TestExpiredAgentChallengeIsRejected(t *testing.T) {
	s := NewServer(Config{AgentPublicKey: testAgentPublicKey}, nil)
	req := signedAgentRequest(t, s, "/ws/agent", time.Now().UTC(), "expired-challenge")
	challenge := req.Header.Get("X-Agent-Challenge")
	s.authMu.Lock()
	s.agentChallenges[challenge] = time.Now().Add(-agentChallengeTTL - time.Second)
	s.authMu.Unlock()
	if s.authenticateAgent(req) {
		t.Fatal("expired agent challenge authenticated")
	}
	s.authMu.Lock()
	_, retained := s.agentChallenges[challenge]
	s.authMu.Unlock()
	if retained {
		t.Fatal("expired agent challenge was not removed")
	}
}

func TestAgentChallengeEndpointIsOneTimeBoundedAndNotCacheable(t *testing.T) {
	s := NewServer(Config{AgentPublicKey: testAgentPublicKey}, nil)
	get := httptest.NewRequest(http.MethodGet, "/ws/agent/challenge", nil)
	getRec := httptest.NewRecorder()
	s.httpSrv.Handler.ServeHTTP(getRec, get)
	if getRec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET challenge status = %d", getRec.Code)
	}
	for i := 0; i < maxAgentChallenges+1; i++ {
		req := httptest.NewRequest(http.MethodPost, "/ws/agent/challenge", nil)
		rec := httptest.NewRecorder()
		s.httpSrv.Handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("POST challenge status=%d cache=%q body=%s", rec.Code, rec.Header().Get("Cache-Control"), rec.Body.String())
		}
	}
	s.authMu.Lock()
	count := len(s.agentChallenges)
	s.authMu.Unlock()
	if count != maxAgentChallenges {
		t.Fatalf("challenge store size = %d, want %d", count, maxAgentChallenges)
	}
}

func TestSilentWebSocketClientExpiresAndCanReconnect(t *testing.T) {
	s, _, _ := testServer(t)
	s.cfg.AgentPingInterval = 20 * time.Millisecond
	s.cfg.AgentPongTimeout = 20 * time.Millisecond
	s.cfg.AgentWriteTimeout = 20 * time.Millisecond
	httpServer := httptest.NewServer(s.httpSrv.Handler)
	defer httpServer.Close()
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/agent"

	first := dialSignedTestAgent(t, wsURL, "stale-agent")
	defer first.Close()
	waitForCondition(t, time.Second, func() bool { return s.hasAgent() }, "first agent connection")
	waitForCondition(t, 2*time.Second, func() bool { return !s.hasAgent() }, "stale agent expiration")

	second := dialSignedTestAgent(t, wsURL, "replacement-agent")
	defer second.Close()
	waitForCondition(t, time.Second, func() bool { return s.hasAgent() }, "replacement agent connection")
}

func TestGatewayWriterAndKeepaliveUseAtomicDeadlineAPIs(t *testing.T) {
	const writeTimeout = 73 * time.Millisecond
	spy := newGatewayDeadlineSpy()
	s := NewServer(Config{
		AgentWriteTimeout: writeTimeout,
		AgentPingInterval: 5 * time.Millisecond,
		AgentPongTimeout:  20 * time.Millisecond,
	}, nil)
	ac := newAgentConn(spy)
	defer ac.disconnectPending()

	ac.mu.Lock()
	ac.sendStates["request-1"] = &requestSendState{phase: "queued"}
	ac.mu.Unlock()
	writerDone := make(chan struct{})
	go func() {
		s.writeAgent(ac)
		close(writerDone)
	}()
	writeResult := make(chan error, 1)
	ac.send <- agentWrite{id: "request-1", payload: []byte("request payload"), result: writeResult, ctx: t.Context()}
	select {
	case call := <-spy.textDeadlineCalls:
		if string(call.payload) != "request payload" || call.timeout != writeTimeout {
			t.Fatalf("text deadline call=%+v", call)
		}
	case <-time.After(time.Second):
		t.Fatal("gateway writer did not call WriteTextWithDeadline")
	}
	if err := <-writeResult; err != nil {
		t.Fatal(err)
	}

	keepaliveDone := make(chan struct{})
	go func() {
		s.keepAgentAlive(ac)
		close(keepaliveDone)
	}()
	select {
	case call := <-spy.pingDeadlineCalls:
		if string(call.payload) != "onprest-keepalive" || call.timeout != writeTimeout {
			t.Fatalf("ping deadline call=%+v", call)
		}
	case <-time.After(time.Second):
		t.Fatal("gateway keepalive did not call WritePingWithDeadline")
	}
	spy.mu.Lock()
	plainWrites := spy.plainWrites
	spy.mu.Unlock()
	if plainWrites != 0 {
		t.Fatalf("gateway regressed to plain WriteText: calls=%d", plainWrites)
	}
	ac.disconnectPending()
	for name, done := range map[string]<-chan struct{}{"writer": writerDone, "keepalive": keepaliveDone} {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("gateway %s goroutine did not terminate", name)
		}
	}
}

func TestConcurrentRequestsOnOneWebSocketDispatchOutOfOrderResponses(t *testing.T) {
	s, _, _ := testServer(t)
	httpServer := httptest.NewServer(s.httpSrv.Handler)
	defer httpServer.Close()
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/agent"
	conn := dialSignedTestAgent(t, wsURL, "concurrent-agent")
	defer conn.Close()

	agentErr := make(chan error, 1)
	go func() {
		var requests []protocol.Request
		for len(requests) < 2 {
			msg, err := conn.ReadText()
			if err != nil {
				agentErr <- err
				return
			}
			var req protocol.Request
			dec := json.NewDecoder(strings.NewReader(string(msg)))
			dec.UseNumber()
			if err := dec.Decode(&req); err != nil {
				agentErr <- err
				return
			}
			if req.Capability == "meta" {
				resp := protocol.ResultResponse(req.ID, map[string]any{"data": map[string]any{"openapi": "3.1.0", "paths": map[string]any{}}})
				if err := conn.WriteText(protocol.MustJSON(resp)); err != nil {
					agentErr <- err
					return
				}
				continue
			}
			requests = append(requests, req)
		}
		for i := len(requests) - 1; i >= 0; i-- {
			resp := protocol.ResultResponse(requests[i].ID, map[string]any{"capability": requests[i].Capability, "count": int64(1)})
			if err := conn.WriteText(protocol.MustJSON(resp)); err != nil {
				agentErr <- err
				return
			}
		}
		agentErr <- nil
	}()

	start := make(chan struct{})
	results := make(chan agentCallResult, 2)
	for _, capability := range []string{"slow", "fast"} {
		capability := capability
		go func() {
			<-start
			results <- s.callAgent(context.Background(), capability, nil)
		}()
	}
	close(start)
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case result := <-results:
			if !result.OK() {
				t.Fatalf("call result = %#v", result)
			}
			var body struct {
				Capability string `json:"capability"`
			}
			if err := json.Unmarshal(result.Payload, &body); err != nil {
				t.Fatal(err)
			}
			seen[body.Capability] = true
		case <-time.After(time.Second):
			t.Fatal("concurrent call timed out")
		}
	}
	if !seen["slow"] || !seen["fast"] {
		t.Fatalf("responses = %v", seen)
	}
	if err := <-agentErr; err != nil {
		t.Fatal(err)
	}
	s.agentMu.RLock()
	ac := s.agent
	s.agentMu.RUnlock()
	waitForCondition(t, time.Second, func() bool {
		ac.mu.Lock()
		defer ac.mu.Unlock()
		return len(ac.pending) == 0 && len(ac.sendStates) == 0
	}, "pending/send-state cleanup after out-of-order responses")
}

func TestHTTPBodyReadDeadlineRejectsSlowBody(t *testing.T) {
	s, _, apiKey := testServer(t)
	s.cfg.BodyReadTimeout = 150 * time.Millisecond
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- s.httpSrv.Serve(listener) }()
	defer func() {
		_ = s.httpSrv.Shutdown(context.Background())
		<-serveDone
	}()
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	_, err = fmt.Fprintf(conn, "POST /api/v1/capabilities/get_customer HTTP/1.1\r\nHost: test\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{", apiKey)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("slow body status=%d body=%s", resp.StatusCode, body)
	}
	if s.httpSrv.IdleTimeout <= 0 {
		t.Fatal("HTTP IdleTimeout is not configured")
	}
}

func dialSignedTestAgent(t *testing.T, rawURL, nonce string) *ws.Conn {
	t.Helper()
	key, err := ws.NewHandshakeKey()
	if err != nil {
		t.Fatal(err)
	}
	privateKey, err := base64.RawURLEncoding.DecodeString(testAgentPrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	timestamp := time.Now().UTC().Format(time.RFC3339)
	path := "/ws/agent"
	challenge := fetchTestAgentChallenge(t, rawURL)
	signature := ed25519.Sign(ed25519.PrivateKey(privateKey), protocol.AgentAuthMessage(path, timestamp, nonce, challenge, key))
	headers := http.Header{}
	headers.Set("Sec-WebSocket-Key", key)
	headers.Set("X-Agent-Timestamp", timestamp)
	headers.Set("X-Agent-Nonce", nonce)
	headers.Set("X-Agent-Challenge", challenge)
	headers.Set("X-Agent-Signature", base64.RawURLEncoding.EncodeToString(signature))
	conn, err := ws.Dial(time.Second, rawURL, headers)
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

func fetchTestAgentChallenge(t *testing.T, rawURL string) string {
	t.Helper()
	challengeURL := "http" + strings.TrimPrefix(strings.TrimSuffix(rawURL, "/ws/agent"), "ws") + "/ws/agent/challenge"
	resp, err := http.Post(challengeURL, "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("challenge status = %s", resp.Status)
	}
	var body struct {
		Challenge string `json:"challenge"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.Challenge == "" {
		t.Fatalf("decode challenge: %v", err)
	}
	return body.Challenge
}

func waitForCondition(t *testing.T, timeout time.Duration, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", description)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type controlledAgentConn struct {
	writeStarted chan struct{}
	disconnect   chan struct{}
	blockWrites  chan struct{}
	writeOnce    sync.Once
	closeOnce    sync.Once
	writesMu     sync.Mutex
	writes       int
	payloads     [][]byte
}

type gatewayDeadlineCall struct {
	payload []byte
	timeout time.Duration
}

type gatewayDeadlineSpy struct {
	mu                sync.Mutex
	plainWrites       int
	textDeadlineCalls chan gatewayDeadlineCall
	pingDeadlineCalls chan gatewayDeadlineCall
	pongHandler       func()
}

func newGatewayDeadlineSpy() *gatewayDeadlineSpy {
	return &gatewayDeadlineSpy{
		textDeadlineCalls: make(chan gatewayDeadlineCall, 1),
		pingDeadlineCalls: make(chan gatewayDeadlineCall, 1),
	}
}

func (c *gatewayDeadlineSpy) ReadText() ([]byte, error) { return nil, io.EOF }
func (c *gatewayDeadlineSpy) WriteText([]byte) error {
	c.mu.Lock()
	c.plainWrites++
	c.mu.Unlock()
	return errors.New("plain WriteText must not be used by the gateway writer")
}
func (c *gatewayDeadlineSpy) WriteTextWithDeadline(payload []byte, timeout time.Duration) error {
	c.textDeadlineCalls <- gatewayDeadlineCall{payload: append([]byte(nil), payload...), timeout: timeout}
	return nil
}
func (c *gatewayDeadlineSpy) WritePingWithDeadline(payload []byte, timeout time.Duration) error {
	select {
	case c.pingDeadlineCalls <- gatewayDeadlineCall{payload: append([]byte(nil), payload...), timeout: timeout}:
	default:
	}
	return nil
}
func (c *gatewayDeadlineSpy) SetReadDeadline(time.Time) error { return nil }
func (c *gatewayDeadlineSpy) SetPongHandler(handler func()) {
	c.mu.Lock()
	c.pongHandler = handler
	c.mu.Unlock()
}
func (c *gatewayDeadlineSpy) Close() error { return nil }

func newControlledAgentConn() *controlledAgentConn {
	return &controlledAgentConn{writeStarted: make(chan struct{}), disconnect: make(chan struct{})}
}

func (c *controlledAgentConn) ReadText() ([]byte, error) { <-c.disconnect; return nil, io.EOF }
func (c *controlledAgentConn) WriteText(payload []byte) error {
	c.writesMu.Lock()
	c.writes++
	c.payloads = append(c.payloads, append([]byte(nil), payload...))
	c.writesMu.Unlock()
	c.writeOnce.Do(func() { close(c.writeStarted) })
	if c.blockWrites != nil {
		<-c.blockWrites
	}
	return nil
}
func (c *controlledAgentConn) WriteTextWithDeadline(payload []byte, _ time.Duration) error {
	return c.WriteText(payload)
}
func (c *controlledAgentConn) Close() error {
	c.closeOnce.Do(func() { close(c.disconnect) })
	return nil
}

type errorWriteAgentConn struct{ err error }

func (c *errorWriteAgentConn) ReadText() ([]byte, error) { return nil, io.EOF }
func (c *errorWriteAgentConn) WriteText([]byte) error    { return c.err }
func (c *errorWriteAgentConn) WriteTextWithDeadline([]byte, time.Duration) error {
	return c.err
}
func (c *errorWriteAgentConn) Close() error { return nil }
