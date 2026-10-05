package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/viewlegacy/onprest/internal/agent/artifacts"
	"github.com/viewlegacy/onprest/internal/agent/providers"
	"github.com/viewlegacy/onprest/internal/contract"
	"github.com/viewlegacy/onprest/internal/gateway"
	"github.com/viewlegacy/onprest/internal/protocol"
	"github.com/viewlegacy/onprest/internal/ws"
	"golang.org/x/crypto/bcrypt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func transportFixture(t *testing.T, url, extra string) (*Runner, *controllerFixture) {
	t.Helper()
	dir := t.TempDir()
	eight := extra == "eight-transfers"
	if eight {
		extra = ""
	}
	ttl := "3s"
	if extra == "long-ttl" {
		ttl = "10s"
		extra = ""
	}
	yaml := `schema_version: 1
service: {title: Transport fixture, version: test}
gateway: {url: ` + url + `, agent_private_key: ` + testAgentPrivateKey + `}
runtime:
  max_concurrent_requests: 2
  spool_dir: ` + filepath.Join(dir, "spool") + `
  identity_dir: ` + filepath.Join(dir, "identity") + `
  transfer_timeout: 2s
  control_limits:
    bytes_per_second: 2MiB
    burst_bytes: 256KiB
    write: {requests: 100, burst: 200, max_concurrent: 4}
    transfer: {requests: 100, burst: 200, max_concurrent: 4}
  input_limits: {max_pending_requests: 2, max_recent_requests: 4}
sources:
  http: {api: {base_url: https://example.internal, allowed_methods: [GET]}}
  databases: {db: {driver: postgres, host: localhost, port: 5432, name: test, user: test}}
capabilities:
  run:
    executor: {type: http, source: api, method: GET, path: /, response: {kind: file}}
    input:
      properties: {doc: {type: file}, documents: {items: {type: file}}, n: {type: integer, default: 7}}
      required: [n]
    output: {files: {}}
    policy: {timeout: 1s, file_ttl: ` + ttl + `, rate_limit: {requests: 10, per: 1s, burst: 10}}
  normal:
    executor: {type: db, source: db, sql: select 1}
    input: {properties: {n: {type: integer}}}
    output: {data: {}}
` + extra
	if eight {
		yaml = strings.Replace(yaml, "transfer: {requests: 100, burst: 200, max_concurrent: 4}", "transfer: {requests: 100, burst: 200, max_concurrent: 16}\n    max_concurrent_transfers: 100", 1)
	}
	cfg, e := contract.Compile([]byte(yaml))
	if e != nil {
		t.Fatal(e)
	}
	cf, e := capabilityRuntimeView(cfg, filepath.Join(dir, "agent.yaml"))
	if e != nil {
		t.Fatal(e)
	}
	p := &controllerFixture{fn: func(ctx context.Context, input map[string]any, effects *providers.Effects) (contract.ProviderResult, error) {
		if input["n"] != int64(7) {
			return contract.ProviderResult{}, fmt.Errorf("default missing")
		}
		effects.NoChange()
		var reader io.Reader = bytes.NewReader(nil)
		if value, ok := input["doc"].(contract.FileValue); ok {
			var e error
			reader, e = OpenInput(ctx, value)
			if e != nil {
				return contract.ProviderResult{}, e
			}
		}
		return contract.ProviderResult{Files: []contract.ProviderFile{{Name: "output.bin", MIMEType: "application/octet-stream", Reader: reader}}}, nil
	}}
	normal := &controllerFixture{fn: func(_ context.Context, input map[string]any, _ *providers.Effects) (contract.ProviderResult, error) {
		count := int64(1)
		n := any(int64(1))
		if value, ok := input["n"]; ok {
			n = value
		}
		return contract.ProviderResult{Data: []any{map[string]any{"n": n}}, Columns: []string{"n"}, ReturnedRows: &count}, nil
	}}
	registry, _ := providers.NewRegistry(map[string]providers.Factory{"http": func(context.Context, *providers.Snapshot) (providers.Provider, error) { return p, nil }, "db": func(context.Context, *providers.Snapshot) (providers.Provider, error) { return normal, nil }})
	prepared, e := registry.Prepare(context.Background(), cfg, cf.ConfigPath, providers.ResolveOptions{})
	if e != nil {
		t.Fatal(e)
	}
	ctl, e := NewExecutionController(prepared, nil, nil)
	if e != nil {
		t.Fatal(e)
	}
	r := &Runner{cfg: Config{CapabilityFile: cf.ConfigPath, ReconnectEvery: 10 * time.Millisecond}, cf: cf, versioned: prepared, controller: ctl}
	if e = r.initializeTransport(); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { r.store.Close(); prepared.Close() })
	return r, p
}
func waitTransport(t *testing.T, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("transport synchronization timed out")
}

type countedInputReader struct {
	io.Reader
	closed atomic.Int64
}

func (r *countedInputReader) Close() error { r.closed.Add(1); return nil }

type slowDestination struct {
	mu sync.Mutex
	bytes.Buffer
	entered chan struct{}
	unblock chan struct{}
	once    sync.Once
}

