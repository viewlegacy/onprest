package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/viewlegacy/onprest/internal/protocol"
	"github.com/viewlegacy/onprest/internal/ws"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTransferLateInputControlDiscardAndBoundedRecentExchanges(t *testing.T) {
	ac := newAgentConn(nil)
	ac.initializeTransfers()
	defer ac.transferCancel()
	defer ac.transfers.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, release, e := ac.exchange(deadlineOf(ctx), "completed")
	if e != nil {
		t.Fatal(e)
	}
	release()
	if e = ac.handleTransfer(protocol.MustJSON(protocol.Frame{Type: "request_accept", ID: "completed"})); e != nil {
		t.Fatal("late accepted control disconnected unrelated requests", e)
	}
	if e = ac.handleTransfer(protocol.MustJSON(protocol.Frame{Type: "request_accept", ID: "unknown"})); e != ws.ErrProtocol {
		t.Fatal("unknown control accepted", e)
	}
	for i := 1; i < 4096; i++ {
		_, release, e := ac.exchange(deadlineOf(ctx), fmt.Sprintf("id%d", i))
		if e != nil {
			t.Fatal(i, e)
		}
		release()
	}
	if _, _, e := ac.exchange(deadlineOf(ctx), "overflow"); e == nil || len(ac.transferRecent) != 4096 || len(ac.transferCalls) != 0 {
		t.Fatal("unbounded recent exchange table", e)
	}
}

func TestTransferSchedulerNormalCancelQueuedDuringAndAfterSend(t *testing.T) {
	for _, phase := range []string{"queued", "during", "after"} {
		t.Run(phase, func(t *testing.T) {
			s := newCancelStateTestServer()
			conn := newDeterministicCancelTransport()
			ac := newAgentConn(conn)
			ac.initializeTransfers()
			s.agent = ac
			seedVersionedTestMetadata(s)
			defer func() { conn.Close(); ac.disconnectPending() }()
			started, unblock := make(chan struct{}), make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(unblock) }) }
			defer release()
			if phase == "queued" {
				ac.scheduler.Post(ac.transferCtx, true, "", func() error { close(started); <-unblock; return nil })
			}
			if phase == "during" {
				conn.blockWrite[0] = unblock
			}
			go ac.scheduler.Run()
			go s.writeAgent(ac)
			if phase == "queued" {
				<-started
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			returned := make(chan agentCallResult, 1)
			go func() { returned <- s.callAgent(ctx, "mutate", nil) }()
			var request []byte
			if phase == "queued" {
				waitForCondition(t, time.Second, func() bool {
					ac.mu.Lock()
					defer ac.mu.Unlock()
					for _, state := range ac.sendStates {
						if state.phase == "sending" {
							return true
						}
					}
					return false
				}, "scheduler queued request")
			} else {
				request = awaitTransportWrite(t, conn)
				if phase == "after" {
					var frame protocol.Request
					json.Unmarshal(request, &frame)
					waitForSendPhase(t, ac, frame.ID, "sent")
				}
			}
			cancel()
			if result := <-returned; result.Status != http.StatusGatewayTimeout {
				t.Fatal(result)
			}
			release()
			if phase != "queued" {
				cancelFrame := awaitTransportWrite(t, conn)
				assertRequestThenCancel(t, request, cancelFrame)
			}
			waitForAgentCallStateEmpty(t, ac)
			select {
			case <-conn.closed:
				t.Fatal("normal cancellation closed healthy connection")
			default:
			}
			if phase == "queued" && len(conn.payloads()) != 0 {
				t.Fatal("queued canceled request reached network")
			}
		})
	}
}

