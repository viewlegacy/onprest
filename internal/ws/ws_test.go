package ws

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestReadTextReassemblesMaskedFragmentsAndHandlesInterleavedPing(t *testing.T) {
	input := append(testFrame(false, 1, true, []byte("hel")), testFrame(true, 9, true, []byte("alive"))...)
	input = append(input, testFrame(true, 0, true, []byte("lo"))...)
	nc := newMemoryConn(input, 2)
	conn := &Conn{c: nc, br: bufio.NewReader(nc)}

	got, err := conn.ReadText()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Fatalf("ReadText() = %q, want hello", got)
	}
	fin, op, payload := decodeTestFrame(t, nc.written.Bytes(), false)
	if !fin || op != 10 || string(payload) != "alive" {
		t.Fatalf("pong = fin:%t op:%d payload:%q", fin, op, payload)
	}
}

func TestReadTextInvokesPongHandler(t *testing.T) {
	nc := newMemoryConn(append(testFrame(true, 10, true, []byte("pong")), testFrame(true, 1, true, []byte("ok"))...), 0)
	conn := &Conn{c: nc, br: bufio.NewReader(nc)}
	called := 0
	conn.SetPongHandler(func() { called++ })
	if _, err := conn.ReadText(); err != nil {
		t.Fatal(err)
	}
	if called != 1 {
		t.Fatalf("pong handler calls = %d, want 1", called)
	}
}

func TestReadTextRejectsProtocolViolationsAndLimits(t *testing.T) {
	tests := []struct {
		name  string
		frame []byte
	}{
		{name: "unmasked client frame", frame: testFrame(true, 1, false, []byte("x"))},
		{name: "unexpected continuation", frame: testFrame(true, 0, true, []byte("x"))},
		{name: "fragmented control", frame: testFrame(false, 9, true, []byte("x"))},
		{name: "binary message", frame: testFrame(true, 2, true, []byte("x"))},
		{name: "oversized frame", frame: oversizedFrameHeader(true)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			nc := newMemoryConn(tc.frame, 0)
			conn := &Conn{c: nc, br: bufio.NewReader(nc)}
			if _, err := conn.ReadText(); err == nil {
				t.Fatal("ReadText() error = nil")
			}
		})
	}
}

func TestReadTextEchoesClosePayload(t *testing.T) {
	payload := []byte{0x03, 0xe8, 'b', 'y', 'e'}
	nc := newMemoryConn(testFrame(true, 8, true, payload), 1)
	conn := &Conn{c: nc, br: bufio.NewReader(nc)}
	if _, err := conn.ReadText(); !errors.Is(err, io.EOF) {
		t.Fatalf("ReadText() error = %v, want EOF", err)
	}
	_, op, got := decodeTestFrame(t, nc.written.Bytes(), false)
	if op != 8 || !bytes.Equal(got, payload) {
		t.Fatalf("close echo op=%d payload=%v, want %v", op, got, payload)
	}
}

func TestWriteFrameRetriesShortWritesAndSerializesConcurrentMessages(t *testing.T) {
	nc := newMemoryConn(nil, 1)
	conn := &Conn{c: nc, br: bufio.NewReader(nc)}
	const count = 64
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(value byte) {
			defer wg.Done()
			if err := conn.WriteText(bytes.Repeat([]byte{value}, 32)); err != nil {
				t.Errorf("WriteText: %v", err)
			}
		}(byte(i))
	}
	wg.Wait()

	reader := bufio.NewReader(bytes.NewReader(nc.written.Bytes()))
	seen := map[byte]bool{}
	for i := 0; i < count; i++ {
		frame, err := readRawTestFrame(reader)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if frame.op != 1 || len(frame.payload) != 32 {
			t.Fatalf("frame %d op=%d len=%d", i, frame.op, len(frame.payload))
		}
		for _, b := range frame.payload {
			if b != frame.payload[0] {
				t.Fatalf("interleaved payload in frame %d: %v", i, frame.payload)
			}
		}
		seen[frame.payload[0]] = true
	}
	if len(seen) != count {
		t.Fatalf("unique messages = %d, want %d", len(seen), count)
	}
}

func TestCloseWritesOneCloseFrameAndClosesTransportOnce(t *testing.T) {
	nc := newMemoryConn(nil, 0)
	conn := &Conn{c: nc, br: bufio.NewReader(nc)}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if nc.closeCount != 1 {
		t.Fatalf("transport close count = %d, want 1", nc.closeCount)
	}
	reader := bufio.NewReader(bytes.NewReader(nc.written.Bytes()))
	frame, err := readRawTestFrame(reader)
	if err != nil || frame.op != 8 {
		t.Fatalf("close frame = %#v, err=%v", frame, err)
	}
	if _, err := readRawTestFrame(reader); !errors.Is(err, io.EOF) {
		t.Fatalf("second frame error = %v, want EOF", err)
	}
}