func (s *slowDestination) Write(ctx context.Context, b []byte) (int, error) {
	s.once.Do(func() {
		close(s.entered)
		select {
		case <-s.unblock:
		case <-ctx.Done():
			return
		}
	})
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Buffer.Write(b)
}
func TestTransportRealSignedAgentGatewayRoundTripSlowReceiverAndNormalCall(t *testing.T) {
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	addr := listener.Addr().String()
	listener.Close()
	private, _ := base64.RawURLEncoding.DecodeString(testAgentPrivateKey)
	public := ed25519.PrivateKey(private).Public().(ed25519.PublicKey)
	hash, _ := bcrypt.GenerateFromPassword([]byte("transport-test"), bcrypt.MinCost)
	server := gateway.NewServer(gateway.Config{Addr: addr, AgentPublicKey: base64.RawURLEncoding.EncodeToString(public), APIKeys: []gateway.APIKey{{Name: "test", KeyHash: string(hash), Capabilities: []string{"*"}}}, RateLimit: gateway.RateLimitConfig{RequestsPerSecond: 1000, Burst: 1000}, AgentPingInterval: 30 * time.Millisecond, AgentPongTimeout: 100 * time.Millisecond}, io.Discard)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	gatewayDone := make(chan error, 1)
	go func() { gatewayDone <- server.ListenAndServeContext(ctx) }()
	waitTransport(t, func() bool {
		conn, e := net.DialTimeout("tcp", addr, 20*time.Millisecond)
		if e == nil {
			conn.Close()
			return true
		}
		return false
	})
	runner, provider := transportFixture(t, "ws://"+addr+"/ws/agent", "")
	runner.logOut = io.Discard
	runnerDone := make(chan error, 1)
	go func() { runnerDone <- runner.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-runnerDone:
		case <-time.After(time.Second):
			t.Error("Agent did not stop")
		}
		select {
		case <-gatewayDone:
		case <-time.After(time.Second):
			t.Error("Gateway did not stop")
		}
	}()
	client := &http.Client{Timeout: time.Second}
	get := func(path string) *http.Response {
		req, _ := http.NewRequest("GET", "http://"+addr+path, nil)
		req.Header.Set("Authorization", "Bearer transport-test")
		response, e := client.Do(req)
		if e != nil {
			return nil
		}
		return response
	}
	waitTransport(t, func() bool {
		r := get("/openapi.json")
		if r == nil {
			return false
		}
		defer r.Body.Close()
		return r.StatusCode == 200
	})
	snapshot, _ := runner.versioned.Snapshot("run")
	body := bytes.Repeat([]byte{0, 255, 10, 1}, 40000)
	begin := protocol.Frame{Type: "request_begin", ID: "real-roundtrip", Capability: "run", CallerBinding: "test-caller", DefinitionHash: snapshot.DefinitionHash(), InputDeadline: time.Now().Add(2 * time.Second)}
	source := &countedInputReader{Reader: bytes.NewReader(body)}
	result, e := server.TransferInput(ctx, begin, map[string]any{"doc": nil}, []gateway.InputStream{{Pointer: "/doc", Name: "input.bin", MIMEType: "application/octet-stream", Reader: source}})
	if e != nil || result.Error != nil {
		t.Fatal(e, result.Error)
	}
	if source.closed.Load() != 1 {
		t.Fatal("input reader closed more than once", source.closed.Load())
	}
	if provider.calls.Load() != 1 || result.Metrics.InputFileBytes != int64(len(body)) || result.Metrics.InputFileCount != 1 {
		t.Fatal("wrong execution or input metrics", provider.calls.Load(), result.Metrics)
	}
	var value contract.ResultValue
	if e = json.Unmarshal(result.Result, &value); e != nil || len(value.Files) != 1 {
		t.Fatal(e, value)
	}
	descriptor := value.Files[0]
	slow := &slowDestination{entered: make(chan struct{}), unblock: make(chan struct{})}
	download := make(chan error, 1)
	go func() {
		download <- server.TransferOutput(ctx, begin.ID, descriptor.ID, descriptor.SizeBytes, descriptor.SHA256, slow.Write)
	}()
	<-slow.entered
	// Short pong deadlines remain active while ACKs for this download are stopped.
	normalRequest, _ := http.NewRequest("POST", "http://"+addr+"/api/v1/capabilities/normal", strings.NewReader("{}"))
	normalRequest.Header.Set("Content-Type", "application/json")
	normalRequest.Header.Set("Authorization", "Bearer transport-test")
	response, e := client.Do(normalRequest)
	if e != nil {
		t.Fatal(e)
	}
	payload, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || !bytes.Contains(payload, []byte(`"count":1`)) {
		t.Fatal("normal call stopped by transfer", response.StatusCode, string(payload))
	}
	time.Sleep(180 * time.Millisecond)
	close(slow.unblock)
	if e = <-download; e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(slow.Bytes(), body) {
		t.Fatal("output byte mismatch")
	}
	if runner.store.Usage() != int64(len(body)) {
		t.Fatal("input retained after execution", runner.store.Usage())
	}
	canceled, stopOutput := context.WithTimeout(ctx, 80*time.Millisecond)
	blocked := &slowDestination{entered: make(chan struct{}), unblock: make(chan struct{})}
	canceledResult := make(chan error, 1)
	go func() {
		canceledResult <- server.TransferOutput(canceled, begin.ID, descriptor.ID, descriptor.SizeBytes, descriptor.SHA256, blocked.Write)
	}()
	<-blocked.entered
	if err := <-canceledResult; err == nil {
		t.Fatal("canceled receiver succeeded")
	}
	stopOutput()
	waitTransport(t, func() bool {
		runner.controls.mu.Lock()
		defer runner.controls.mu.Unlock()
		return runner.controls.transfers == 0
	})
	if runner.store.Usage() != int64(len(body)) {
		t.Fatal("cancel reclaimed retained artifact", runner.store.Usage())
	}

	// Same connection's completed ID cannot execute twice.
	if _, e = server.TransferInput(ctx, begin, map[string]any{}, nil); e == nil {
		t.Fatal("duplicate begin accepted")
	}
	if provider.calls.Load() != 1 {
		t.Fatal("duplicate reexecuted provider")
	}
	optional := begin
	optional.ID = "optional-zero"
	optional.InputDeadline = time.Now().Add(time.Second)
	zero, e := server.TransferInput(ctx, optional, map[string]any{}, nil)
	if e != nil || zero.Error != nil || zero.Metrics.InputFileCount != 0 {
		t.Fatal("optional zero input bypassed phase", e, zero.Error)
	}
	// Output failure is returned promptly and leaves other artifacts intact.
	missing, stopMissing := context.WithTimeout(ctx, time.Second)
	defer stopMissing()
	if err := server.TransferOutput(missing, begin.ID, "unknown-file", descriptor.SizeBytes, descriptor.SHA256, func(_ context.Context, b []byte) (int, error) { return len(b), nil }); err == nil || err.Error() != "NOT_FOUND" {
		t.Fatal("missing output", err)
	}
	waitTransport(t, func() bool { return runner.store.Usage() == 0 })
	expired, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	if err := server.TransferOutput(expired, begin.ID, descriptor.ID, descriptor.SizeBytes, descriptor.SHA256, func(_ context.Context, b []byte) (int, error) { return len(b), nil }); err == nil || err.Error() != "NOT_FOUND" {
		t.Fatal("expired output", err)
	}

}

