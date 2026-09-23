package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/viewlegacy/onprest/internal/protocol"
	"github.com/viewlegacy/onprest/internal/ws"
)

func TestCapabilityRateLimiterIntegerRefillBoundariesAndSampling(t *testing.T) {
	now := time.Now()
	bucket := newCapabilityRateLimiter(RateLimitDef{Requests: 60, Per: "1m", Burst: 10}, now)
	for i := 0; i < 10; i++ {
		if allowed, _ := bucket.take(now); !allowed {
			t.Fatalf("burst request %d was rejected", i+1)
		}
	}
	if allowed, sampled := bucket.take(now); allowed || !sampled {
		t.Fatalf("first exhausted result = allowed %t sampled %t", allowed, sampled)
	}
	if allowed, sampled := bucket.take(now); allowed || sampled {
		t.Fatalf("repeated exhausted result = allowed %t sampled %t", allowed, sampled)
	}

	now = now.Add(999 * time.Millisecond)
	if allowed, _ := bucket.take(now); allowed {
		t.Fatal("bucket refilled a whole token before one second")
	}
	now = now.Add(time.Millisecond)
	if allowed, _ := bucket.take(now); !allowed {
		t.Fatal("bucket did not refill one token after one second")
	}
	if allowed, _ := bucket.take(now); allowed {
		t.Fatal("one-second refill admitted more than one token")
	}

	// A backward wall-clock value must not create credit or move the monotonic
	// refill anchor backward.
	if allowed, _ := bucket.take(now.Add(-time.Hour)); allowed {
		t.Fatal("backward clock movement refilled the bucket")
	}
	now = now.Add(24 * time.Hour)
	for i := 0; i < 10; i++ {
		if allowed, _ := bucket.take(now); !allowed {
			t.Fatalf("long-idle saturation request %d was rejected", i+1)
		}
	}
	if allowed, sampled := bucket.take(now); allowed || !sampled {
		t.Fatalf("sampling did not reopen after interval: allowed=%t sampled=%t", allowed, sampled)
	}
}