func TestWriteDeadlineAndCloseUnblockCommunicationBlackhole(t *testing.T) {
	local, remote := net.Pipe()
	defer remote.Close()
	conn := &Conn{c: local, br: bufio.NewReader(local), isClient: true}
	start := time.Now()
	err := conn.WriteTextWithDeadline(bytes.Repeat([]byte("x"), 1<<20), 50*time.Millisecond)
	if err == nil {
		t.Fatal("blackholed websocket write succeeded")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("write deadline took %s", elapsed)
	}
	start = time.Now()
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Close blocked for %s after a stalled write", elapsed)
	}
}

func TestTextAndPingDeadlinesStayInsideSerializedFrameWrite(t *testing.T) {
	nc := &deadlineTrackingConn{writeStarted: make(chan struct{}), releaseWrite: make(chan struct{})}
	pingAtWriteLock := make(chan struct{})
	conn := &Conn{
		c:  nc,
		br: bufio.NewReader(nc),
		beforeWriteLock: func(op byte) {
			if op == 9 {
				close(pingAtWriteLock)
			}
		},
	}
	textDone := make(chan error, 1)
	go func() { textDone <- conn.WriteTextWithDeadline([]byte("text"), time.Second) }()
	select {
	case <-nc.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("text write did not start")
	}
	pingDone := make(chan error, 1)
	go func() {
		pingDone <- conn.WritePingWithDeadline([]byte("ping"), time.Second)
	}()
	select {
	case <-pingAtWriteLock:
	case <-time.After(time.Second):
		t.Fatal("ping did not reach the atomic helper immediately before writeMu")
	}
	nc.mu.Lock()
	blockedEvents := append([]string(nil), nc.events...)
	nc.mu.Unlock()
	if got := fmt.Sprint(blockedEvents); got != fmt.Sprint([]string{"deadline:set", "write:1:deadline-set"}) {
		t.Fatalf("ping mutated the deadline outside writeMu while text was blocked: events=%v", blockedEvents)
	}

	// The hook proves that the ping reached the exact point immediately before
	// writeMu while the text frame still owns it. Releasing the underlying
	// write only after that observation makes the interleave deterministic.
	close(nc.releaseWrite)
	if err := <-textDone; err != nil {
		t.Fatal(err)
	}
	if err := <-pingDone; err != nil {
		t.Fatal(err)
	}
	nc.mu.Lock()
	events := append([]string(nil), nc.events...)
	nc.mu.Unlock()
	want := []string{"deadline:set", "write:1:deadline-set", "deadline:clear", "deadline:set", "write:9:deadline-set", "deadline:clear"}
	if fmt.Sprint(events) != fmt.Sprint(want) {
		t.Fatalf("events=%v want=%v", events, want)
	}
}

func TestPingDeadlineUnblocksCommunicationBlackhole(t *testing.T) {
	local, remote := net.Pipe()
	defer remote.Close()
	conn := &Conn{c: local, br: bufio.NewReader(local), isClient: true}
	start := time.Now()
	if err := conn.WritePingWithDeadline([]byte("ping"), 50*time.Millisecond); err == nil {
		t.Fatal("blackholed ping succeeded")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("ping deadline took %s", elapsed)
	}
	_ = conn.Close()
}

func TestCloseUnblocksConcurrentWriteWithoutDeadline(t *testing.T) {
	local, remote := net.Pipe()
	defer remote.Close()
	conn := &Conn{c: local, br: bufio.NewReader(local), isClient: true}
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- conn.WriteText(bytes.Repeat([]byte("x"), 1<<20))
	}()

	// net.Pipe has no buffering, so a peer that never reads leaves the write
	// holding writeMu until Close's forced transport shutdown releases it.
	time.Sleep(20 * time.Millisecond)
	start := time.Now()
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Close blocked for %s behind a concurrent stalled write", elapsed)
	}
	select {
	case err := <-writeDone:
		if err == nil {
			t.Fatal("blackholed write succeeded after Close")
		}
	case <-time.After(time.Second):
		t.Fatal("concurrent stalled write did not unblock after Close")
	}
}

func TestDialTimesOutDuringUpgradeResponse(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan struct{})
	release := make(chan struct{})
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		close(accepted)
		_, _ = bufio.NewReader(conn).ReadString('\n')
		<-release
	}()
	start := time.Now()
	conn, err := Dial(50*time.Millisecond, "ws://"+listener.Addr().String()+"/ws", nil)
	if conn != nil {
		_ = conn.Close()
		t.Fatal("Dial() unexpectedly succeeded")
	}
	if err == nil {
		t.Fatal("Dial() error = nil")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("Dial() did not honor handshake timeout: %s", time.Since(start))
	}
	<-accepted
	close(release)
}