// Adversarial peer still performs the production Runner's signed handshake;
// the real Gateway/Runner connection above separately proves both endpoints.
func signedInputPeer(t *testing.T, options ...string) (*Runner, *controllerFixture, *ws.Conn, func()) {
	r, p, c, _, _, cleanup := signedInputPeerConnections(t, options...)
	return r, p, c, cleanup
}
func signedInputPeerConnections(t *testing.T, options ...string) (*Runner, *controllerFixture, *ws.Conn, <-chan *ws.Conn, <-chan *ws.Conn, func()) {
	t.Helper()
	accepted := make(chan *ws.Conn, 2)
	closed := make(chan struct{})
	private, _ := base64.RawURLEncoding.DecodeString(testAgentPrivateKey)
	public := ed25519.PrivateKey(private).Public().(ed25519.PublicKey)
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ws/agent/challenge" {
			w.WriteHeader(201)
			io.WriteString(w, `{"challenge":"signed-input-challenge"}`)
			return
		}
		signature, e := base64.RawURLEncoding.DecodeString(r.Header.Get("X-Agent-Signature"))
		if e != nil || !ed25519.Verify(public, protocol.AgentAuthMessage(r.URL.Path, r.Header.Get("X-Agent-Timestamp"), r.Header.Get("X-Agent-Nonce"), r.Header.Get("X-Agent-Challenge"), r.Header.Get("Sec-WebSocket-Key")), signature) {
			http.Error(w, "invalid signature", 401)
			return
		}
		w.Header().Set(protocol.MajorHeader, protocol.Major)
		conn, e := ws.Accept(w, r)
		if e != nil {
			return
		}
		accepted <- conn
		<-closed
		conn.Close()
	}))
	extra := ""
	if len(options) > 0 {
		extra = options[0]
	}
	captureLogs := extra == "capture-logs"
	if captureLogs {
		extra = ""
	}
	runner, provider := transportFixture(t, "ws"+strings.TrimPrefix(peer.URL, "http")+"/ws/agent", extra)
	if captureLogs {
		runner.logOut = &transportLogBuffer{}
		runner.detailLog = &transportLogBuffer{}
	}
	clientWire := make(chan *ws.Conn, 4)
	runner.dial = func(timeout time.Duration, url string, headers http.Header) (*ws.Conn, error) {
		conn, e := ws.Dial(timeout, url, headers)
		if e == nil {
			clientWire <- conn
		}
		return conn, e
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	var conn *ws.Conn
	select {
	case conn = <-accepted:
	case <-time.After(time.Second):
		t.Fatal("signed connection failed")
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	cleanup := func() {
		cancel()
		conn.Close()
		close(closed)
		peer.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("Runner shutdown blocked")
		}
	}
	return runner, provider, conn, accepted, clientWire, cleanup
}
func inputRead(t *testing.T, conn *ws.Conn, kind string) protocol.Frame {
	t.Helper()
	raw, e := conn.ReadText()
	if e != nil {
		t.Fatal(e)
	}
	f, e := protocol.DecodeFrame(raw)
	if e != nil || f.Type != kind {
		t.Fatalf("want %s, got %s (%v)", kind, raw, e)
	}
	return f
}
func inputError(t *testing.T, conn *ws.Conn, code string) {
	t.Helper()
	raw, e := conn.ReadText()
	if e != nil {
		t.Fatal(e)
	}
	var r protocol.Response
	if e = json.Unmarshal(raw, &r); e != nil || r.Error == nil || r.Error.Code != code || r.Error.Outcome != "not_started" {
		t.Fatalf("want %s/not_started, got %s (%v)", code, raw, e)
	}
}
func inputBegin(t *testing.T, r *Runner, conn *ws.Conn, id string, deadline time.Time) {
	t.Helper()
	snapshot, _ := r.versioned.Snapshot("run")
	if e := conn.WriteText(protocol.MustJSON(protocol.Frame{Type: "request_begin", ID: id, Capability: "run", CallerBinding: "caller", DefinitionHash: snapshot.DefinitionHash(), InputDeadline: deadline})); e != nil {
		t.Fatal(e)
	}
	inputRead(t, conn, "request_accept")
}
func inputFile(t *testing.T, conn *ws.Conn, id, transfer, pointer string, complete bool) protocol.Frame {
	t.Helper()
	conn.WriteText(protocol.MustJSON(protocol.Frame{Type: "input_open", ID: id, TransferID: transfer, Pointer: pointer, Name: "secret-name.bin", MIMEType: "application/octet-stream"}))
	inputRead(t, conn, "input_accept")
	if !complete {
		return protocol.Frame{}
	}
	conn.WriteText(protocol.MustJSON(protocol.Frame{Type: "transfer_chunk", ID: id, TransferID: transfer, Sequence: 0, DataBase64: "QQ=="}))
	inputRead(t, conn, "ack")
	sum := sha256.Sum256([]byte("A"))
	conn.WriteText(protocol.MustJSON(protocol.Frame{Type: "transfer_end", ID: id, TransferID: transfer, Size: 1, SHA256: hex.EncodeToString(sum[:])}))
	return inputRead(t, conn, "input_ready")
}
func TestTransportSignedInputCommitBarrierBindingsAndRecent(t *testing.T) {
	for _, kind := range []string{"early", "unused", "foreign", "placeholder", "array_gap", "duplicate"} {
		t.Run(kind, func(t *testing.T) {
			r, p, conn, cleanup := signedInputPeer(t)
			defer cleanup()
			inputBegin(t, r, conn, "request", time.Now().Add(time.Second))
			pointer := "/doc"
			if kind == "array_gap" {
				pointer = "/documents/1"
			}
			ready := inputFile(t, conn, "request", "transfer", pointer, kind != "early")
			params := map[string]any{"doc": nil}
			files := []protocol.FileBinding{{Pointer: pointer, FileID: ready.FileID}}
			switch kind {
			case "early":
				files = []protocol.FileBinding{}
			case "unused":
				files = []protocol.FileBinding{}
				params = map[string]any{}
			case "foreign":
				files[0].FileID = "foreign-file"
			case "placeholder":
				params["doc"] = "non-null"
			case "array_gap":
				params = map[string]any{"documents": []any{nil, nil}}
			}
			commit := protocol.Frame{Type: "request_commit", ID: "request", Params: params, Files: files}
			conn.WriteText(protocol.MustJSON(commit))
			if kind == "duplicate" {
				raw, e := conn.ReadText()
				var response protocol.Response
				if e != nil || json.Unmarshal(raw, &response) != nil || response.Error != nil {
					t.Fatal(string(raw), e)
				}
				conn.WriteText(protocol.MustJSON(commit))
				inputError(t, conn, "CONFLICT")
				if p.calls.Load() != 1 {
					t.Fatal("commit reexecuted")
				}
			} else {
				inputError(t, conn, "VALIDATION_FAILED")
				if p.calls.Load() != 0 {
					t.Fatal("invalid input executed provider")
				}
			}
			waitTransport(t, func() bool { return r.store.Usage() == 0 || kind == "duplicate" && r.store.Usage() == 1 })
		})
	}
}
func TestTransportSignedInputProtocolViolationsCloseAndReclaim(t *testing.T) {
	for _, kind := range []string{"sequence", "owner", "credit", "duplicate_transfer", "invalid_id"} {
		t.Run(kind, func(t *testing.T) {
			r, p, conn, cleanup := signedInputPeer(t)
			defer cleanup()
			inputBegin(t, r, conn, "request", time.Now().Add(time.Second))
			inputFile(t, conn, "request", "transfer", "/doc", false)
			f := protocol.Frame{Type: "transfer_chunk", ID: "request", TransferID: "transfer", DataBase64: "QQ=="}
			switch kind {
			case "sequence":
				f.Sequence = 1
			case "owner":
				f.ID = "another"
			case "credit":
				r.controls.mu.Lock()
				r.controls.bytes.tokens = 0
				r.controls.bytes.rate = 1
				r.controls.mu.Unlock()
				for sequence := int64(0); sequence < 4; sequence++ {
					f.Sequence = sequence
					conn.WriteText(protocol.MustJSON(f))
				}
				f.Sequence = 4
			case "duplicate_transfer":
				f = protocol.Frame{Type: "input_open", ID: "request", TransferID: "transfer", Pointer: "/documents/0", Name: "a.bin", MIMEType: "application/octet-stream"}
			case "invalid_id":
				f.ID = strings.Repeat("x", 257)
			}
			conn.WriteText(protocol.MustJSON(f))
			for {
				_, e := conn.ReadText()
				if e != nil {
					break
				}
			}
			if p.calls.Load() != 0 {
				t.Fatal("protocol violation executed provider")
			}
			waitTransport(t, func() bool { return r.store.Usage() == 0 })
		})
	}
}
func TestTransportSignedPendingRecentFixedDeadlineAndRateOnce(t *testing.T) {
	r, p, conn, cleanup := signedInputPeer(t)
	defer cleanup()
	r.controller.rates["run"] = newCapabilityRateLimiter(RateLimitDef{Requests: 1, Per: "1h", Burst: 1}, time.Now())
	inputBegin(t, r, conn, "rate-once", time.Now().Add(time.Second))
	conn.WriteText(protocol.MustJSON(protocol.Frame{Type: "request_commit", ID: "rate-once", Params: map[string]any{}, Files: []protocol.FileBinding{}}))
	raw, e := conn.ReadText()
	var response protocol.Response
	if e != nil || json.Unmarshal(raw, &response) != nil || response.Error != nil || p.calls.Load() != 1 {
		t.Fatal("commit consumed second rate token", string(raw), e)
	}
	snapshot, _ := r.versioned.Snapshot("run")
	begin := protocol.Frame{Type: "request_begin", ID: "rate-twice", Capability: "run", CallerBinding: "caller", DefinitionHash: snapshot.DefinitionHash(), InputDeadline: time.Now().Add(time.Second)}
	conn.WriteText(protocol.MustJSON(begin))
	inputError(t, conn, "RATE_LIMITED")
	// Use the same fixed, capability-independent management buckets, without
	// inventing one bucket for each new externally supplied ID.
	delete(r.controller.rates, "run")
	for _, id := range []string{"pending-a", "pending-b"} {
		inputBegin(t, r, conn, id, time.Now().Add(time.Second))
	}
	begin.ID = "pending-full"
	conn.WriteText(protocol.MustJSON(begin))
	inputError(t, conn, "BUSY")
	for _, id := range []string{"pending-a", "pending-b"} {
		conn.WriteText(protocol.MustJSON(protocol.CancelRequest{Type: "cancel", ID: id}))
	}
	time.Sleep(30 * time.Millisecond)
	inputBegin(t, r, conn, "last-slot", time.Now().Add(100*time.Millisecond))
	inputError(t, conn, "EXECUTION_TIMEOUT")
	begin.ID = "recent-full"
	begin.InputDeadline = time.Now().Add(time.Second)
	conn.WriteText(protocol.MustJSON(begin))
	inputError(t, conn, "BUSY")
	if len(r.controls.buckets) != 4 {
		t.Fatal("management buckets grew")
	}
	if p.calls.Load() != 1 {
		t.Fatal("pending/expiry executed provider")
	}
}

