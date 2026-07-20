package gateway

import (
	"bufio"
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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/viewlegacy/onprest/internal/protocol"
	"github.com/viewlegacy/onprest/internal/ws"
)

func TestAgentDisconnectImmediatelyReleasesInflightRequest(t *testing.T) {
	s, _, _ := testServer(t)
	conn := newControlledAgentConn()
	ac := newAgentConn(conn)
	s.agent = ac
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
	ac.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending entries after write timeout = %d", pending)
	}
	close(conn.blockWrites)
	time.Sleep(50 * time.Millisecond)
	conn.writesMu.Lock()
	writes := conn.writes
	conn.writesMu.Unlock()
	if writes != 1 {
		t.Fatalf("network writes = %d, want only the already-started write", writes)
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
	ac.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending entries after failed write = %d", pending)
	}
	ac.disconnectPending()
}

func TestMetaFetchRetriesCurrentConnectionUntilSuccess(t *testing.T) {
	attempts := 0
	s, _, _, cleanup := testServerWithAgent(t, func(req agentRequest) agentResponse {
		attempts++
		if attempts < 3 {
			return agentResponse{ID: req.ID, Error: &wireError{Code: errAgentInternal, Message: "temporary"}}
		}
		return agentResponse{ID: req.ID, Result: json.RawMessage(`{"data":{"openapi":"3.1.0","paths":{}}}`)}
	})
	defer cleanup()
	s.fetchMeta()
	if attempts != 3 {
		t.Fatalf("meta attempts = %d, want 3", attempts)
	}
	s.agentMu.RLock()
	cached := s.openapi != nil
	s.agentMu.RUnlock()
	if !cached {
		t.Fatal("metadata was not cached after retry success")
	}
}

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
			resp := protocol.ResultResponse(requests[i].ID, map[string]any{"capability": requests[i].Capability})
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
}

func newControlledAgentConn() *controlledAgentConn {
	return &controlledAgentConn{writeStarted: make(chan struct{}), disconnect: make(chan struct{})}
}

func (c *controlledAgentConn) ReadText() ([]byte, error) { <-c.disconnect; return nil, io.EOF }
func (c *controlledAgentConn) WriteText([]byte) error {
	c.writesMu.Lock()
	c.writes++
	c.writesMu.Unlock()
	c.writeOnce.Do(func() { close(c.writeStarted) })
	if c.blockWrites != nil {
		<-c.blockWrites
	}
	return nil
}
func (c *controlledAgentConn) Close() error {
	c.closeOnce.Do(func() { close(c.disconnect) })
	return nil
}

type errorWriteAgentConn struct{ err error }

func (c *errorWriteAgentConn) ReadText() ([]byte, error) { return nil, io.EOF }
func (c *errorWriteAgentConn) WriteText([]byte) error    { return c.err }
func (c *errorWriteAgentConn) Close() error              { return nil }
