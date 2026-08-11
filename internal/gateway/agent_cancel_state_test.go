package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/viewlegacy/onprest/internal/protocol"
)

type cancelTransportRead struct {
	payload []byte
	err     error
}

type deterministicCancelTransport struct {
	mu          sync.Mutex
	writes      [][]byte
	writeEvents chan []byte
	readEvents  chan cancelTransportRead
	blockWrite  map[int]chan struct{}
	writeErrors map[int]error
	closed      chan struct{}
	closeOnce   sync.Once
}

func newDeterministicCancelTransport() *deterministicCancelTransport {
	return &deterministicCancelTransport{
		writeEvents: make(chan []byte, 8),
		readEvents:  make(chan cancelTransportRead, 1),
		blockWrite:  map[int]chan struct{}{},
		writeErrors: map[int]error{},
		closed:      make(chan struct{}),
	}
}

func newCancelStateTestServer() *Server {
	return NewServer(Config{
		AgentTimeout:      time.Second,
		AgentWriteTimeout: time.Second,
	}, io.Discard)
}

func (c *deterministicCancelTransport) ReadText() ([]byte, error) {
	select {
	case event := <-c.readEvents:
		return event.payload, event.err
	case <-c.closed:
		return nil, io.EOF
	}
}

func (c *deterministicCancelTransport) WriteText(payload []byte) error {
	c.mu.Lock()
	index := len(c.writes)
	copyPayload := append([]byte(nil), payload...)
	c.writes = append(c.writes, copyPayload)
	release := c.blockWrite[index]
	err := c.writeErrors[index]
	c.mu.Unlock()
	c.writeEvents <- copyPayload
	if release != nil {
		select {
		case <-release:
		case <-c.closed:
			return io.ErrClosedPipe
		}
	}
	return err
}

func (c *deterministicCancelTransport) WriteTextWithDeadline(payload []byte, _ time.Duration) error {
	return c.WriteText(payload)
}

func (c *deterministicCancelTransport) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func (c *deterministicCancelTransport) payloads() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([][]byte, len(c.writes))
	for i := range c.writes {
		result[i] = append([]byte(nil), c.writes[i]...)
	}
	return result
}