func TestTransportSignedCancellationWaitsForWorkerAndReader(t *testing.T) {
	r, p, conn, cleanup := signedInputPeer(t)
	defer cleanup()
	entered, stopping, closeReader := make(chan struct{}), make(chan struct{}), make(chan struct{})
	p.fn = func(ctx context.Context, input map[string]any, effects *providers.Effects) (contract.ProviderResult, error) {
		reader, e := OpenInput(ctx, input["doc"].(contract.FileValue))
		if e != nil {
			return contract.ProviderResult{}, e
		}
		close(entered)
		<-ctx.Done()
		close(stopping)
		<-closeReader
		reader.Close()
		return contract.ProviderResult{}, ctx.Err()
	}
	inputBegin(t, r, conn, "cancel-worker", time.Now().Add(time.Second))
	ready := inputFile(t, conn, "cancel-worker", "reader", "/doc", true)
	conn.WriteText(protocol.MustJSON(protocol.Frame{Type: "request_commit", ID: "cancel-worker", Params: map[string]any{"doc": nil}, Files: []protocol.FileBinding{{Pointer: "/doc", FileID: ready.FileID}}}))
	<-entered
	conn.WriteText(protocol.MustJSON(protocol.CancelRequest{Type: "cancel", ID: "cancel-worker"}))
	<-stopping
	if r.store.Usage() != 1 {
		t.Fatal("live input reclaimed before worker stopped", r.store.Usage())
	}
	close(closeReader)
	raw, e := conn.ReadText()
	var response protocol.Response
	if e != nil || json.Unmarshal(raw, &response) != nil || response.Error == nil || response.Error.Code != "EXECUTION_TIMEOUT" {
		t.Fatal(string(raw), e)
	}
	waitTransport(t, func() bool { return r.store.Usage() == 0 })
	if p.calls.Load() != 1 {
		t.Fatal("cancel reexecuted")
	}
}
func TestTransportSignedManagementAdmissionBeforeLookupAndOptionalBarrier(t *testing.T) {
	r, p, conn, cleanup := signedInputPeer(t)
	defer cleanup()
	snapshot, _ := r.versioned.Snapshot("run")
	conn.WriteText(protocol.MustJSON(protocol.Request{Type: "request", ID: "no-phase", Capability: "run", DefinitionHash: snapshot.DefinitionHash(), Params: map[string]any{}}))
	inputError(t, conn, "INVALID_REQUEST")
	r.controls.mu.Lock()
	r.controls.buckets["write"].tokens = 1
	r.controls.buckets["write"].rate = 0
	r.controls.buckets["read"].tokens = 0
	r.controls.buckets["read"].rate = 0
	r.controls.mu.Unlock()
	begin := protocol.Frame{Type: "request_begin", ID: "unknown", Capability: "missing", CallerBinding: "caller", DefinitionHash: snapshot.DefinitionHash(), InputDeadline: time.Now().Add(time.Second)}
	conn.WriteText(protocol.MustJSON(begin))
	inputError(t, conn, "NOT_FOUND")
	begin.ID = "limited"
	begin.Capability = "run"
	conn.WriteText(protocol.MustJSON(begin))
	inputError(t, conn, "RATE_LIMITED")
	conn.WriteText(protocol.MustJSON(protocol.Frame{Type: "transfer_open", ID: "foreign", TransferID: "limited-read", FileID: "unknown"}))
	inputError(t, conn, "RATE_LIMITED")
	if len(r.controls.buckets) != 4 || r.store.Usage() != 0 || p.calls.Load() != 0 {
		t.Fatal("rejected management mutated state")
	}
}
func TestTransportLogicalByteBudgetIsCancelableAndFixedClasses(t *testing.T) {
	r, _ := transportFixture(t, "ws://127.0.0.1:1/ws/agent", "")
	r.controls.mu.Lock()
	r.controls.bytes.tokens = 0
	r.controls.bytes.rate = 1
	r.controls.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if e := r.controls.consume(ctx, protocol.ChunkBytes); e != context.DeadlineExceeded {
		t.Fatal(e)
	}
	releases := []func(){}
	for i := 0; i < 4; i++ {
		release, code, _ := r.controls.admit("transfer")
		if code != "" {
			t.Fatal(code)
		}
		releases = append(releases, release)
	}
	if _, code, _ := r.controls.admit("transfer"); code != "BUSY" {
		t.Fatal(code)
	}
	if release, code, _ := r.controls.admit("cancel"); code != "" {
		t.Fatal(code)
	} else {
		release()
	}
	for _, release := range releases {
		release()
		release()
	}
	if r.controls.buckets["transfer"].active != 0 || len(r.controls.buckets) != 4 {
		t.Fatal("admission retained or grew")
	}
}

func TestTransportTransferAdmissionEightSlotsAtomicAndReturns(t *testing.T) {
	r, _ := transportFixture(t, "ws://127.0.0.1:1/ws/agent", "")
	runtime := r.versioned.Runtime()
	runtime["control_limits"].(map[string]any)["max_concurrent_transfers"] = int64(100)
	control := newControlAdmission(runtime)
	if control.maxTransfers != 8 || runtime["control_limits"].(map[string]any)["max_concurrent_transfers"] != int64(100) {
		t.Fatal("protocol clamp changed configuration")
	}
	start, release := make(chan struct{}), make(chan struct{})
	var accepted atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			slot, ok := control.transferSlot()
			if ok {
				accepted.Add(1)
				<-release
				slot()
				slot()
			}
		}()
	}
	close(start)
	waitTransport(t, func() bool { return accepted.Load() == 8 })
	time.Sleep(10 * time.Millisecond)
	if slot, ok := control.transferSlot(); ok {
		slot()
		t.Fatal("ninth slot granted")
	}
	control.mu.Lock()
	count := control.transfers
	control.mu.Unlock()
	if count != 8 {
		t.Fatal(count)
	}
	close(release)
	wg.Wait()
	if control.transfers != 0 {
		t.Fatal("slot leak")
	}
}

func TestTransportSignedReconnectInvalidatesInputOutputAndOldFileIDs(t *testing.T) {
	r, p, conn, reconnect, _, cleanup := signedInputPeerConnections(t)
	defer cleanup()
	inputBegin(t, r, conn, "output-owner", time.Now().Add(time.Second))
	ready := inputFile(t, conn, "output-owner", "old-transfer", "/doc", true)
	conn.WriteText(protocol.MustJSON(protocol.Frame{Type: "request_commit", ID: "output-owner", Params: map[string]any{"doc": nil}, Files: []protocol.FileBinding{{Pointer: "/doc", FileID: ready.FileID}}}))
	raw, e := conn.ReadText()
	var response protocol.Response
	var result contract.ResultValue
	if e != nil || json.Unmarshal(raw, &response) != nil || response.Error != nil || json.Unmarshal(response.Result, &result) != nil {
		t.Fatal(string(raw), e)
	}
	inputBegin(t, r, conn, "unexecuted", time.Now().Add(time.Second))
	inputFile(t, conn, "unexecuted", "pending-transfer", "/doc", true)
	conn.Close()
	var next *ws.Conn
	select {
	case next = <-reconnect:
	case <-time.After(time.Second):
		t.Fatal("reconnect failed")
	}
	defer next.Close()
	next.SetReadDeadline(time.Now().Add(3 * time.Second))
	waitTransport(t, func() bool { return r.store.Usage() == 0 })
	next.WriteText(protocol.MustJSON(protocol.Frame{Type: "transfer_open", ID: "output-owner", TransferID: "new-transfer", FileID: result.Files[0].ID}))
	inputError(t, next, "NOT_FOUND")
	inputBegin(t, r, next, "output-owner", time.Now().Add(time.Second))
	next.WriteText(protocol.MustJSON(protocol.Frame{Type: "request_commit", ID: "output-owner", Params: map[string]any{"doc": nil}, Files: []protocol.FileBinding{{Pointer: "/doc", FileID: ready.FileID}}}))
	inputError(t, next, "VALIDATION_FAILED")
	if p.calls.Load() != 1 {
		t.Fatal("stale handle executed provider")
	}
}