func TestTransferInputCancelOrdersBeginBeforeCancelAndSkipsQueuedBegin(t *testing.T) {
	for _, phase := range []string{"queued", "during", "after", "cancel_error"} {
		t.Run(phase, func(t *testing.T) {
			s := newCancelStateTestServer()
			conn := newDeterministicCancelTransport()
			ac := newAgentConn(conn)
			ac.initializeTransfers()
			s.agent = ac
			s.openapi = map[string]any{}
			s.capabilities = map[string]protocol.CapabilityMetadata{"run": {DefinitionHash: "hash", InputTimeout: "2s", ExecutionTimeout: "1s"}}
			defer func() { conn.Close(); ac.disconnectPending() }()
			entered, unblock := make(chan struct{}), make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(unblock) }) }
			defer release()
			if phase == "queued" {
				ac.scheduler.Post(ac.transferCtx, true, "", func() error { close(entered); <-unblock; return nil })
			}
			if phase == "during" {
				conn.blockWrite[0] = unblock
			}
			if phase == "cancel_error" {
				conn.writeErrors[1] = io.ErrClosedPipe
			}
			go ac.scheduler.Run()
			if phase == "queued" {
				<-entered
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				_, e := s.TransferInput(ctx, protocol.Frame{Type: "request_begin", ID: "begin-cancel", Capability: "run", CallerBinding: "caller", DefinitionHash: "hash", InputDeadline: time.Now().Add(time.Second)}, map[string]any{}, nil)
				result <- e
			}()
			var begin []byte
			if phase == "queued" {
				waitForCondition(t, time.Second, func() bool { ac.mu.Lock(); defer ac.mu.Unlock(); return ac.transferCalls["begin-cancel"] != nil }, "begin registered")
			} else {
				begin = awaitTransportWrite(t, conn)
			}
			cancel()
			release()
			if e := <-result; e != context.Canceled {
				t.Fatal(e)
			}
			if _, e := s.TransferInput(context.Background(), protocol.Frame{Type: "request_begin", ID: "begin-cancel", Capability: "run", CallerBinding: "caller", DefinitionHash: "hash", InputDeadline: time.Now().Add(time.Second)}, map[string]any{}, nil); e == nil || e.Error() != "CONFLICT" {
				t.Fatal("same ID begin reused recent", e)
			}
			if phase != "queued" {
				raw := awaitTransportWrite(t, conn)
				var first protocol.Frame
				var last protocol.CancelRequest
				if json.Unmarshal(begin, &first) != nil || json.Unmarshal(raw, &last) != nil || first.Type != "request_begin" || last.Type != "cancel" || first.ID != last.ID {
					t.Fatal("begin/cancel order", string(begin), string(raw))
				}
			}
			if phase == "cancel_error" {
				select {
				case <-conn.closed:
				default:
					t.Fatal("failed cancel left connection live")
				}
			}
			ac.mu.Lock()
			live := len(ac.transferCalls)
			ac.mu.Unlock()
			if live != 0 {
				t.Fatal("exchange not released")
			}
			if phase == "queued" {
				select {
				case raw := <-conn.writeEvents:
					t.Fatal("queued begin or cancel sent", string(raw))
				default:
				}
			}
		})
	}
}

type observedSignedWire struct {
	*ws.Conn
	open chan protocol.Frame
	at   chan struct{}
	once sync.Once
}