func TestHTTPContextCancelBeforeDuringAndAfterRequestSend(t *testing.T) {
	t.Run("before send", func(t *testing.T) {
		s := newCancelStateTestServer()
		conn := newDeterministicCancelTransport()
		ac := newAgentConn(conn)
		s.agent = ac
		for range cap(ac.send) {
			ac.send <- agentWrite{}
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		result := s.callAgent(ctx, "mutate", nil)
		if result.Status != http.StatusGatewayTimeout {
			t.Fatalf("result=%+v", result)
		}
		assertAgentCallStateEmpty(t, ac)
		if len(conn.payloads()) != 0 {
			t.Fatalf("canceled request reached transport: %q", conn.payloads())
		}
	})

	for _, phase := range []string{"during send", "after send"} {
		t.Run(phase, func(t *testing.T) {
			s := newCancelStateTestServer()
			s.cfg.AgentTimeout = time.Second
			conn := newDeterministicCancelTransport()
			if phase == "during send" {
				conn.blockWrite[0] = make(chan struct{})
			}
			ac := newAgentConn(conn)
			s.agent = ac
			go s.writeAgent(ac)
			ctx, cancel := context.WithCancel(context.Background())
			resultCh := make(chan agentCallResult, 1)
			go func() { resultCh <- s.callAgent(ctx, "mutate", nil) }()
			requestPayload := awaitTransportWrite(t, conn)
			var request protocol.Request
			if err := json.Unmarshal(requestPayload, &request); err != nil {
				t.Fatal(err)
			}
			if phase == "after send" {
				waitForSendPhase(t, ac, request.ID, "sent")
			}
			cancel()
			result := <-resultCh
			if result.Status != http.StatusGatewayTimeout {
				t.Fatalf("result=%+v", result)
			}
			if phase == "during send" {
				close(conn.blockWrite[0])
			}
			cancelPayload := awaitTransportWrite(t, conn)
			assertRequestThenCancel(t, requestPayload, cancelPayload)
			waitForAgentCallStateEmpty(t, ac)
			ac.disconnectPending()
		})
	}
}

func TestResponseAndContextCancelSimultaneousTerminalCleanup(t *testing.T) {
	for iteration := range 50 {
		t.Run(string(rune('a'+iteration%26))+time.Duration(iteration).String(), func(t *testing.T) {
			s := newCancelStateTestServer()
			s.cfg.AgentTimeout = time.Second
			conn := newDeterministicCancelTransport()
			ac := newAgentConn(conn)
			s.agent = ac
			go s.writeAgent(ac)
			readDone := make(chan struct{})
			go func() {
				s.readAgent(ac)
				close(readDone)
			}()
			ctx, cancel := context.WithCancel(context.Background())
			resultCh := make(chan agentCallResult, 1)
			go func() { resultCh <- s.callAgent(ctx, "mutate", nil) }()
			requestPayload := awaitTransportWrite(t, conn)
			var request protocol.Request
			if err := json.Unmarshal(requestPayload, &request); err != nil {
				t.Fatal(err)
			}
			waitForSendPhase(t, ac, request.ID, "sent")
			start := make(chan struct{})
			go func() {
				<-start
				cancel()
			}()
			go func() {
				<-start
				conn.readEvents <- cancelTransportRead{payload: protocol.MustJSON(protocol.Response{ID: request.ID, Result: json.RawMessage(`{"count":1}`)})}
			}()
			close(start)
			result := <-resultCh
			if result.Status != http.StatusOK && result.Status != http.StatusGatewayTimeout {
				t.Fatalf("result=%+v", result)
			}
			waitForAgentCallStateEmpty(t, ac)
			for _, payload := range conn.payloads()[1:] {
				assertRequestThenCancel(t, requestPayload, payload)
			}
			_ = conn.Close()
			select {
			case <-readDone:
			case <-time.After(time.Second):
				t.Fatal("reader did not stop")
			}
		})
	}
}

func TestControlQueueSaturationClosesConnectionAndCleansState(t *testing.T) {
	s := newCancelStateTestServer()
	conn := newDeterministicCancelTransport()
	ac := newAgentConn(conn)
	const id = "saturated"
	ac.sendStates[id] = &requestSendState{phase: "sent"}
	for range cap(ac.control) {
		ac.control <- agentWrite{}
	}
	s.requestAgentCancel(ac, id)
	assertAgentCallStateEmpty(t, ac)
	select {
	case <-conn.closed:
	default:
		t.Fatal("control queue saturation did not close connection")
	}
}

func TestCancelWriteFailureClosesConnectionAndCleansTerminalState(t *testing.T) {
	s := newCancelStateTestServer()
	s.cfg.AgentTimeout = 20 * time.Millisecond
	conn := newDeterministicCancelTransport()
	conn.writeErrors[1] = errors.New("cancel write failed")
	ac := newAgentConn(conn)
	s.agent = ac
	go s.writeAgent(ac)
	result := s.callAgent(context.Background(), "mutate", nil)
	if result.Status != http.StatusGatewayTimeout {
		t.Fatalf("result=%+v", result)
	}
	requestPayload := awaitExistingTransportWrite(t, conn, 0)
	cancelPayload := awaitExistingTransportWrite(t, conn, 1)
	assertRequestThenCancel(t, requestPayload, cancelPayload)
	waitForAgentCallStateEmpty(t, ac)
	select {
	case <-conn.closed:
	case <-time.After(time.Second):
		t.Fatal("cancel write failure did not close connection")
	}
}

func awaitTransportWrite(t *testing.T, conn *deterministicCancelTransport) []byte {
	t.Helper()
	select {
	case payload := <-conn.writeEvents:
		return payload
	case <-time.After(time.Second):
		t.Fatal("transport write did not start")
		return nil
	}
}

func awaitExistingTransportWrite(t *testing.T, conn *deterministicCancelTransport, index int) []byte {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		payloads := conn.payloads()
		if len(payloads) > index {
			return payloads[index]
		}
		if time.Now().After(deadline) {
			t.Fatalf("transport write %d did not occur", index)
		}
		time.Sleep(time.Millisecond)
	}
}

func assertRequestThenCancel(t *testing.T, requestPayload, cancelPayload []byte) {
	t.Helper()
	var request protocol.Request
	if err := json.Unmarshal(requestPayload, &request); err != nil {
		t.Fatal(err)
	}
	var cancel protocol.CancelRequest
	if err := json.Unmarshal(cancelPayload, &cancel); err != nil {
		t.Fatal(err)
	}
	if request.ID == "" || cancel.Type != "cancel" || cancel.ID != request.ID {
		t.Fatalf("request=%s cancel=%s", requestPayload, cancelPayload)
	}
}

func waitForSendPhase(t *testing.T, ac *agentConn, id, phase string) {
	t.Helper()
	waitForCondition(t, time.Second, func() bool {
		ac.mu.Lock()
		defer ac.mu.Unlock()
		state := ac.sendStates[id]
		return state != nil && state.phase == phase
	}, "request send phase "+phase)
}

func waitForAgentCallStateEmpty(t *testing.T, ac *agentConn) {
	t.Helper()
	waitForCondition(t, time.Second, func() bool {
		ac.mu.Lock()
		defer ac.mu.Unlock()
		return len(ac.pending) == 0 && len(ac.sendStates) == 0
	}, "pending/send state cleanup")
}

func assertAgentCallStateEmpty(t *testing.T, ac *agentConn) {
	t.Helper()
	ac.mu.Lock()
	defer ac.mu.Unlock()
	if len(ac.pending) != 0 || len(ac.sendStates) != 0 {
		t.Fatalf("pending=%d sendStates=%d", len(ac.pending), len(ac.sendStates))
	}
}