func TestTransportSignedOutputOwnersReserveCapacityBeyondRecentExpiry(t *testing.T) {
	r, p, conn, _, _, cleanup := signedInputPeerConnections(t, "long-ttl")
	defer cleanup()
	ids := []string{"owner-a", "owner-b", "owner-c"}
	var first contract.PublicFileDescriptor
	for _, id := range ids {
		inputBegin(t, r, conn, id, time.Now().Add(100*time.Millisecond))
		conn.WriteText(protocol.MustJSON(protocol.Frame{Type: "request_commit", ID: id, Params: map[string]any{}, Files: []protocol.FileBinding{}}))
		raw, e := conn.ReadText()
		var response protocol.Response
		var result contract.ResultValue
		if e != nil || json.Unmarshal(raw, &response) != nil || response.Error != nil || json.Unmarshal(response.Result, &result) != nil {
			t.Fatal(string(raw), e)
		}
		if id == ids[0] {
			first = result.Files[0]
		}
	}
	// begin-time recent budget is 100ms + 1s execution + 2s output transfer.
	time.Sleep(3200 * time.Millisecond)
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	snapshot, _ := r.versioned.Snapshot("run")
	begin := protocol.Frame{Type: "request_begin", ID: ids[0], Capability: "run", CallerBinding: "caller", DefinitionHash: snapshot.DefinitionHash(), InputDeadline: time.Now().Add(time.Second)}
	conn.WriteText(protocol.MustJSON(begin))
	inputError(t, conn, "CONFLICT")
	conn.WriteText(protocol.MustJSON(protocol.Frame{Type: "transfer_open", ID: ids[0], TransferID: "retained-after-recent", FileID: first.ID}))
	end := inputRead(t, conn, "transfer_end")
	if end.Size != 0 || end.SHA256 != first.SHA256 {
		t.Fatal("owner expired with recent table")
	}
	inputBegin(t, r, conn, "fourth-reservation", time.Now().Add(time.Second))
	begin.ID = "fifth-reservation"
	conn.WriteText(protocol.MustJSON(begin))
	inputError(t, conn, "BUSY")
	if p.calls.Load() != 3 {
		t.Fatal("capacity rejection executed")
	}
}
func TestTransportSignedOutputFailureDoesNotRegisterOwnerOrLeakQuota(t *testing.T) {
	r, p, conn, cleanup := signedInputPeer(t)
	defer cleanup()
	p.fn = func(context.Context, map[string]any, *providers.Effects) (contract.ProviderResult, error) {
		return contract.ProviderResult{Files: []contract.ProviderFile{{Name: "private.bin", MIMEType: "application/octet-stream", Reader: badTransportReader{}}}}, nil
	}
	inputBegin(t, r, conn, "failed-output", time.Now().Add(time.Second))
	conn.WriteText(protocol.MustJSON(protocol.Frame{Type: "request_commit", ID: "failed-output", Params: map[string]any{}, Files: []protocol.FileBinding{}}))
	raw, e := conn.ReadText()
	var response protocol.Response
	if e != nil || json.Unmarshal(raw, &response) != nil || response.Error == nil {
		t.Fatal("partial output published", string(raw), e)
	}
	waitTransport(t, func() bool { return r.store.Usage() == 0 })
	conn.WriteText(protocol.MustJSON(protocol.Frame{Type: "transfer_open", ID: "failed-output", TransferID: "failed-lookup", FileID: "unpublished"}))
	inputError(t, conn, "NOT_FOUND")
}

type badTransportReader struct{}

func (badTransportReader) Read([]byte) (int, error) { return 0, fmt.Errorf("private local path") }

func TestTransportSignedEightTransferWindowsNinthRefusalAndCleanup(t *testing.T) {
	r, p, conn, _, _, cleanup := signedInputPeerConnections(t, "eight-transfers")
	defer cleanup()
	inputBegin(t, r, conn, "eight-windows", time.Now().Add(time.Second))
	for i := 0; i < 8; i++ {
		inputFile(t, conn, "eight-windows", fmt.Sprintf("window-%d", i), fmt.Sprintf("/documents/%d", i), false)
	}
	objects := filepath.Join(r.versioned.Runtime()["spool_dir"].(string), "objects")
	before, e := os.ReadDir(objects)
	if e != nil || len(before) != 8 {
		t.Fatal("initial windows not created", len(before), e)
	}
	conn.WriteText(protocol.MustJSON(protocol.Frame{Type: "input_open", ID: "eight-windows", TransferID: "window-9", Pointer: "/documents/8", Name: "ninth.bin", MIMEType: "application/octet-stream"}))
	inputError(t, conn, "BUSY")
	after, e := os.ReadDir(objects)
	if e != nil || len(after) != len(before) || p.calls.Load() != 0 {
		t.Fatal("capacity refusal changed stage/state", len(after), e)
	}
	conn.WriteText(protocol.MustJSON(protocol.CancelRequest{Type: "cancel", ID: "eight-windows"}))
	waitTransport(t, func() bool { r.controls.mu.Lock(); defer r.controls.mu.Unlock(); return r.controls.transfers == 0 })
	waitTransport(t, func() bool { entries, e := os.ReadDir(objects); return e == nil && len(entries) == 0 })
}

func TestTransportSignedInputEndIntegrityAndOpenRefusals(t *testing.T) {
	for _, kind := range []string{"size", "digest", "duplicate-pointer", "undeclared-leaf", "unknown-request"} {
		t.Run(kind, func(t *testing.T) {
			r, p, conn, cleanup := signedInputPeer(t)
			defer cleanup()
			inputBegin(t, r, conn, "integrity", time.Now().Add(time.Second))
			if kind == "duplicate-pointer" {
				inputFile(t, conn, "integrity", "first", "/doc", false)
			}
			if kind == "undeclared-leaf" || kind == "duplicate-pointer" || kind == "unknown-request" {
				pointer, id := "/doc", "integrity"
				if kind == "undeclared-leaf" {
					pointer = "/missing"
				}
				if kind == "unknown-request" {
					id = "unknown"
				}
				conn.WriteText(protocol.MustJSON(protocol.Frame{Type: "input_open", ID: id, TransferID: "refused", Pointer: pointer, Name: "private.bin", MIMEType: "application/octet-stream"}))
				code := "VALIDATION_FAILED"
				if kind != "undeclared-leaf" {
					code = "CONFLICT"
				}
				inputError(t, conn, code)
			} else {
				inputFile(t, conn, "integrity", "body", "/doc", false)
				conn.WriteText(protocol.MustJSON(protocol.Frame{Type: "transfer_chunk", ID: "integrity", TransferID: "body", DataBase64: "QQ=="}))
				inputRead(t, conn, "ack")
				sum := sha256.Sum256([]byte("A"))
				end := protocol.Frame{Type: "transfer_end", ID: "integrity", TransferID: "body", Size: 1, SHA256: hex.EncodeToString(sum[:])}
				if kind == "size" {
					end.Size = 2
				} else {
					end.SHA256 = strings.Repeat("0", 64)
				}
				conn.WriteText(protocol.MustJSON(end))
				inputError(t, conn, "VALIDATION_FAILED")
			}
			if p.calls.Load() != 0 {
				t.Fatal("refusal invoked provider")
			}
			waitTransport(t, func() bool { return r.store.Usage() == 0 })
		})
	}
}

func TestTransportSignedInputCleanupFailureIsSafeAndNotSuccess(t *testing.T) {
	r, p, conn, cleanup := signedInputPeer(t)
	defer cleanup()
	objects := filepath.Join(r.versioned.Runtime()["spool_dir"].(string), "objects")
	var replaced string
	p.fn = func(_ context.Context, _ map[string]any, effects *providers.Effects) (contract.ProviderResult, error) {
		entries, e := os.ReadDir(objects)
		if e != nil || len(entries) != 1 {
			return contract.ProviderResult{}, fmt.Errorf("fault fixture missing input")
		}
		replaced = filepath.Join(objects, entries[0].Name())
		if e = os.Remove(replaced); e != nil {
			return contract.ProviderResult{}, e
		}
		if e = os.Mkdir(replaced, 0700); e != nil {
			return contract.ProviderResult{}, e
		}
		if e = os.WriteFile(filepath.Join(replaced, "cleanup-blocker"), []byte("private input"), 0600); e != nil {
			return contract.ProviderResult{}, e
		}
		effects.NoChange()
		return contract.ProviderResult{}, nil
	}
	inputBegin(t, r, conn, "cleanup-failure", time.Now().Add(time.Second))
	ready := inputFile(t, conn, "cleanup-failure", "cleanup-body", "/doc", true)
	conn.WriteText(protocol.MustJSON(protocol.Frame{Type: "request_commit", ID: "cleanup-failure", Params: map[string]any{"doc": nil}, Files: []protocol.FileBinding{{Pointer: "/doc", FileID: ready.FileID}}}))
	raw, e := conn.ReadText()
	var response protocol.Response
	if e != nil || json.Unmarshal(raw, &response) != nil || response.Error == nil || response.Error.Code != "EXECUTION_FAILED" || response.Error.Outcome != "no_change" || bytes.Contains(raw, []byte(objects)) || bytes.Contains(raw, []byte("cleanup-blocker")) {
		t.Fatal("cleanup failure published success or detail", string(raw), e)
	}
	if r.store.Usage() != 1 {
		t.Fatal("failed cleanup forgot quota", r.store.Usage())
	}
	if e := os.Remove(filepath.Join(replaced, "cleanup-blocker")); e != nil {
		t.Fatal(e)
	}
	if e := os.Remove(replaced); e != nil {
		t.Fatal(e)
	}
}

