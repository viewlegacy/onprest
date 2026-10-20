package gateway

import (
	"bufio"
	"context"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMCPFileResponseLeaseBoundsDeadlineAndCancelThroughHTTP(t *testing.T) {
	s := NewServer(Config{}, io.Discard)
	completed := make(chan struct{}, 4)
	text := strings.Repeat("<", 256<<10) // Encoded response exceeds a TCP send buffer.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		release, ok := s.reserveMCPResponse(32<<20, "id", 0)
		if !ok {
			w.WriteHeader(503)
			return
		}
		defer func() { release(); completed <- struct{}{} }()
		ctx, finish := mcpDeliveryContext(w, r, 2*time.Second)
		defer finish()
		writeMCPDelivery(ctx, w, "id", map[string]any{"contents": []any{map[string]any{"text": text}}})
	})
	server := httptest.NewUnstartedServer(s.withRecovery(handler))
	server.Config.ConnContext = func(ctx context.Context, conn net.Conn) context.Context {
		conn.(*net.TCPConn).SetWriteBuffer(1024)
		return ctx
	}
	server.Start()
	defer server.Close()
	var clients []net.Conn
	var readers []*bufio.Reader
	for i := 0; i < 4; i++ {
		conn, e := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
		if e != nil {
			t.Fatal(e)
		}
		clients = append(clients, conn)
		defer conn.Close()
		conn.(*net.TCPConn).SetReadBuffer(1024)
		io.WriteString(conn, "GET / HTTP/1.1\r\nHost: localhost\r\n\r\n")
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		readers = append(readers, bufio.NewReader(conn))
	}
	for _, reader := range readers {
		response, e := http.ReadResponse(reader, nil)
		if e != nil || response.StatusCode != 200 {
			t.Fatal("slow response headers", e)
		}
	}
	s.mcpResponseMu.Lock()
	count := s.mcpResponseCount
	reserved := new(big.Int).Set(&s.mcpResponseBytes)
	s.mcpResponseMu.Unlock()
	bound := big.NewInt(4 * (16*(int64(32<<20)+maxAgentResultBytes) + 2*int64(len(`"id"`)) + 65536))
	if count != 4 || reserved.Cmp(bound) != 0 {
		t.Fatal("response count/byte bound", count, reserved, bound)
	}
	response, e := http.Get(server.URL)
	if e != nil {
		t.Fatal(e)
	}
	response.Body.Close()
	if response.StatusCode != 503 {
		t.Fatal("fifth response queued", response.StatusCode)
	}
	// Outer request cancellation interrupts the pending write before the fixed
	// deadline; the other live clients stop at that deadline without reading.
	clients[0].Close()
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("cancel did not stop HTTP writer")
	}
	for i := 0; i < 3; i++ {
		select {
		case <-completed:
		case <-time.After(3 * time.Second):
			t.Fatal("deadline did not finish handler")
		}
	}
	s.mcpResponseMu.Lock()
	count = s.mcpResponseCount
	zero := s.mcpResponseBytes.Sign() == 0
	s.mcpResponseMu.Unlock()
	if count != 0 || !zero {
		t.Fatal("response lease leaked", count)
	}
	// A subsequent small response can use the same fixed frame, with no queue
	// or accumulated per-request byte reservations.
	release, ok := s.reserveMCPResponse(32<<20, "id", 0)
	if !ok {
		t.Fatal("lease not reclaimed")
	}
	release()
}

func TestMCPFileDeliveryDeadlineUsesOuterContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest("POST", "http://gateway/mcp", nil).WithContext(ctx)
	delivery, finish := mcpDeliveryContext(httptest.NewRecorder(), request, time.Second)
	cancel()
	select {
	case <-delivery.Done():
	case <-time.After(time.Second):
		t.Fatal("outer cancel")
	}
	finish()
}