func TestCapabilityRateLimiterConcurrentAdmissionIsAtomic(t *testing.T) {
	now := time.Now()
	bucket := newCapabilityRateLimiter(RateLimitDef{Requests: 1, Per: "1h", Burst: 7}, now)
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := bucket.take(now); ok {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := allowed.Load(); got != 7 {
		t.Fatalf("concurrent admissions = %d, want 7", got)
	}
}

func TestCapabilityRateLimiterRepresentableExtremeValues(t *testing.T) {
	definition := RateLimitDef{Requests: int64(^uint64(0) >> 1), Per: "1ns", Burst: int64(^uint64(0) >> 1)}
	if err := definition.lint(); err != nil {
		t.Fatalf("representable extreme definition was rejected: %v", err)
	}
	now := time.Now()
	bucket := newCapabilityRateLimiter(definition, now)
	if allowed, _ := bucket.take(now); !allowed {
		t.Fatal("full extreme bucket rejected its initial token")
	}
	if allowed, _ := bucket.take(now.Add(time.Nanosecond)); !allowed {
		t.Fatal("extreme refill did not saturate safely")
	}
}

func TestRunnerRateLimitAdmissionOrderingIsolationAndRestart(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	rate := &RateLimitDef{Requests: 1, Per: "1h", Burst: 1}
	cf := &CapabilityFile{
		Database: DatabaseDef{Driver: "postgres"},
		Capabilities: map[string]CapabilityDef{
			"limited": {
				SQL: "select :secret as value",
				Params: map[string]ParamDef{
					"secret": {Type: "string", Required: true},
				},
				Policy:    PolicyDef{Timeout: "1s", MaxBytes: "1KB", RateLimit: rate},
				Operation: sqlOperationSelect,
			},
			"separate": {
				SQL: "select 1 as value", Policy: PolicyDef{Timeout: "1s", MaxBytes: "1KB", RateLimit: rate}, Operation: sqlOperationSelect,
			},
			"unlimited": {
				SQL: "select 1 as value", Policy: PolicyDef{Timeout: "1s", MaxBytes: "1KB"}, Operation: sqlOperationSelect,
			},
		},
	}
	r := &Runner{cf: cf, caps: cf.ByName(), clock: clock, detailLog: &bytes.Buffer{}}
	r.rateLimits = newCapabilityRateLimiters(cf, clock)
	if len(r.rateLimits) != 2 {
		t.Fatalf("rate limiter map size = %d, want 2 fixed configured capabilities", len(r.rateLimits))
	}

	invalid := protocol.Request{ID: "invalid", Capability: "limited", Params: map[string]any{"secret": 42}}
	if _, rejected := r.admit(invalid); rejected == nil || rejected.Error.Code != "AGENT_VALIDATION_FAILED" {
		t.Fatalf("invalid admission response = %#v", rejected)
	}
	unknown := protocol.Request{ID: "unknown", Capability: "hidden-name", Params: map[string]any{}}
	if _, rejected := r.admit(unknown); rejected == nil || rejected.Error.Code != "GATEWAY_CAPABILITY_NOT_FOUND" {
		t.Fatalf("unknown admission response = %#v", rejected)
	}
	meta := protocol.Request{ID: "meta", Capability: "meta"}
	if prepared, rejected := r.admit(meta); rejected != nil || !prepared.metadata {
		t.Fatalf("meta admission = prepared %#v rejected %#v", prepared, rejected)
	}

	valid := protocol.Request{ID: "valid", Capability: "limited", Params: map[string]any{"secret": "TOP-SECRET-PARAM"}}
	if prepared, rejected := r.admit(valid); rejected != nil || prepared == nil {
		t.Fatalf("first valid admission = prepared %#v rejected %#v", prepared, rejected)
	}
	if _, rejected := r.admit(valid); rejected == nil || rejected.Error.Code != "AGENT_RATE_LIMITED" || rejected.Error.Message != rateLimitedMessage {
		t.Fatalf("exhausted admission response = %#v", rejected)
	}
	if prepared, rejected := r.admit(protocol.Request{ID: "separate", Capability: "separate"}); rejected != nil || prepared == nil {
		t.Fatalf("separate capability shared a bucket: prepared %#v rejected %#v", prepared, rejected)
	}
	for i := 0; i < 20; i++ {
		if prepared, rejected := r.admit(protocol.Request{ID: "unlimited", Capability: "unlimited"}); rejected != nil || prepared == nil {
			t.Fatalf("unlimited admission %d = prepared %#v rejected %#v", i, prepared, rejected)
		}
	}
	if len(r.rateLimits) != 2 {
		t.Fatalf("hostile IDs changed limiter map size to %d", len(r.rateLimits))
	}
	logText := r.detailLog.(*bytes.Buffer).String()
	if bytes.Contains([]byte(logText), []byte("TOP-SECRET-PARAM")) {
		t.Fatalf("secret param reached rejection log: %s", logText)
	}
	if got := bytes.Count([]byte(logText), []byte("AGENT_RATE_LIMITED")); got != 1 {
		t.Fatalf("sampled rejection logs = %d, want 1; logs=%s", got, logText)
	}

	// A new Runner represents an Agent restart and starts again at burst.
	restarted := &Runner{cf: cf, caps: cf.ByName(), clock: clock}
	restarted.rateLimits = newCapabilityRateLimiters(cf, clock)
	if prepared, rejected := restarted.admit(valid); rejected != nil || prepared == nil {
		t.Fatalf("restart did not restore burst: prepared %#v rejected %#v", prepared, rejected)
	}
}

func TestFailedExecutionDoesNotReturnRateLimitToken(t *testing.T) {
	state := &mutationDriverState{execErr: errors.New("database rejected execution")}
	r := mutationRunner(t, state)
	now := time.Now()
	r.clock = func() time.Time { return now }
	rate := &RateLimitDef{Requests: 1, Per: "1h", Burst: 1}
	readonly := false
	cap := CapabilityDef{
		SQL: "update t set v=1", Operation: sqlOperationUpdate,
		Policy: PolicyDef{Readonly: &readonly, Timeout: "1s", MaxBytes: "1KB", RateLimit: rate},
	}
	r.cf.Capabilities = map[string]CapabilityDef{"mutate": cap}
	r.caps = r.cf.ByName()
	r.rateLimits = newCapabilityRateLimiters(r.cf, r.clock)
	req := protocol.Request{ID: "first", Capability: "mutate", Params: map[string]any{}}
	if resp := r.handle(context.Background(), req); resp.Error == nil || resp.Error.Code != "AGENT_QUERY_FAILED" {
		t.Fatalf("first failed execution response = %#v", resp)
	}
	req.ID = "second"
	if resp := r.handle(context.Background(), req); resp.Error == nil || resp.Error.Code != "AGENT_RATE_LIMITED" {
		t.Fatalf("failed execution returned its token: %#v", resp)
	}
}

func TestPrepareRejectionsPreserveWorkerQueueOrderingCancellationAndBusy(t *testing.T) {
	type harness struct {
		state     *mutationDriverState
		client    *ws.Conn
		serveDone <-chan error
		cleanup   func()
	}
	newHarness := func(t *testing.T) harness {
		t.Helper()
		state := &mutationDriverState{rows: 1, execStarted: make(chan struct{}), execRelease: make(chan struct{})}
		r := mutationRunner(t, state)
		maxConcurrent := 1
		r.cf.Runtime.MaxConcurrentRequests = &maxConcurrent
		now := time.Now()
		r.clock = func() time.Time { return now }
		readonly := false
		rate := &RateLimitDef{Requests: 1, Per: "1h", Burst: 1}
		r.cf.Capabilities = map[string]CapabilityDef{
			"slow": {
				SQL: "update t set v=1", Operation: sqlOperationUpdate,
				Policy: PolicyDef{Readonly: &readonly, Timeout: "5s", MaxBytes: "1KB"},
			},
			"needs-value": {
				SQL: "update t set v=:value", Operation: sqlOperationUpdate,
				Params: map[string]ParamDef{"value": {Type: "string", Required: true}},
				Policy: PolicyDef{Readonly: &readonly, Timeout: "5s", MaxBytes: "1KB"},
			},
			"limited": {
				SQL: "update t set v=1", Operation: sqlOperationUpdate,
				Policy: PolicyDef{Readonly: &readonly, Timeout: "5s", MaxBytes: "1KB", RateLimit: rate},
			},
		}
		r.caps = r.cf.ByName()
		r.rateLimits = newCapabilityRateLimiters(r.cf, r.clock)
		if allowed, _ := r.rateLimits["limited"].take(now); !allowed {
			t.Fatal("failed to exhaust ordering-test rate limiter")
		}
		client, serveDone, cleanup := dialRunnerWebSocket(t, r)
		return harness{state: state, client: client, serveDone: serveDone, cleanup: cleanup}
	}
	writeRequest := func(t *testing.T, client *ws.Conn, req protocol.Request) {
		t.Helper()
		if err := client.WriteText(protocol.MustJSON(req)); err != nil {
			t.Fatal(err)
		}
	}
	readResponse := func(t *testing.T, client *ws.Conn) protocol.Response {
		t.Helper()
		msg, err := client.ReadText()
		if err != nil {
			t.Fatal(err)
		}
		var response protocol.Response
		if err := json.Unmarshal(msg, &response); err != nil {
			t.Fatal(err)
		}
		return response
	}
	startSlow := func(t *testing.T, h harness) {
		t.Helper()
		writeRequest(t, h.client, protocol.Request{ID: "running", Capability: "slow", Params: map[string]any{}})
		select {
		case <-h.state.execStarted:
		case <-time.After(time.Second):
			t.Fatal("running request did not reach DB barrier")
		}
	}
	assertNoResponse := func(t *testing.T, client *ws.Conn) {
		t.Helper()
		if err := client.SetReadDeadline(time.Now().Add(75 * time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		_, err := client.ReadText()
		var netErr net.Error
		if err == nil || !errors.As(err, &netErr) || !netErr.Timeout() {
			t.Fatalf("read before worker release error = %v, want timeout", err)
		}
		if err := client.SetReadDeadline(time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	syncCancel := func(t *testing.T, client *ws.Conn, id string) {
		t.Helper()
		pong := make(chan struct{}, 1)
		client.SetPongHandler(func() {
			select {
			case pong <- struct{}{}:
			default:
			}
		})
		if err := client.WriteText(protocol.MustJSON(protocol.CancelRequest{Type: "cancel", ID: id})); err != nil {
			t.Fatal(err)
		}
		if err := client.WritePing([]byte("cancel-processed")); err != nil {
			t.Fatal(err)
		}
		assertNoResponse(t, client)
		select {
		case <-pong:
		default:
			t.Fatal("cancel/ping control frames were not processed before read deadline")
		}
	}
	assertDBExecutions := func(t *testing.T, state *mutationDriverState, want int) {
		t.Helper()
		state.mu.Lock()
		execCalls := 0
		for _, call := range state.calls {
			if call == "exec" {
				execCalls++
			}
		}
		state.mu.Unlock()
		if execCalls != want {
			t.Fatalf("database executions = %d, want %d", execCalls, want)
		}
	}
	finish := func(t *testing.T, h harness) {
		t.Helper()
		if err := h.client.Close(); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-h.serveDone:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("serveConn did not stop")
		}
	}
	prepareRejections := []struct {
		name string
		req  protocol.Request
		code string
	}{
		{name: "unknown", req: protocol.Request{ID: "rejected", Capability: "hidden"}, code: "GATEWAY_CAPABILITY_NOT_FOUND"},
		{name: "invalid", req: protocol.Request{ID: "rejected", Capability: "needs-value", Params: map[string]any{"value": 42}}, code: "AGENT_VALIDATION_FAILED"},
	}

	for _, tc := range prepareRejections {
		t.Run(tc.name+" waits behind running request", func(t *testing.T) {
			h := newHarness(t)
			defer h.cleanup()
			startSlow(t, h)
			writeRequest(t, h.client, tc.req)
			assertNoResponse(t, h.client)
			close(h.state.execRelease)
			if response := readResponse(t, h.client); response.ID != "running" || response.Error != nil {
				t.Fatalf("running response = %#v", response)
			}
			if response := readResponse(t, h.client); response.ID != tc.req.ID || response.Error == nil || response.Error.Code != tc.code {
				t.Fatalf("queued rejection response = %#v, want %s", response, tc.code)
			}
			finish(t, h)
		})

		t.Run(tc.name+" queued cancel drops response", func(t *testing.T) {
			h := newHarness(t)
			defer h.cleanup()
			startSlow(t, h)
			writeRequest(t, h.client, tc.req)
			syncCancel(t, h.client, tc.req.ID)
			close(h.state.execRelease)
			if response := readResponse(t, h.client); response.ID != "running" || response.Error != nil {
				t.Fatalf("running response = %#v", response)
			}
			assertNoResponse(t, h.client)
			assertDBExecutions(t, h.state, 1)
			finish(t, h)
		})
	}

	t.Run("prepare rejection queue full returns busy", func(t *testing.T) {
		h := newHarness(t)
		defer h.cleanup()
		startSlow(t, h)
		writeRequest(t, h.client, protocol.Request{ID: "queued", Capability: "hidden"})
		writeRequest(t, h.client, protocol.Request{ID: "busy", Capability: "needs-value", Params: map[string]any{"value": 42}})
		if response := readResponse(t, h.client); response.ID != "busy" || response.Error == nil || response.Error.Code != "AGENT_BUSY" {
			t.Fatalf("queue-full rejection response = %#v", response)
		}
		syncCancel(t, h.client, "queued")
		close(h.state.execRelease)
		if response := readResponse(t, h.client); response.ID != "running" || response.Error != nil {
			t.Fatalf("running response = %#v", response)
		}
		assertNoResponse(t, h.client)
		assertDBExecutions(t, h.state, 1)
		finish(t, h)
	})

	t.Run("rate rejection bypasses running worker", func(t *testing.T) {
		h := newHarness(t)
		defer h.cleanup()
		startSlow(t, h)
		writeRequest(t, h.client, protocol.Request{ID: "rate", Capability: "limited", Params: map[string]any{}})
		if err := h.client.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if response := readResponse(t, h.client); response.ID != "rate" || response.Error == nil || response.Error.Code != "AGENT_RATE_LIMITED" {
			t.Fatalf("immediate rate response = %#v", response)
		}
		if err := h.client.SetReadDeadline(time.Time{}); err != nil {
			t.Fatal(err)
		}
		close(h.state.execRelease)
		if response := readResponse(t, h.client); response.ID != "running" || response.Error != nil {
			t.Fatalf("running response = %#v", response)
		}
		finish(t, h)
	})
}

func TestAgentWireIDBoundariesPreserveGatewayULID(t *testing.T) {
	boundaryID := strings.Repeat("i", agentMaxWireIDBytes)
	oversizedID := boundaryID + "x"
	gatewayULID := "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	if !validAgentWireID(boundaryID) {
		t.Fatal("ID at the byte limit was rejected")
	}
	if validAgentWireID(oversizedID) {
		t.Fatal("ID one byte over the limit was accepted")
	}
	if validAgentWireID("") {
		t.Fatal("empty ID was accepted")
	}
	if len(gatewayULID) != 26 || !validAgentWireID(gatewayULID) {
		t.Fatalf("production Gateway ULID length/limit = %d/%t", len(gatewayULID), validAgentWireID(gatewayULID))
	}

	r := mutationRunner(t, &mutationDriverState{})
	maxConcurrent := 1
	r.cf.Runtime.MaxConcurrentRequests = &maxConcurrent
	now := time.Now()
	r.clock = func() time.Time { return now }
	rate := &RateLimitDef{Requests: 1, Per: "1h", Burst: 1}
	readonly := false
	r.cf.Capabilities = map[string]CapabilityDef{
		"mutate": {
			SQL: "update t set v=1", Operation: sqlOperationUpdate,
			Policy: PolicyDef{Readonly: &readonly, Timeout: "5s", MaxBytes: "1KB", RateLimit: rate},
		},
	}
	r.caps = r.cf.ByName()
	r.rateLimits = newCapabilityRateLimiters(r.cf, r.clock)
	if allowed, _ := r.rateLimits["mutate"].take(now); !allowed {
		t.Fatal("failed to exhaust boundary-test rate limiter")
	}
	client, serveDone, cleanup := dialRunnerWebSocket(t, r)
	defer cleanup()

	// The exact-limit ID is accepted for both request and control envelopes.
	if err := client.WriteText(protocol.MustJSON(protocol.Request{ID: boundaryID, Capability: "meta"})); err != nil {
		t.Fatal(err)
	}
	msg, err := client.ReadText()
	if err != nil {
		t.Fatal(err)
	}
	var response protocol.Response
	if err := json.Unmarshal(msg, &response); err != nil {
		t.Fatal(err)
	}
	if response.ID != boundaryID || response.Error != nil {
		t.Fatalf("exact-limit response = %#v", response)
	}
	if err := client.WriteText(protocol.MustJSON(protocol.CancelRequest{Type: "cancel", ID: boundaryID})); err != nil {
		t.Fatal(err)
	}
	// Synchronous invalid and rate-limit responses can echo at most the bounded
	// ID. Exercise both paths at the exact limit while the peer is reading.
	if err := client.WriteText(protocol.MustJSON(protocol.Request{ID: boundaryID, Capability: "mutate"})); err != nil {
		t.Fatal(err)
	}
	msg, err = client.ReadText()
	if err != nil {
		t.Fatal(err)
	}
	response = protocol.Response{}
	if err := json.Unmarshal(msg, &response); err != nil {
		t.Fatal(err)
	}
	if response.ID != boundaryID || response.Error == nil || response.Error.Code != "AGENT_RATE_LIMITED" {
		t.Fatalf("exact-limit rate response = %#v", response)
	}
	if err := client.WriteText(protocol.MustJSON(protocol.Request{ID: boundaryID, Capability: "unknown"})); err != nil {
		t.Fatal(err)
	}
	msg, err = client.ReadText()
	if err != nil {
		t.Fatal(err)
	}
	response = protocol.Response{}
	if err := json.Unmarshal(msg, &response); err != nil {
		t.Fatal(err)
	}
	if response.ID != boundaryID || response.Error == nil || response.Error.Code != "GATEWAY_CAPABILITY_NOT_FOUND" {
		t.Fatalf("exact-limit invalid response = %#v", response)
	}
	if err := client.WriteText(protocol.MustJSON(protocol.Request{ID: gatewayULID, Capability: "meta"})); err != nil {
		t.Fatal(err)
	}
	msg, err = client.ReadText()
	if err != nil {
		t.Fatal(err)
	}
	response = protocol.Response{}
	if err := json.Unmarshal(msg, &response); err != nil {
		t.Fatal(err)
	}
	if response.ID != gatewayULID || response.Error != nil {
		t.Fatalf("Gateway ULID response = %#v", response)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serveConn did not stop after valid boundary requests")
	}
}

func TestAgentWebSocketRejectsInvalidOrOversizedIDsBeforeAdmissionAndResponseQueue(t *testing.T) {
	state := &mutationDriverState{rows: 1}
	r := mutationRunner(t, state)
	maxConcurrent := 1
	r.cf.Runtime.MaxConcurrentRequests = &maxConcurrent
	now := time.Now()
	r.clock = func() time.Time { return now }
	rate := &RateLimitDef{Requests: 1, Per: "1h", Burst: 1}
	readonly := false
	r.cf.Capabilities = map[string]CapabilityDef{
		"mutate": {
			SQL: "update t set v=1", Operation: sqlOperationUpdate,
			Policy: PolicyDef{Readonly: &readonly, Timeout: "5s", MaxBytes: "1KB", RateLimit: rate},
		},
	}
	r.caps = r.cf.ByName()
	r.rateLimits = newCapabilityRateLimiters(r.cf, r.clock)
	var detailLog bytes.Buffer
	r.detailLog = &detailLog

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	assertClosedWithoutResponse := func(t *testing.T, payload []byte) {
		t.Helper()
		client, serveDone, cleanup := dialRunnerWebSocket(t, r)
		defer cleanup()
		if err := client.WriteText(payload); err != nil {
			t.Fatal(err)
		}
		// Do not read while the Agent handles the hostile frame. A response
		// enqueue would therefore be observable before the connection closes.
		select {
		case err := <-serveDone:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("invalid or oversized ID did not close the connection")
		}
		if err := client.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if msg, err := client.ReadText(); err == nil {
			t.Fatalf("invalid or oversized ID produced a response instead of closing: %.128q", msg)
		}
	}

	oneByteOver := strings.Repeat("i", agentMaxWireIDBytes+1)
	t.Run("empty request ID", func(t *testing.T) {
		assertClosedWithoutResponse(t, protocol.MustJSON(protocol.Request{Capability: "mutate"}))
	})
	t.Run("empty control ID", func(t *testing.T) {
		assertClosedWithoutResponse(t, protocol.MustJSON(protocol.CancelRequest{Type: "cancel"}))
	})
	t.Run("request one byte over", func(t *testing.T) {
		assertClosedWithoutResponse(t, protocol.MustJSON(protocol.Request{ID: oneByteOver, Capability: "mutate"}))
	})
	t.Run("control one byte over", func(t *testing.T) {
		assertClosedWithoutResponse(t, protocol.MustJSON(protocol.CancelRequest{Type: "cancel", ID: oneByteOver}))
	})
	t.Run("large non-reading peer", func(t *testing.T) {
		// Exercise one near-maximum (16 MiB) WebSocket message, rather than
		// reproducing the vulnerable pattern of retaining hundreds of them.
		largeID := strings.Repeat("h", (16<<20)-1024)
		assertClosedWithoutResponse(t, protocol.MustJSON(protocol.Request{ID: largeID, Capability: "mutate"}))
	})

	limiter := r.rateLimits["mutate"]
	limiter.mu.Lock()
	credit, capacity := limiter.credit, limiter.capacity
	rejectionLogged := limiter.rejectionLogged
	limiter.mu.Unlock()
	if credit != capacity || rejectionLogged {
		t.Fatalf("invalid or oversized IDs reached rate admission: credit/capacity=%d/%d rejection_logged=%t", credit, capacity, rejectionLogged)
	}
	if len(r.rateLimits) != 1 {
		t.Fatalf("limiter map grew to %d entries", len(r.rateLimits))
	}
	state.mu.Lock()
	calls := append([]string(nil), state.calls...)
	state.mu.Unlock()
	if len(calls) != 0 {
		t.Fatalf("invalid or oversized IDs reached the database: calls=%v", calls)
	}
	if detailLog.Len() != 0 {
		t.Fatalf("invalid or oversized ID reached detail log: bytes=%d", detailLog.Len())
	}
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if after.HeapAlloc > before.HeapAlloc+(4<<20) {
		t.Fatalf("live heap grew by %d bytes after invalid or oversized IDs", after.HeapAlloc-before.HeapAlloc)
	}
}

func dialRunnerWebSocket(t *testing.T, r *Runner) (*ws.Conn, <-chan error, func()) {
	t.Helper()
	serveDone := make(chan error, 1)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		conn, err := ws.Accept(w, req)
		if err != nil {
			serveDone <- err
			return
		}
		r.serveConn(context.Background(), conn)
		serveDone <- nil
	}))
	client, err := ws.Dial(time.Second, "ws"+strings.TrimPrefix(httpServer.URL, "http"), nil)
	if err != nil {
		httpServer.Close()
		t.Fatal(err)
	}
	cleanup := func() {
		_ = client.Close()
		httpServer.Close()
	}
	return client, serveDone, cleanup
}