func TestTransportSignedNormalRequestStrictDecoderAndIntegerPrecision(t *testing.T) {
	r, p, conn, cleanup := signedInputPeer(t)
	defer cleanup()
	snapshot, _ := r.versioned.Snapshot("normal")
	frame := protocol.Request{Type: "request", ID: "normal-precision", Capability: "normal", CallerBinding: "caller", DefinitionHash: snapshot.DefinitionHash(), Deadline: time.Now().Add(time.Second), Params: map[string]any{"n": int64(9223372036854775807)}}
	for n, value := range []int64{9223372036854775807, -9223372036854775808} {
		frame.ID = fmt.Sprintf("normal-precision%d", n)
		frame.Params = map[string]any{"n": value}
		conn.WriteText(protocol.MustJSON(frame))
		raw, e := conn.ReadText()
		if e != nil || !bytes.Contains(raw, []byte(fmt.Sprint(value))) {
			t.Fatal("normal integer precision", string(raw), e)
		}
	}
	frame.Params = map[string]any{"n": int64(9223372036854775807)}
	good := string(protocol.MustJSON(frame))
	for _, bad := range []string{strings.Replace(good, `"params":{"n":9223372036854775807}`, `"params":{"n":1,"n":2}`, 1), strings.Replace(good, `"type":"request"`, `"type":"bogus"`, 1), strings.Replace(good, `"capability":"normal"`, `"capability":7`, 1), strings.Replace(good, `"params":{"n":9223372036854775807}`, `"params":{"n":"`+string([]byte{0xff})+`"}`, 1)} {
		conn.WriteText([]byte(bad))
		inputError(t, conn, "INVALID_REQUEST")
	}
	if p.calls.Load() != 0 {
		t.Fatal("file provider called")
	}
}
func TestTransportSignedCommitNearInputDeadlineUsesExecutionBudget(t *testing.T) {
	r, p, conn, cleanup := signedInputPeer(t)
	defer cleanup()
	p.fn = func(ctx context.Context, _ map[string]any, effects *providers.Effects) (contract.ProviderResult, error) {
		effects.NoChange()
		select {
		case <-time.After(150 * time.Millisecond):
			return contract.ProviderResult{}, nil
		case <-ctx.Done():
			return contract.ProviderResult{}, ctx.Err()
		}
	}
	deadline := time.Now().Add(200 * time.Millisecond)
	inputBegin(t, r, conn, "near-deadline", deadline)
	time.Sleep(time.Until(deadline.Add(-60 * time.Millisecond)))
	conn.WriteText(protocol.MustJSON(protocol.Frame{Type: "request_commit", ID: "near-deadline", Params: map[string]any{}, Files: []protocol.FileBinding{}}))
	raw, e := conn.ReadText()
	var response protocol.Response
	if e != nil || json.Unmarshal(raw, &response) != nil || response.Error != nil || p.calls.Load() != 1 {
		t.Fatal("execution inherited input deadline", string(raw), e)
	}
	if time.Now().Before(deadline) {
		t.Fatal("test did not cross input deadline")
	}
}

type transportLogBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *transportLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}
func (b *transportLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}
func TestTransportSignedMalformedIDsCloseBeforeAdmissionAndPreserveReplacementRune(t *testing.T) {
	for _, id := range [][]byte{[]byte(`""`), []byte(`"` + strings.Repeat("x", 257) + `"`), []byte(`"` + strings.Repeat("x", 2000) + `"`), []byte{'"', 0xff, '"'}, []byte(`"\ud800"`), []byte(`"\udfff"`), []byte(`"nul\u0000id"`)} {
		t.Run(fmt.Sprintf("%x", id[:min(len(id), 8)]), func(t *testing.T) {
			r, p, conn, cleanup := signedInputPeer(t, "capture-logs")
			defer cleanup()
			r.controls.mu.Lock()
			tokens := r.controls.buckets["write"].tokens
			r.controls.mu.Unlock()
			raw := append([]byte(`{"type":"request_begin","id":`), id...)
			raw = append(raw, []byte(`,"capability":"run"}`)...)
			conn.WriteText(raw)
			if payload, e := conn.ReadText(); e == nil {
				t.Fatal("malformed ID echoed", string(payload))
			}
			r.controls.mu.Lock()
			after := r.controls.buckets["write"].tokens
			r.controls.mu.Unlock()
			if strings.Contains(r.logOut.(*transportLogBuffer).String(), "request_id") || r.detailLog.(*transportLogBuffer).String() != "" {
				t.Fatal("malformed ID reached logs")
			}
			if after != tokens || p.calls.Load() != 0 || r.store.Usage() != 0 {
				t.Fatal("malformed ID reached admission/provider")
			}
		})
	}
	r, _, conn, cleanup := signedInputPeer(t, "capture-logs")
	defer cleanup()
	inputBegin(t, r, conn, "valid-\ufffd", time.Now().Add(time.Second))
	conn.WriteText(protocol.MustJSON(protocol.CancelRequest{Type: "cancel", ID: "valid-\ufffd"}))
}
func TestTransportSignedRejectsReceiverOnlyFramesBeforeAdmission(t *testing.T) {
	for _, kind := range []string{"request_accept", "input_accept", "input_ready"} {
		t.Run(kind, func(t *testing.T) {
			r, p, conn, cleanup := signedInputPeer(t)
			defer cleanup()
			r.controls.mu.Lock()
			tokens := r.controls.buckets["write"].tokens
			r.controls.mu.Unlock()
			conn.WriteText(protocol.MustJSON(protocol.Frame{Type: kind, ID: "unsolicited", TransferID: "other", FileID: "file"}))
			if raw, e := conn.ReadText(); e == nil {
				t.Fatal("wrong-direction frame accepted", string(raw))
			}
			r.controls.mu.Lock()
			after := r.controls.buckets["write"].tokens
			r.controls.mu.Unlock()
			if after != tokens || p.calls.Load() != 0 || r.store.Usage() != 0 {
				t.Fatal("wrong-direction frame mutated admission/state")
			}
		})
	}
}

