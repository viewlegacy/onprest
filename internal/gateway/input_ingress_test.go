package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/viewlegacy/onprest/internal/contract"
	"github.com/viewlegacy/onprest/internal/protocol"
	"golang.org/x/crypto/bcrypt"
)

func TestInlineInputCanonicalFileAndNestedLimits(t *testing.T) {
	schema, e := contract.CompileSchema([]byte(`properties: {nested: {properties: {files: {items: {type: file, max_bytes: 4, extensions: [pdf], mime_types: [application/pdf], allow_empty: false}, maxItems: 2}}}}`), true)
	if e != nil {
		t.Fatal(e)
	}
	for _, test := range []struct {
		label, data, name, media string
		valid                    bool
	}{{"canonical", "AQIDBA==", "A.PDF", "application/pdf; charset=binary", true}, {"empty", "", "a.pdf", "application/pdf", false}, {"whitespace", "AQID\nBA==", "a.pdf", "application/pdf", false}, {"url", "____", "a.pdf", "application/pdf", false}, {"paddingbits", "AR==", "a.pdf", "application/pdf", false}, {"missingpadding", "AQ", "a.pdf", "application/pdf", false}, {"overflow", "AQIDBAU=", "a.pdf", "application/pdf", false}, {"path", "AQ==", "../a.pdf", "application/pdf", false}, {"extension", "AQ==", "a.txt", "application/pdf", false}, {"mime", "AQ==", "a.pdf", "text/plain", false}} {
		t.Run(test.label, func(t *testing.T) {
			in := map[string]any{"nested": map[string]any{"files": []any{map[string]any{"name": test.name, "mime_type": test.media, "data": test.data}}}}
			var streams []InputStream
			value, e := normalizeInline(schema, in, "", "request", &streams)
			defer closeStreams(streams)
			if (e == nil) != test.valid {
				t.Fatal(e)
			}
			if e != nil {
				return
			}
			out, leaves, e := contract.ValidateInput(schema, value.(map[string]any), contract.Limits{MaxBytes: 1024, MaxDepth: 8, MaxItems: 30, MaxFileBytes: 4, MaxTotalFileBytes: 8, MaxFiles: 2})
			if e != nil || len(leaves) != 1 || leaves[0].Pointer != "/nested/files/0" {
				t.Fatal(out, leaves, e)
			}
			b, e := io.ReadAll(streams[0].Reader)
			if e != nil || string(b) != string([]byte{1, 2, 3, 4}) {
				t.Fatal(b, e)
			}
		})
	}
}
func TestMultipartGuardBoundedFramingAndStrictEOF(t *testing.T) {
	for _, test := range []struct {
		name, wire string
		valid      bool
	}{{"normal", "--b\r\nContent-Disposition: form-data; name=x\r\n\r\na\n--bxxxx\n\r\n--b--\r\n", true}, {"empty", "--b--\r\n", true}, {"preamble", "pre\r\n--b--\r\n", false}, {"epilogue", "--b--\r\nx", false}, {"LF", "--b\n", false}, {"unfinished", "--b\r\nContent-Type: application/json\r\n\r\n0", false}, {"oversizedheader", "--b\r\nX: " + strings.Repeat("x", 8192) + "\r\n\r\n0\r\n--b--\r\n", false},
		// Transport padding after a delimiter still opens a counted part header.
		{"padded", "--b \t\r\nContent-Disposition: form-data; name=x\r\n\r\na\r\n--b  \r\nContent-Disposition: form-data; name=y\r\n\r\nb\r\n--b-- \r\n", true},
		{"padded_space_oversized_header", "--b \r\nX: " + strings.Repeat("x", 8192) + "\r\n\r\n0\r\n--b--\r\n", false},
		{"padded_tab_oversized_header", "--b\r\nContent-Disposition: form-data; name=x\r\n\r\na\r\n--b\t\r\nX: " + strings.Repeat("x", 8192) + "\r\n\r\n0\r\n--b--\r\n", false},
		{"dash_content", "--b\r\nContent-Disposition: form-data; name=x\r\n\r\na\r\n--b-x\r\n--b \rx\r\n--b--\r\n", true}} {
		t.Run(test.name, func(t *testing.T) {
			g := &multipartGuard{source: strings.NewReader(test.wire), boundary: "b", first: true}
			_, e := io.Copy(io.Discard, g)
			if (e == nil) != test.valid {
				t.Fatal(e)
			}
			if cap(g.prefix) > 128 {
				t.Fatal("unbounded prefix")
			}
		})
	}
}
func TestInputBodyLeaseJSONAndMCPRetainedUntilWriterReleases(t *testing.T) {
	for _, protocolName := range []string{"rest", "mcp"} {
		t.Run(protocolName, func(t *testing.T) {
			s := newCancelStateTestServer()
			hash, _ := bcrypt.GenerateFromPassword([]byte("dev-key"), bcrypt.MinCost)
			s.cfg.APIKeys = []APIKey{{Name: "test", KeyHash: string(hash), Capabilities: []string{"*"}}}
			s.bodySlots = make(chan struct{}, 2)
			conn := newDeterministicCancelTransport()
			ac := newAgentConn(conn)
			ac.initializeTransfers()
			s.agent = ac
			seedVersionedTestMetadata(s)
			entered, unblock := make(chan struct{}), make(chan struct{})
			var unblockOnce sync.Once
			releaseWriter := func() { unblockOnce.Do(func() { close(unblock) }) }
			ac.scheduler.Post(ac.transferCtx, true, "", func() error { close(entered); <-unblock; return nil })
			go ac.scheduler.Run()
			go s.writeAgent(ac)
			<-entered
			defer func() { releaseWriter(); conn.Close(); ac.disconnectPending() }()
			var cancels []context.CancelFunc
			results := make(chan int, 2)
			for range 2 {
				ctx, cancel := context.WithCancel(context.Background())
				cancels = append(cancels, cancel)
				req := httptest.NewRequest("POST", "/api/v1/capabilities/get_customer", strings.NewReader(`{"large":"`+strings.Repeat("x", 64<<10)+`"}`)).WithContext(ctx)
				if protocolName == "mcp" {
					req = httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_customer","arguments":{"large":"`+strings.Repeat("x", 64<<10)+`"}}}`)).WithContext(ctx)
					req.Header.Set("MCP-Protocol-Version", "2025-11-25")
				}
				req.Header.Set("Authorization", "Bearer dev-key")
				req.Header.Set("Content-Type", "application/json")
				go func() { rec := httptest.NewRecorder(); s.httpSrv.Handler.ServeHTTP(rec, req); results <- rec.Code }()
			}
			waitForCondition(t, 2*time.Second, func() bool { return len(s.bodySlots) == 2 }, "body slots occupied")
			waitForCondition(t, 2*time.Second, func() bool { ac.mu.Lock(); defer ac.mu.Unlock(); return len(ac.pending) == 2 }, "JSON parsed and requests retained")
			waitForCondition(t, 2*time.Second, func() bool { return len(ac.send) == 1 }, "both serialized requests queued")
			req := httptest.NewRequest("POST", "/api/v1/capabilities/get_customer", strings.NewReader(`{}`))
			req.Header.Set("Authorization", "Bearer dev-key")
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			s.httpSrv.Handler.ServeHTTP(rec, req)
			if rec.Code != 503 || !strings.Contains(rec.Body.String(), "BUSY") {
				t.Fatal(rec.Code, rec.Body.String())
			}
			for _, cancel := range cancels {
				cancel()
			}
			for range 2 {
				select {
				case status := <-results:
					if status != 504 && status != 200 {
						t.Fatal(status)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("cancel blocked")
				}
			}
			if len(s.bodySlots) != 2 {
				t.Fatal("input payload lease released while queued", len(s.bodySlots))
			}
			// A scheduler retains serialized input after HTTP cancellation. Unblocking
			// it discards both jobs, drops their references, and only then returns slots.
			releaseWriter()
			waitForCondition(t, time.Second, func() bool { return len(s.bodySlots) == 0 }, "queued payloads released")
			runtime.GC()
			var memory runtime.MemStats
			runtime.ReadMemStats(&memory)
			t.Logf("body_slots=%d queued_requests=%d heap_alloc=%d", len(s.bodySlots), len(ac.send), memory.HeapAlloc)
			if len(ac.send) != 0 || memory.HeapAlloc > 64<<20 {
				t.Fatal("cancelled input memory/queue remained", memory.HeapAlloc)
			}
		})
	}
}
func TestInputUsageErrorFrameRoundTrip(t *testing.T) {
	r := protocol.Response{ID: "request", Error: &contract.ErrorValue{Code: "LIMIT_EXCEEDED", Message: "limit", Outcome: "not_started"}, Metrics: &protocol.Metrics{ExecutorKind: "file", InputFileCount: 1, InputFileBytes: 65536}}
	b, e := json.Marshal(r)
	if e != nil {
		t.Fatal(e)
	}
	var got protocol.Response
	if json.Unmarshal(b, &got) != nil || got.Metrics == nil || got.Metrics.InputFileBytes != 65536 {
		t.Fatal(string(b))
	}
}

var _ = http.MethodPost

func TestInputCommitBodyLeaseRetainsCancelledQueuedParams(t *testing.T) {
	s := newCancelStateTestServer()
	s.bodySlots = make(chan struct{}, 1)
	conn := newDeterministicCancelTransport()
	ac := newAgentConn(conn)
	ac.initializeTransfers()
	entered, unblock := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(unblock) }) }
	ac.scheduler.Post(ac.transferCtx, true, "", func() error { close(entered); <-unblock; return nil })
	go ac.scheduler.Run()
	<-entered
	defer func() { release(); conn.Close(); ac.disconnectPending() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, lease, ok := s.acquireBody(httptest.NewRequest("POST", "/", nil).WithContext(ctx))
	if !ok {
		t.Fatal("body lease")
	}
	done := make(chan error, 1)
	go func() {
		done <- ac.frame(r.Context(), protocol.Frame{Type: "request_commit", ID: "test", Params: map[string]any{"large": strings.Repeat("x", 64<<10)}})
	}()
	waitForCondition(t, time.Second, func() bool { lease.mu.Lock(); defer lease.mu.Unlock(); return lease.refs == 2 }, "commit retained")
	cancel()
	select {
	case e := <-done:
		// The commit never started writing, so nothing can have run.
		if errors.As(e, new(inputExecutionError)) || inputResult(e).Failure.Outcome != "not_started" {
			t.Fatal("queued commit cancellation", e)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel blocked")
	}
	lease.Close()
	if len(s.bodySlots) != 1 {
		t.Fatal("queued commit returned its input slot")
	}
	release()
	waitForCondition(t, time.Second, func() bool { return len(s.bodySlots) == 0 }, "commit payload discarded")
}

// Once the commit write has started, the Agent may already be executing it:
// cancellation from then on is reported as an unknown outcome.
func TestInputCommitCancelAfterWriteStartIsUnknown(t *testing.T) {
	s := newCancelStateTestServer()
	s.bodySlots = make(chan struct{}, 1)
	conn := newDeterministicCancelTransport()
	unblock := make(chan struct{})
	conn.blockWrite[0] = unblock
	ac := newAgentConn(conn)
	ac.initializeTransfers()
	go ac.scheduler.Run()
	defer func() { conn.Close(); ac.disconnectPending() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, lease, ok := s.acquireBody(httptest.NewRequest("POST", "/", nil).WithContext(ctx))
	if !ok {
		t.Fatal("body lease")
	}
	defer lease.Close()
	done := make(chan error, 1)
	go func() {
		done <- ac.frame(r.Context(), protocol.Frame{Type: "request_commit", ID: "test", Params: map[string]any{"x": "y"}})
	}()
	select {
	case <-conn.writeEvents: // the write began and is now held open
	case <-time.After(time.Second):
		t.Fatal("commit write did not start")
	}
	cancel()
	var e error
	select {
	case e = <-done:
	case <-time.After(time.Second):
		t.Fatal("cancel blocked")
	}
	close(unblock)
	result := inputResult(e)
	if !errors.As(e, new(inputExecutionError)) || result.Failure.Outcome != "unknown" {
		t.Fatal("started commit cancellation", e, result)
	}
}

func TestMultipartGuardAggregateFramingBoundary(t *testing.T) {
	for _, delta := range []int64{-1, 0, 1} {
		var wire strings.Builder
		for range 127 {
			wire.WriteString("--b\r\nX: " + strings.Repeat("x", 8185) + "\r\n\r\nx\r\n")
		}
		lastHeaders := (1 << 20) + delta - 127*(8192+5) - 5 - 7
		wire.WriteString("--b\r\nX: " + strings.Repeat("x", int(lastHeaders)-7) + "\r\n\r\nx\r\n--b--\r\n")
		guard := &multipartGuard{source: strings.NewReader(wire.String()), boundary: "b", first: true}
		_, e := io.Copy(io.Discard, guard)
		if (e == nil) != (delta <= 0) {
			t.Fatalf("delta %d framing %d: %v", delta, guard.framing, e)
		}
	}
}

func TestInputBodyReadBudgetUsesAuthorizedMetadataGeneration(t *testing.T) {
	s := NewServer(Config{BodyReadTimeout: 15 * time.Second}, io.Discard)
	s.capabilities = map[string]protocol.CapabilityMetadata{"short": {InputTimeout: "100ms"}, "long": {InputTimeout: "20s"}, "json": {}}
	for _, test := range []struct {
		name string
		key  *APIKey
		want time.Duration
	}{
		{"short", nil, 100 * time.Millisecond}, {"long", nil, 15 * time.Second}, {"json", nil, 15 * time.Second},
		{"", &APIKey{Capabilities: []string{"short"}}, 100 * time.Millisecond},
		{"", &APIKey{Capabilities: []string{"short", "long"}}, 15 * time.Second},
		{"", &APIKey{Capabilities: []string{"short", "json"}}, 15 * time.Second},
	} {
		if got := s.inputBodyReadBudget(test.name, test.key); got != test.want {
			t.Fatal(test.name, got, test.want)
		}
	}
}

func TestInputExecutionFailureRetainsCauseAndOutcome(t *testing.T) {
	for _, cause := range []error{context.DeadlineExceeded, context.Canceled, io.EOF} {
		wrapped := inputExecutionError{cause}
		if !errors.Is(wrapped, cause) {
			t.Fatal("execution cause lost", cause)
		}
		result := inputResult(wrapped)
		code, status := "EXECUTION_TIMEOUT", http.StatusGatewayTimeout
		if cause == io.EOF {
			code, status = "AGENT_OFFLINE", http.StatusServiceUnavailable
		}
		if result.Status != status || result.Code != code || result.Failure == nil || result.Failure.Outcome != "unknown" {
			t.Fatal(cause, result)
		}
	}
	result := inputResult(context.DeadlineExceeded)
	if result.Status != http.StatusGatewayTimeout || result.Code != "EXECUTION_TIMEOUT" || result.Failure.Outcome != "not_started" {
		t.Fatal(result)
	}
}

func TestInputDeliveryCommitExecutionTimeoutReturnsHTTP504(t *testing.T) {
	s := newCancelStateTestServer()
	hash, _ := bcrypt.GenerateFromPassword([]byte("dev-key"), bcrypt.MinCost)
	s.cfg.APIKeys = []APIKey{{Name: "test", KeyHash: string(hash), Capabilities: []string{"*"}}}
	conn := newDeterministicCancelTransport()
	ac := newAgentConn(conn)
	ac.initializeTransfers()
	s.agent, s.openapi = ac, map[string]any{}
	s.capabilities = map[string]protocol.CapabilityMetadata{"run": {
		DefinitionHash: strings.Repeat("a", 64), ExecutorKind: "file", EffectClass: "possible", InputTimeout: "1s", ExecutionTimeout: "100ms",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"optional": map[string]any{"type": "file", "max_bytes": "1B"}}},
		InputLimits: map[string]int64{"max_input_bytes": 1024, "max_json_depth": 32, "max_json_items": 100000, "max_input_files": 1, "max_input_file_bytes": 1, "max_total_input_file_bytes": 1},
	}}
	defer func() { conn.Close(); ac.disconnectPending() }()
	go ac.scheduler.Run()
	httpServer := httptest.NewServer(s.httpSrv.Handler)
	defer httpServer.Close()
	type httpResult struct {
		status int
		body   []byte
		err    error
	}
	done := make(chan httpResult, 1)
	go func() {
		req, _ := http.NewRequest("POST", httpServer.URL+"/api/v1/capabilities/run", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer dev-key")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- httpResult{err: err}
			return
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		done <- httpResult{resp.StatusCode, body, err}
	}()
	var begin protocol.Frame
	if err := json.Unmarshal(awaitTransportWrite(t, conn), &begin); err != nil || begin.Type != "request_begin" {
		t.Fatal(begin, err)
	}
	if err := ac.handleTransfer(protocol.MustJSON(protocol.Frame{Type: "request_accept", ID: begin.ID})); err != nil {
		t.Fatal(err)
	}
	var commit protocol.Frame
	if err := json.Unmarshal(awaitTransportWrite(t, conn), &commit); err != nil || commit.Type != "request_commit" || commit.ID != begin.ID {
		t.Fatal(commit, err)
	}
	// The transport has accepted and sent the real commit. Withhold only its
	// result so the production execution phase, rather than input, expires.
	select {
	case result := <-done:
		if result.err != nil || result.status != http.StatusGatewayTimeout || !strings.Contains(string(result.body), `"code":"EXECUTION_TIMEOUT"`) || !strings.Contains(string(result.body), `"outcome":"unknown"`) {
			t.Fatal(result.status, string(result.body), result.err)
		}
		t.Logf("sent request_commit id=%s; HTTP %d EXECUTION_TIMEOUT/unknown", commit.ID, result.status)
	case <-time.After(time.Second):
		t.Fatal("post-commit timeout did not return")
	}
	waitForCondition(t, time.Second, func() bool { return len(s.bodySlots) == 0 }, "commit input slot released")
}