type memoryConn struct {
	reader     *bytes.Reader
	written    bytes.Buffer
	maxWrite   int
	closeCount int
}

type deadlineTrackingConn struct {
	mu           sync.Mutex
	deadline     time.Time
	events       []string
	writeStarted chan struct{}
	releaseWrite chan struct{}
	startOnce    sync.Once
	reader       bytes.Reader
}

func (c *deadlineTrackingConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
func (c *deadlineTrackingConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	op := byte(0)
	if len(p) > 0 {
		op = p[0] & 0x0f
	}
	state := "deadline-clear"
	if !c.deadline.IsZero() {
		state = "deadline-set"
	}
	c.events = append(c.events, fmt.Sprintf("write:%d:%s", op, state))
	c.mu.Unlock()
	if op == 1 {
		c.startOnce.Do(func() { close(c.writeStarted) })
		<-c.releaseWrite
	}
	return len(p), nil
}
func (c *deadlineTrackingConn) Close() error                    { return nil }
func (c *deadlineTrackingConn) LocalAddr() net.Addr             { return testAddr("local") }
func (c *deadlineTrackingConn) RemoteAddr() net.Addr            { return testAddr("remote") }
func (c *deadlineTrackingConn) SetDeadline(time.Time) error     { return nil }
func (c *deadlineTrackingConn) SetReadDeadline(time.Time) error { return nil }
func (c *deadlineTrackingConn) SetWriteDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.deadline = deadline
	if deadline.IsZero() {
		c.events = append(c.events, "deadline:clear")
	} else {
		c.events = append(c.events, "deadline:set")
	}
	c.mu.Unlock()
	return nil
}

func newMemoryConn(input []byte, maxWrite int) *memoryConn {
	return &memoryConn{reader: bytes.NewReader(input), maxWrite: maxWrite}
}

func (c *memoryConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
func (c *memoryConn) Write(p []byte) (int, error) {
	if c.maxWrite > 0 && len(p) > c.maxWrite {
		p = p[:c.maxWrite]
	}
	return c.written.Write(p)
}
func (c *memoryConn) Close() error                     { c.closeCount++; return nil }
func (c *memoryConn) LocalAddr() net.Addr              { return testAddr("local") }
func (c *memoryConn) RemoteAddr() net.Addr             { return testAddr("remote") }
func (c *memoryConn) SetDeadline(time.Time) error      { return nil }
func (c *memoryConn) SetReadDeadline(time.Time) error  { return nil }
func (c *memoryConn) SetWriteDeadline(time.Time) error { return nil }

type testAddr string

func (a testAddr) Network() string { return "test" }
func (a testAddr) String() string  { return string(a) }

func testFrame(fin bool, op byte, masked bool, payload []byte) []byte {
	first := op
	if fin {
		first |= 0x80
	}
	maskBit := byte(0)
	if masked {
		maskBit = 0x80
	}
	frame := []byte{first, maskBit | byte(len(payload))}
	if masked {
		mask := []byte{1, 2, 3, 4}
		frame = append(frame, mask...)
		for i, b := range payload {
			frame = append(frame, b^mask[i%4])
		}
	} else {
		frame = append(frame, payload...)
	}
	return frame
}

func oversizedFrameHeader(masked bool) []byte {
	maskBit := byte(0)
	if masked {
		maskBit = 0x80
	}
	frame := []byte{0x81, maskBit | 127}
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], maxMessageSize+1)
	return append(frame, size[:]...)
}

type rawTestFrame struct {
	fin     bool
	op      byte
	payload []byte
}

func readRawTestFrame(r *bufio.Reader) (rawTestFrame, error) {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return rawTestFrame{}, err
	}
	length := int(h[1] & 0x7f)
	if length == 126 {
		var b [2]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return rawTestFrame{}, err
		}
		length = int(binary.BigEndian.Uint16(b[:]))
	} else if length == 127 {
		var b [8]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return rawTestFrame{}, err
		}
		length = int(binary.BigEndian.Uint64(b[:]))
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return rawTestFrame{}, err
	}
	return rawTestFrame{fin: h[0]&0x80 != 0, op: h[0] & 0xf, payload: payload}, nil
}

func decodeTestFrame(t *testing.T, raw []byte, masked bool) (bool, byte, []byte) {
	t.Helper()
	frame, err := readRawTestFrame(bufio.NewReader(bytes.NewReader(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if masked {
		t.Fatal("masked frame decoding is not used by this assertion")
	}
	return frame.fin, frame.op, frame.payload
}