func TestTransportRealSignedCancelBeforeRequestAcceptReclaimsPending(t *testing.T) {
	testSignedDelayedAccept(t, false, false)
}
func TestTransportRealSignedLateAcceptUsesFixedInputBudget(t *testing.T) {
	testSignedDelayedAccept(t, true, false)
}
func TestTransportRealSignedRecentInputConflictExpiryAndOutputReopen(t *testing.T) {
	testSignedDelayedAccept(t, false, true)
}
func testSignedDelayedAccept(t *testing.T, late, expiry bool) {
	reserve, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	addr := reserve.Addr().String()
	reserve.Close()
	key, _ := base64.RawURLEncoding.DecodeString(testAgentPrivateKey)
	public := ed25519.PrivateKey(key).Public().(ed25519.PublicKey)
	hash, _ := bcrypt.GenerateFromPassword([]byte("cancel-test"), bcrypt.MinCost)
	server := gateway.NewServer(gateway.Config{Addr: addr, AgentPublicKey: base64.RawURLEncoding.EncodeToString(public), APIKeys: []gateway.APIKey{{Name: "test", KeyHash: string(hash), Capabilities: []string{"*"}}}, AgentPingInterval: 10 * time.Second}, io.Discard)
	ctx, cancel := context.WithTimeout(context.Background(), 14*time.Second)
	defer cancel()
	gatewayDone := make(chan error, 1)
	go func() { gatewayDone <- server.ListenAndServeContext(ctx) }()
	waitTransport(t, func() bool {
		c, e := net.DialTimeout("tcp", addr, 20*time.Millisecond)
		if e == nil {
			c.Close()
			return true
		}
		return false
	})
	proxy, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	var armed atomic.Bool
	var heldOnce, releaseOnce sync.Once
	held, release := make(chan struct{}), make(chan struct{})
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	var beginWrites atomic.Int32
	var peersMu sync.Mutex
	var peers []net.Conn
	go func() {
		for {
			front, e := proxy.Accept()
			if e != nil {
				return
			}
			back, e := net.Dial("tcp", addr)
			if e != nil {
				front.Close()
				continue
			}
			peersMu.Lock()
			peers = append(peers, front, back)
			peersMu.Unlock()
			go func() {
				defer front.Close()
				defer back.Close()
				io.Copy(&countBeginFrames{Writer: front, count: &beginWrites}, back)
			}()
			go func() {
				defer front.Close()
				defer back.Close()
				buffer := make([]byte, 65536)
				for {
					n, e := front.Read(buffer)
					if n > 0 {
						if armed.Load() {
							heldOnce.Do(func() { close(held) })
							select {
							case <-release:
							case <-ctx.Done():
								return
							}
						}
						if _, we := back.Write(buffer[:n]); we != nil {
							return
						}
					}
					if e != nil {
						return
					}
				}
			}()
		}
	}()
	r, p := transportFixture(t, "ws://"+proxy.Addr().String()+"/ws/agent", "")
	runnerDone := make(chan error, 1)
	go func() { runnerDone <- r.Run(ctx) }()
	defer func() {
		unblock()
		cancel()
		proxy.Close()
		peersMu.Lock()
		for _, c := range peers {
			c.Close()
		}
		peersMu.Unlock()
		select {
		case <-runnerDone:
		case <-time.After(time.Second):
			t.Error("runner stop")
		}
		select {
		case <-gatewayDone:
		case <-time.After(time.Second):
			t.Error("gateway stop")
		}
	}()
	waitTransport(t, func() bool {
		req, _ := http.NewRequest("GET", "http://"+addr+"/openapi.json", nil)
		req.Header.Set("Authorization", "Bearer cancel-test")
		resp, e := http.DefaultClient.Do(req)
		if e != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == 200
	})
	snapshot, _ := r.versioned.Snapshot("run")
	inputDeadline := time.Now().Add(2 * time.Second)
	if expiry {
		inputDeadline = time.Now().Add(300 * time.Millisecond)
	}
	callCtx, stopCall := context.WithCancel(ctx)
	var outerDeadline time.Time
	if late {
		stopCall()
		callCtx, stopCall = context.WithTimeout(ctx, 80*time.Millisecond)
		outerDeadline, _ = callCtx.Deadline()
	}
	result := make(chan error, 1)
	armed.Store(true)
	go func() {
		_, e := server.TransferInput(callCtx, protocol.Frame{Type: "request_begin", ID: "cancel-before-accept", Capability: "run", CallerBinding: "caller", DefinitionHash: snapshot.DefinitionHash(), InputDeadline: inputDeadline}, map[string]any{}, nil)
		result <- e
	}()
	select {
	case <-held:
	case <-time.After(time.Second):
		t.Fatal("accept did not reach delay proxy")
	}
	stopCall()
	if e := <-result; !errors.Is(e, context.Canceled) {
		t.Fatal("canceled begin", e)
	}
	if _, e := server.TransferInput(ctx, protocol.Frame{Type: "request_begin", ID: "cancel-before-accept", Capability: "run", CallerBinding: "caller", DefinitionHash: snapshot.DefinitionHash(), InputDeadline: time.Now().Add(time.Second)}, map[string]any{}, nil); e == nil || e.Error() != "CONFLICT" || beginWrites.Load() != 1 || p.calls.Load() != 0 {
		t.Fatal("recent input ID reused or transmitted", e, beginWrites.Load(), p.calls.Load())
	}
	// While request_accept is still held, both finite P06 pending slots must be
	// available. The original 2s input deadline must not perform this cleanup.
	allFree := false
	until := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(until) {
		var requests []*artifacts.Request
		var owners []artifacts.Owner
		for n := 0; n < 2; n++ {
			owner := artifacts.Owner{RequestID: fmt.Sprintf("probe%d", n), Capability: "run", CallerBinding: "probe", DefinitionHash: snapshot.DefinitionHash(), AuthGeneration: "probe"}
			request, e := r.store.BeginInput(ctx, owner, snapshot.Definition(), time.Now().Add(time.Second))
			if e != nil {
				break
			}
			requests = append(requests, request)
			owners = append(owners, owner)
		}
		allFree = len(requests) == 2
		for n, request := range requests {
			request.Abort(owners[n])
			request.ReleaseStopped(owners[n])
		}
		if allFree {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !allFree || !time.Now().Before(inputDeadline) || p.calls.Load() != 0 || r.store.Usage() != 0 {
		t.Fatal("begin cancel waited for input deadline or invoked provider", allFree, p.calls.Load(), r.store.Usage())
	}
	if late {
		time.Sleep(time.Until(outerDeadline.Add(5*time.Second + 60*time.Millisecond)))
		if !time.Now().Before(inputDeadline.Add(5 * time.Second)) {
			t.Fatal("late-accept test exceeded begin discard budget")
		}
	}
	armed.Store(false)
	unblock()
	// A late accept is discarded without disconnecting unrelated work.
	response, e := server.TransferInput(ctx, protocol.Frame{Type: "request_begin", ID: "after-cancel", Capability: "run", CallerBinding: "caller", DefinitionHash: snapshot.DefinitionHash(), InputDeadline: time.Now().Add(time.Second)}, map[string]any{}, nil)
	if e != nil || response.Error != nil || p.calls.Load() != 1 {
		t.Fatal("late accept broke healthy connection", e, response.Error)
	}
	if expiry {
		time.Sleep(time.Until(inputDeadline.Add(time.Second + 2*time.Second + 5*time.Second + 60*time.Millisecond)))
		response, e = server.TransferInput(ctx, protocol.Frame{Type: "request_begin", ID: "cancel-before-accept", Capability: "run", CallerBinding: "caller", DefinitionHash: snapshot.DefinitionHash(), InputDeadline: time.Now().Add(time.Second)}, map[string]any{}, nil)
		if e != nil || response.Error != nil || p.calls.Load() != 2 || beginWrites.Load() != 3 {
			t.Fatal("expired input ID remained reserved", e, response.Error, p.calls.Load(), beginWrites.Load())
		}
		var value contract.ResultValue
		if json.Unmarshal(response.Result, &value) != nil || len(value.Files) != 1 {
			t.Fatal(string(response.Result))
		}
		file := value.Files[0]
		for range 2 {
			if e := server.TransferOutput(ctx, "cancel-before-accept", file.ID, file.SizeBytes, file.SHA256, func(_ context.Context, b []byte) (int, error) { return len(b), nil }); e != nil {
				t.Fatal("recent request prevented repeat output read", e)
			}
		}
	}
}

func holdSignedFrameWriter(t *testing.T, conn *ws.Conn) func() {
	t.Helper()
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		conn.WriteTextWithDeadlineStart(protocol.MustJSON(protocol.Frame{Type: "request_accept", ID: "barrier-holder"}), time.Second, func() error { close(entered); <-release; return nil })
	}()
	<-entered
	var once sync.Once
	return func() { once.Do(func() { close(release); <-done }) }
}
func TestTransportSignedActualWriteBarrierRejectsEarlyCreditAndACK(t *testing.T) {
	for _, kind := range []string{"input_accept", "ack", "output_chunk"} {
		t.Run(kind, func(t *testing.T) {
			r, p, peer, _, wire, cleanup := signedInputPeerConnections(t)
			defer cleanup()
			client := <-wire
			inputBegin(t, r, peer, "barrier-owner", time.Now().Add(time.Second))
			var descriptor contract.PublicFileDescriptor
			if kind == "ack" {
				peer.WriteText(protocol.MustJSON(protocol.Frame{Type: "input_open", ID: "barrier-owner", TransferID: "wire", Pointer: "/doc", Name: "x", MIMEType: "application/octet-stream"}))
				inputRead(t, peer, "input_accept")
			}
			if kind == "output_chunk" {
				ready := inputFile(t, peer, "barrier-owner", "wire", "/doc", true)
				peer.WriteText(protocol.MustJSON(protocol.Frame{Type: "request_commit", ID: "barrier-owner", Params: map[string]any{"doc": nil}, Files: []protocol.FileBinding{{Pointer: "/doc", FileID: ready.FileID}}}))
				raw, e := peer.ReadText()
				var response protocol.Response
				var result contract.ResultValue
				if e != nil || json.Unmarshal(raw, &response) != nil || json.Unmarshal(response.Result, &result) != nil || len(result.Files) != 1 {
					t.Fatal(string(raw), e)
				}
				descriptor = result.Files[0]
			}
			release := holdSignedFrameWriter(t, client)
			defer release()
			switch kind {
			case "input_accept":
				peer.WriteText(protocol.MustJSON(protocol.Frame{Type: "input_open", ID: "barrier-owner", TransferID: "wire", Pointer: "/doc", Name: "x", MIMEType: "application/octet-stream"}))
				waitTransport(t, func() bool {
					entries, _ := os.ReadDir(filepath.Join(filepath.Dir(r.cf.ConfigPath), "spool", "objects"))
					return len(entries) == 1
				})
				peer.WriteText(protocol.MustJSON(protocol.Frame{Type: "transfer_chunk", ID: "barrier-owner", TransferID: "wire", Sequence: 0, DataBase64: "QQ=="}))
			case "ack":
				for seq := int64(0); seq < 4; seq++ {
					peer.WriteText(protocol.MustJSON(protocol.Frame{Type: "transfer_chunk", ID: "barrier-owner", TransferID: "wire", Sequence: seq, DataBase64: "QQ=="}))
				}
				waitTransport(t, func() bool { return r.store.Usage() > 0 })
				peer.WriteText(protocol.MustJSON(protocol.Frame{Type: "transfer_chunk", ID: "barrier-owner", TransferID: "wire", Sequence: 4, DataBase64: "QQ=="}))
			case "output_chunk":
				peer.WriteText(protocol.MustJSON(protocol.Frame{Type: "transfer_open", ID: "barrier-owner", TransferID: "output-wire", FileID: descriptor.ID}))
				waitTransport(t, func() bool { r.controls.mu.Lock(); defer r.controls.mu.Unlock(); return r.controls.transfers == 1 })
				time.Sleep(20 * time.Millisecond)
				peer.WriteText(protocol.MustJSON(protocol.Frame{Type: "ack", TransferID: "output-wire", Sequence: 0}))
			}
			if raw, e := peer.ReadText(); e == nil {
				t.Fatal("peer used credit before locked network write", string(raw))
			}
			release()
			waitTransport(t, func() bool { return r.store.Usage() == 0 })
			expected := int32(0)
			if kind == "output_chunk" {
				expected = 1
			}
			if p.calls.Load() != expected {
				t.Fatal("wrong provider calls", p.calls.Load())
			}
		})
	}
}
func TestTransportSignedKnownInputByteWaitUsesFixedDeadlineAndCancel(t *testing.T) {
	for _, kind := range []string{"input_open", "request_commit", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			r, p, peer, cleanup := signedInputPeer(t)
			defer cleanup()
			deadline := time.Now().Add(150 * time.Millisecond)
			if kind == "cancel" {
				deadline = time.Now().Add(time.Second)
			}
			inputBegin(t, r, peer, "byte-owner", deadline)
			r.controls.mu.Lock()
			r.controls.bytes.tokens = 0
			r.controls.bytes.rate = 1
			r.controls.mu.Unlock()
			frame := protocol.Frame{Type: kind, ID: "byte-owner", TransferID: "byte-wire", Pointer: "/doc", Name: "x", MIMEType: "application/octet-stream", Params: map[string]any{}, Files: []protocol.FileBinding{}}
			class := "write"
			if kind != "request_commit" {
				frame.Type = "input_open"
				class = "transfer"
			}
			peer.WriteText(protocol.MustJSON(frame))
			waitTransport(t, func() bool {
				r.controls.mu.Lock()
				defer r.controls.mu.Unlock()
				return r.controls.buckets[class].active == 1
			})
			started := time.Now()
			if kind == "cancel" {
				peer.WriteText(protocol.MustJSON(protocol.CancelRequest{Type: "cancel", ID: "byte-owner"}))
			}
			waitTransport(t, func() bool {
				r.controls.mu.Lock()
				defer r.controls.mu.Unlock()
				return r.controls.buckets[class].active == 0
			})
			if time.Since(started) > 400*time.Millisecond || r.store.Usage() != 0 || p.calls.Load() != 0 {
				t.Fatal("input byte wait outlived fixed phase", time.Since(started))
			}
			snapshot, _ := r.versioned.Snapshot("normal")
			peer.WriteText(protocol.MustJSON(protocol.Request{Type: "request", ID: "healthy", Capability: "normal", CallerBinding: "caller", DefinitionHash: snapshot.DefinitionHash(), Deadline: time.Now().Add(time.Second), Params: map[string]any{}}))
			for {
				raw, e := peer.ReadText()
				var response protocol.Response
				if e != nil || json.Unmarshal(raw, &response) != nil {
					t.Fatal("byte wait disconnected unrelated request", string(raw), e)
				}
				if response.ID == "healthy" {
					if response.Error != nil {
						t.Fatal(response.Error)
					}
					break
				}
			}
		})
	}
}