func TestRateLimitedWebSocketQueueFullCancellationKeepsStateMemoryAndControlBounded(t *testing.T) {
	state := &mutationDriverState{rows: 1, execStarted: make(chan struct{}), execRelease: make(chan struct{})}
	r := mutationRunner(t, state)
	maxConcurrent := 1
	r.cf.Runtime.MaxConcurrentRequests = &maxConcurrent
	now := time.Now()
	r.clock = func() time.Time { return now }
	rate := &RateLimitDef{Requests: 1, Per: "1h", Burst: 3}
	readonly := false
	cap := CapabilityDef{
		SQL: "update t set v=1", Operation: sqlOperationUpdate,
		Policy: PolicyDef{Readonly: &readonly, Timeout: "5s", MaxBytes: "1KB", RateLimit: rate},
	}
	r.cf.Capabilities = map[string]CapabilityDef{"mutate": cap}
	r.caps = r.cf.ByName()
	r.rateLimits = newCapabilityRateLimiters(r.cf, r.clock)

	serveDone := make(chan struct{})
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		conn, err := ws.Accept(w, req)
		if err != nil {
			t.Errorf("accept websocket: %v", err)
			return
		}
		r.serveConn(context.Background(), conn)
		close(serveDone)
	}))
	defer httpServer.Close()
	client, err := ws.Dial(time.Second, "ws"+strings.TrimPrefix(httpServer.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	writeRequest := func(id string) {
		t.Helper()
		if err := client.WriteText(protocol.MustJSON(protocol.Request{ID: id, Capability: "mutate", Params: map[string]any{}})); err != nil {
			t.Fatal(err)
		}
	}
	readResponse := func() protocol.Response {
		t.Helper()
		msg, err := client.ReadText()
		if err != nil {
			t.Fatal(err)
		}
		var response protocol.Response
		if err := json.Unmarshal(msg, &response); err != nil {
			t.Fatal(err)
		}
		return response
	}

	if got := agentResponseQueueSize(maxConcurrent); got != agentMinResponseQueueSize {
		t.Fatalf("response queue size = %d, want fixed finite minimum %d", got, agentMinResponseQueueSize)
	}
	if got := agentResponseQueueSize(agentMinResponseQueueSize); got != agentMinResponseQueueSize*2+1 {
		t.Fatalf("scaled response queue size = %d, want %d", got, agentMinResponseQueueSize*2+1)
	}

	// Hold the first admitted request inside the DB driver. The single worker is
	// then occupied and the channel with capacity one deterministically holds the
	// second request. The third request consumes a token before hitting the full
	// queue and returning AGENT_BUSY; the fourth proves that token is not returned.
	writeRequest("running")
	select {
	case <-state.execStarted:
	case <-time.After(time.Second):
		t.Fatal("running request did not reach the DB barrier")
	}
	writeRequest("queued")
	writeRequest("busy")
	writeRequest("after-busy")

	var pongCount atomic.Int64
	client.SetPongHandler(func() { pongCount.Add(1) })
	if err := client.WritePing([]byte("queue-full-responsive")); err != nil {
		t.Fatal(err)
	}
	if err := client.WriteText(protocol.MustJSON(protocol.CancelRequest{Type: "cancel", ID: "queued"})); err != nil {
		t.Fatal(err)
	}
	if err := client.WriteText(protocol.MustJSON(protocol.CancelRequest{Type: "cancel", ID: "running"})); err != nil {
		t.Fatal(err)
	}

	responses := make(map[string]protocol.Response, 3)
	for range 3 {
		response := readResponse()
		responses[response.ID] = response
	}
	if response := responses["busy"]; response.Error == nil || response.Error.Code != "AGENT_BUSY" {
		t.Fatalf("queue-full response = %#v", response)
	}
	if response := responses["after-busy"]; response.Error == nil || response.Error.Code != "AGENT_RATE_LIMITED" {
		t.Fatalf("queue-full token was returned: %#v", response)
	}
	if response := responses["running"]; response.Error == nil || response.Error.Code != "AGENT_QUERY_TIMEOUT" {
		t.Fatalf("running cancellation response = %#v", response)
	}
	if _, ok := responses["queued"]; ok {
		t.Fatalf("queued cancellation unexpectedly produced a response: %#v", responses["queued"])
	}
	if got := pongCount.Load(); got != 1 {
		t.Fatalf("queue-full pong responses = %d, want 1", got)
	}

	state.mu.Lock()
	initialExecCalls := 0
	for _, call := range state.calls {
		if call == "exec" {
			initialExecCalls++
		}
	}
	state.mu.Unlock()
	if initialExecCalls != 1 {
		t.Fatalf("queued cancellation reached DB: exec calls=%d, want 1", initialExecCalls)
	}
	close(state.execRelease)

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	const rejectedRequests = 2000
	for i := 0; i < rejectedRequests; i++ {
		id := "rejected-" + time.Duration(i).String()
		writeRequest(id)
		if i%100 == 0 {
			if err := client.WriteText(protocol.MustJSON(protocol.CancelRequest{Type: "cancel", ID: id})); err != nil {
				t.Fatal(err)
			}
			if err := client.WritePing([]byte("still-responsive")); err != nil {
				t.Fatal(err)
			}
		}
		response := readResponse()
		if response.ID != id || response.Error == nil || response.Error.Code != "AGENT_RATE_LIMITED" {
			t.Fatalf("rejected response %d = %#v", i, response)
		}
	}
	if got, want := pongCount.Load(), int64(1+rejectedRequests/100); got != want {
		t.Fatalf("pong responses = %d, want %d", got, want)
	}
	if len(r.rateLimits) != 1 {
		t.Fatalf("rate limiter map grew to %d entries", len(r.rateLimits))
	}
	state.mu.Lock()
	execCalls := 0
	for _, call := range state.calls {
		if call == "exec" {
			execCalls++
		}
	}
	state.mu.Unlock()
	if execCalls != 1 {
		t.Fatalf("database executions = %d, want only the running admission", execCalls)
	}
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if after.HeapAlloc > before.HeapAlloc+(4<<20) {
		t.Fatalf("live heap grew by %d bytes after bounded flood", after.HeapAlloc-before.HeapAlloc)
	}

	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-serveDone:
	case <-time.After(2 * time.Second):
		t.Fatal("serveConn did not stop after bounded flood")
	}
}