func (w *observedSignedWire) WriteTextWithDeadlineStart(payload []byte, timeout time.Duration, start func() error) error {
	var f protocol.Frame
	json.Unmarshal(payload, &f)
	if f.Type == "transfer_open" {
		w.open <- f
		w.once.Do(func() { close(w.at) })
	}
	return w.Conn.WriteTextWithDeadlineStart(payload, timeout, start)
}
func TestTransferSignedGatewayOpenCannotGrantBeforeActualWriteLock(t *testing.T) {
	s, _, _ := testServer(t)
	captured := make(chan *observedSignedWire, 1)
	s.agentWire = func(conn *ws.Conn) textConn {
		w := &observedSignedWire{Conn: conn, open: make(chan protocol.Frame, 1), at: make(chan struct{})}
		captured <- w
		return w
	}
	server := httptest.NewServer(s.httpSrv.Handler)
	defer server.Close()
	peer := dialSignedTestAgent(t, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/agent", "write-boundary")
	defer peer.Close()
	wire := <-captured
	// Complete production metadata publication before testing the file exchange.
	raw, e := peer.ReadText()
	var meta protocol.Request
	if e != nil || json.Unmarshal(raw, &meta) != nil {
		t.Fatal(e)
	}
	response := versionedTestResponse(meta, protocol.Response{ID: meta.ID, Result: json.RawMessage(`{"data":{"openapi":"3.1.0","paths":{"/api/v1/capabilities/get_customer":{"post":{"x-onprest-capability":"get_customer"}}}},"response_kinds":{"get_customer":"select"}}`)})
	peer.WriteText(protocol.MustJSON(response))
	waitForCondition(t, time.Second, func() bool { s.agentMu.RLock(); defer s.agentMu.RUnlock(); return s.openapi != nil }, "metadata ready")
	entered, release, writerDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(writerDone)
		wire.Conn.WriteTextWithDeadlineStart(protocol.MustJSON(protocol.CancelRequest{Type: "cancel", ID: "holder"}), time.Second, func() error { close(entered); <-release; return nil })
	}()
	<-entered
	var once sync.Once
	unblock := func() { once.Do(func() { close(release); <-writerDone }) }
	defer unblock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- s.TransferOutput(ctx, "owner", "file", 1, strings.Repeat("0", 64), func(_ context.Context, b []byte) (int, error) {
			t.Error("sink ran before open write")
			return len(b), nil
		})
	}()
	open := <-wire.open
	peer.WriteText(protocol.MustJSON(protocol.Frame{Type: "transfer_chunk", ID: open.ID, TransferID: open.TransferID, Sequence: 0, DataBase64: "QQ=="}))
	if raw, e := peer.ReadText(); e == nil {
		t.Fatal("unwritten open granted peer credit", string(raw))
	}
	unblock()
	if e := <-result; e == nil {
		t.Fatal("early chunk succeeded")
	}
	s.agentMu.RLock()
	ac := s.agent
	s.agentMu.RUnlock()
	if ac != nil && ac.transfers.Retained() != 0 {
		t.Fatal("failed open retained buffer")
	}
}
func TestTransferInputMetadataGateAndFixedRecentBudget(t *testing.T) {
	s := newCancelStateTestServer()
	conn := newDeterministicCancelTransport()
	ac := newAgentConn(conn)
	ac.initializeTransfers()
	s.agent = ac
	defer func() { conn.Close(); ac.disconnectPending() }()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	begin := protocol.Frame{Type: "request_begin", ID: "fixed", Capability: "run", DefinitionHash: "hash", CallerBinding: "caller", InputDeadline: time.Now().Add(time.Hour)}
	if _, e := s.TransferInput(ctx, begin, nil, nil); e == nil || e.Error() != "OPENAPI_NOT_READY" {
		t.Fatal(e)
	}
	s.openapi = map[string]any{}
	s.capabilities = map[string]protocol.CapabilityMetadata{}
	if _, e := s.TransferInput(ctx, begin, nil, nil); e == nil || e.Error() != "NOT_FOUND" {
		t.Fatal(e)
	}
	s.capabilities = map[string]protocol.CapabilityMetadata{"run": {DefinitionHash: "other", InputTimeout: "100ms", ExecutionTimeout: "200ms"}}
	if _, e := s.TransferInput(ctx, begin, nil, nil); e == nil || e.Error() != "CONFLICT" {
		t.Fatal(e)
	}
	v := s.capabilities["run"]
	v.DefinitionHash = "hash"
	s.capabilities["run"] = v
	go ac.scheduler.Run()
	start := time.Now()
	result := make(chan error, 1)
	go func() { _, e := s.TransferInput(ctx, begin, nil, nil); result <- e }()
	wire := awaitTransportWrite(t, conn)
	f, e := protocol.DecodeFrame(wire)
	if e != nil || f.InputDeadline.After(start.Add(150*time.Millisecond)) {
		t.Fatal("input deadline was not clamped", f.InputDeadline, e)
	}
	if e := <-result; !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal(e)
	}
	ac.mu.Lock()
	expiry := ac.transferRecent["fixed"]
	ac.mu.Unlock()
	if !expiry.Equal(f.InputDeadline.Add(200*time.Millisecond + 100*time.Millisecond + 5*time.Second)) {
		t.Fatal("recent followed outer context", expiry, f.InputDeadline)
	}
}

func TestTransferRecentInputRefusalDoesNotRenewAndOutputReopens(t *testing.T) {
	ac := newAgentConn(nil)
	ac.initializeTransfers()
	defer ac.transferCancel()
	defer ac.transfers.Close()
	until := time.Now().Add(time.Second)
	_, release, e := ac.exchangeInput(until, "reserved")
	if e != nil {
		t.Fatal(e)
	}
	release()
	before := ac.transferRecent["reserved"]
	if _, _, e := ac.exchangeInput(until.Add(time.Hour), "reserved"); e == nil || e.Error() != "CONFLICT" {
		t.Fatal(e)
	}
	if len(ac.transferRecent) != 1 || len(ac.transferCalls) != 0 || !ac.transferRecent["reserved"].Equal(before) {
		t.Fatal("refusal allocated or renewed recent")
	}
	for range 2 {
		_, release, e := ac.exchange(until, "reserved")
		if e != nil {
			t.Fatal("output blocked by recent", e)
		}
		release()
	}
	ac.transferRecent["reserved"] = time.Now().Add(-time.Millisecond)
	if _, release, e := ac.exchangeInput(until, "reserved"); e != nil {
		t.Fatal("expired input still reserved", e)
	} else {
		release()
	}
}