func TestTransportSignedCancelDuringWriteBarrierKeepsOtherRequestsAlive(t *testing.T) {
	r, p, peer, _, wire, cleanup := signedInputPeerConnections(t)
	defer cleanup()
	client := <-wire
	inputBegin(t, r, peer, "barrier-cancel", time.Now().Add(time.Second))
	release := holdSignedFrameWriter(t, client)
	defer release()
	peer.WriteText(protocol.MustJSON(protocol.Frame{Type: "input_open", ID: "barrier-cancel", TransferID: "wire", Pointer: "/doc", Name: "x", MIMEType: "application/octet-stream"}))
	waitTransport(t, func() bool {
		entries, _ := os.ReadDir(filepath.Join(filepath.Dir(r.cf.ConfigPath), "spool", "objects"))
		return len(entries) == 1
	})
	peer.WriteText(protocol.MustJSON(protocol.CancelRequest{Type: "cancel", ID: "barrier-cancel"}))
	waitTransport(t, func() bool {
		entries, _ := os.ReadDir(filepath.Join(filepath.Dir(r.cf.ConfigPath), "spool", "objects"))
		return len(entries) == 0
	})
	release()
	snapshot, _ := r.versioned.Snapshot("normal")
	peer.WriteText(protocol.MustJSON(protocol.Request{Type: "request", ID: "other-live", Capability: "normal", CallerBinding: "caller", DefinitionHash: snapshot.DefinitionHash(), Deadline: time.Now().Add(time.Second), Params: map[string]any{}}))
	for {
		raw, e := peer.ReadText()
		var id struct {
			ID string `json:"id"`
		}
		if e != nil || json.Unmarshal(raw, &id) != nil {
			t.Fatal("cancel during write lock disconnected other requests", e)
		}
		if id.ID == "other-live" {
			var response protocol.Response
			if json.Unmarshal(raw, &response) != nil || response.Error != nil {
				t.Fatal(string(raw))
			}
			break
		}
	}
	if p.calls.Load() != 0 || r.store.Usage() != 0 {
		t.Fatal("canceled barrier provider or quota")
	}
}

type countBeginFrames struct {
	io.Writer
	count *atomic.Int32
	tail  []byte
}

func (w *countBeginFrames) Write(p []byte) (int, error) {
	pattern := []byte(`"type":"request_begin"`)
	joined := append(w.tail, p...)
	w.count.Add(int32(bytes.Count(joined, pattern)))
	keep := min(len(joined), len(pattern)-1)
	w.tail = append([]byte(nil), joined[len(joined)-keep:]...)
	return w.Writer.Write(p)
}
