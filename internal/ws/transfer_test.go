package ws

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/viewlegacy/onprest/internal/protocol"
	"io"
	"sync"
	"testing"
	"time"
)

func TestTransferReceiverWindowSequenceOwnershipAndDigest(t *testing.T) {
	for _, kind := range []string{"sequence", "owner", "credit", "duplicate", "ack"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			block := make(chan struct{})
			x := NewTransfers(ctx, func(_ context.Context, _ protocol.Frame, _ bool, _ string, grant func()) error {
				if grant != nil {
					grant()
				}
				return nil
			})
			defer x.Close()
			if e := x.Receive(ctx, "r", "t", func([]byte) error {
				select {
				case <-block:
				case <-ctx.Done():
					return ctx.Err()
				}
				return nil
			}, func(protocol.Frame) error { return nil }, func(error) {}); e != nil {
				t.Fatal(e)
			}
			f := protocol.Frame{Type: "transfer_chunk", ID: "r", TransferID: "t", DataBase64: "QQ=="}
			switch kind {
			case "sequence":
				f.Sequence = 1
			case "owner":
				f.ID = "other"
			case "ack":
				f.Type = "ack"
			case "duplicate":
				if e := x.Receive(ctx, "r", "t", func([]byte) error { return nil }, func(protocol.Frame) error { return nil }, func(error) {}); !errors.Is(e, ErrProtocol) {
					t.Fatal("duplicate accepted", e)
				}
				close(block)
				return
			case "credit":
				for i := int64(0); i < protocol.TransferWindow; i++ {
					f.Sequence = i
					if e := x.Handle(f); e != nil {
						t.Fatal(e)
					}
				}
				f.Sequence = 4
			}
			if !errors.Is(x.Handle(f), ErrProtocol) {
				t.Fatal("violation accepted")
			}
			if x.Retained() > 4 {
				t.Fatal("window exceeded")
			}
			cancel()
		})
	}
}
func TestTransferSlowReceiverDoesNotBlockAnotherTransferAndCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var sender, receiver *Transfers
	started := make(chan struct{})
	unblock := make(chan struct{})
	sender = NewTransfers(ctx, func(_ context.Context, f protocol.Frame, _ bool, _ string, grant func()) error {
		if grant != nil {
			grant()
		}
		return receiver.Handle(f)
	})
	receiver = NewTransfers(ctx, func(_ context.Context, f protocol.Frame, _ bool, _ string, grant func()) error {
		if grant != nil {
			grant()
		}
		return sender.Handle(f)
	})
	defer sender.Close()
	defer receiver.Close()
	var slowOnce sync.Once
	var data bytes.Buffer
	end := make(chan protocol.Frame, 1)
	receiver.Receive(ctx, "slow", "slow", func([]byte) error {
		slowOnce.Do(func() { close(started) })
		select {
		case <-unblock:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}, func(protocol.Frame) error { return nil }, func(error) {})
	receiver.Receive(ctx, "fast", "fast", func(b []byte) error { _, e := data.Write(b); return e }, func(f protocol.Frame) error { end <- f; return nil }, func(error) {})
	slow, e := sender.Send(ctx, "slow", "slow", io.NopCloser(bytes.NewReader(make([]byte, 10*protocol.ChunkBytes))), nil)
	if e != nil {
		t.Fatal(e)
	}
	<-started
	body := bytes.Repeat([]byte{0, 255, 42}, 50000)
	fast, e := sender.Send(ctx, "fast", "fast", io.NopCloser(bytes.NewReader(body)), nil)
	if e != nil {
		t.Fatal(e)
	}
	if e = <-fast; e != nil {
		t.Fatal(e)
	}
	f := <-end
	sum := sha256.Sum256(body)
	if !bytes.Equal(data.Bytes(), body) || f.Size != int64(len(body)) || f.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal("transfer altered bytes")
	}
	if sender.Retained() > protocol.ConnectionWindow || receiver.Retained() > protocol.ConnectionWindow {
		t.Fatal("connection budget exceeded")
	}
	sender.CancelRequest("slow")
	receiver.CancelRequest("slow")
	if e := <-slow; !errors.Is(e, context.Canceled) {
		t.Fatal("cancel did not stop sender", e)
	}
	close(unblock)
}
func TestTransferConnectionWindowAndQueueBound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	x := NewTransfers(ctx, func(_ context.Context, _ protocol.Frame, _ bool, _ string, grant func()) error {
		if grant != nil {
			grant()
		}
		return nil
	})
	defer x.Close()
	for i := 0; i < 9; i++ {
		id := string(rune('a' + i))
		x.Receive(ctx, id, id, func([]byte) error { <-ctx.Done(); return ctx.Err() }, func(protocol.Frame) error { return nil }, func(error) {})
		for seq := int64(0); seq < 4; seq++ {
			e := x.Handle(protocol.Frame{Type: "transfer_chunk", ID: id, TransferID: id, Sequence: seq, DataBase64: base64.StdEncoding.EncodeToString([]byte{1})})
			if i < 8 && e != nil {
				t.Fatal(e)
			}
			if i == 8 && !errors.Is(e, ErrProtocol) {
				t.Fatal("33rd retained chunk accepted")
			}
		}
	}
	if x.Retained() != 32 {
		t.Fatal(x.Retained())
	}
	cancel()
}
func TestSchedulerControlPriorityRoundRobinAndCanceledJobs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := NewScheduler(ctx)
	var got []string
	add := func(label, transfer string, priority bool) {
		_, e := s.Post(ctx, priority, transfer, func() error { got = append(got, label); return nil })
		if e != nil {
			t.Fatal(e)
		}
	}
	add("a1", "a", false)
	add("a2", "a", false)
	add("b1", "b", false)
	add("b2", "b", false)
	add("call", "", false)
	add("call2", "", false)
	add("ack", "", true)
	for i := 0; i < 7; i++ {
		j, ok := s.next()
		if !ok {
			t.Fatal("job missing")
		}
		j.run()
	}
	if string(bytes.Join([][]byte{[]byte(got[0]), []byte(got[1]), []byte(got[2]), []byte(got[3]), []byte(got[4]), []byte(got[5]), []byte(got[6])}, []byte(","))) != "ack,call,a1,call2,b1,a2,b2" {
		t.Fatal(got)
	}
	for i := 0; i < 256; i++ {
		add("control", "", true)
	}
	if _, e := s.Post(ctx, true, "", func() error { return nil }); e == nil {
		t.Fatal("unbounded control queue")
	}
}

type blockingReader struct {
	started chan struct{}
	closed  chan struct{}
	once    sync.Once
	count   int
}

func (r *blockingReader) Read([]byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	<-r.closed
	return 0, io.ErrClosedPipe
}
func (r *blockingReader) Close() error {
	r.count++
	if r.count == 1 {
		close(r.closed)
	}
	return nil
}
func TestTransferCancelClosesBlockedReaderBeforeCompletion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	engine := NewTransfers(ctx, func(_ context.Context, _ protocol.Frame, _ bool, _ string, grant func()) error {
		if grant != nil {
			grant()
		}
		return nil
	})
	reader := &blockingReader{started: make(chan struct{}), closed: make(chan struct{})}
	done, e := engine.Send(ctx, "request", "blocked", reader, nil)
	if e != nil {
		t.Fatal(e)
	}
	<-reader.started
	engine.Cancel("blocked")
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reader did not stop")
	}
	engine.Close()
	if reader.count != 1 || engine.Retained() != 0 {
		t.Fatal("reader or credit retained", reader.count, engine.Retained())
	}
}

func TestTransferSharedBidirectional32ChunksAndInitialCreditCapacity(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	engine := NewTransfers(ctx, func(_ context.Context, _ protocol.Frame, _ bool, _ string, grant func()) error {
		if grant != nil {
			grant()
		}
		return nil
	})
	defer engine.Close()
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("send%d", i)
		if _, e := engine.Send(ctx, id, id, io.NopCloser(bytes.NewReader(make([]byte, 5*protocol.ChunkBytes))), nil); e != nil {
			t.Fatal(e)
		}
	}
	for deadline := time.Now().Add(time.Second); engine.Retained() != 16 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	if engine.Retained() != 16 {
		t.Fatal("sender buffers not accounted", engine.Retained())
	}
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("receive%d", i)
		if e := engine.Receive(ctx, id, id, func([]byte) error { <-ctx.Done(); return ctx.Err() }, func(protocol.Frame) error { return nil }, func(error) {}); e != nil {
			t.Fatal(e)
		}
		for seq := int64(0); seq < 4; seq++ {
			if e := engine.Handle(protocol.Frame{Type: "transfer_chunk", ID: id, TransferID: id, Sequence: seq, DataBase64: "QQ=="}); e != nil {
				t.Fatal(e)
			}
		}
	}
	if engine.Retained() != 32 {
		t.Fatal(engine.Retained())
	}
	if e := engine.Receive(ctx, "ninth", "ninth", func([]byte) error { return nil }, func(protocol.Frame) error { return nil }, func(error) {}); !errors.Is(e, ErrCapacity) {
		t.Fatal("ninth initial window granted", e)
	}
	if engine.Retained() != 32 || len(engine.live) != 8 {
		t.Fatal("capacity rejection mutated connection")
	}
	cancel()
	engine.Close()
	if engine.Retained() != 0 {
		t.Fatal("credit not reclaimed")
	}
}

func TestTransferRejectsChunkBeforeInitialCreditGrant(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	engine := NewTransfers(ctx, func(_ context.Context, _ protocol.Frame, _ bool, _ string, grant func()) error {
		if grant != nil {
			grant()
		}
		return nil
	})
	defer engine.Close()
	engine.ReceiveReserved(ctx, "request", "ungranted", func([]byte) error { return nil }, func(protocol.Frame) error { return nil }, func(error) {})
	if e := engine.Handle(protocol.Frame{Type: "transfer_chunk", ID: "request", TransferID: "ungranted", DataBase64: "QQ=="}); !errors.Is(e, ErrProtocol) {
		t.Fatal("chunk accepted without initial credit", e)
	}
	if engine.Retained() != 0 {
		t.Fatal("refused chunk retained")
	}
}

type closeBarrierReader struct {
	io.Reader
	entered, release chan struct{}
}

func (r *closeBarrierReader) Close() error { close(r.entered); <-r.release; return nil }
func TestTransferCompletionWaitsForReaderClose(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	engine := NewTransfers(ctx, func(_ context.Context, _ protocol.Frame, _ bool, _ string, grant func()) error {
		if grant != nil {
			grant()
		}
		return nil
	})
	defer engine.Close()
	reader := &closeBarrierReader{Reader: bytes.NewReader(nil), entered: make(chan struct{}), release: make(chan struct{})}
	done, e := engine.Send(ctx, "request", "close-barrier", reader, nil)
	if e != nil {
		t.Fatal(e)
	}
	<-reader.entered
	select {
	case <-done:
		close(reader.release)
		t.Fatal("completion preceded reader Close")
	default:
	}
	close(reader.release)
	if e := <-done; e != nil {
		t.Fatal(e)
	}
}

func TestTransferRecentDiscardUsesFixedWireGraceAndWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	engine := NewTransfers(ctx, nil)
	v, e := engine.add(ctx, "owner", "late", false)
	if e != nil {
		t.Fatal(e)
	}
	deadline, _ := ctx.Deadline()
	engine.retire(v, context.Canceled)
	engine.wg.Done()
	expiry := engine.recent["late"].expiry
	if !expiry.Equal(deadline.Add(5 * time.Second)) {
		t.Fatal("discard grace reset", expiry)
	}
	for seq := int64(0); seq < 4; seq++ {
		if e := engine.Handle(protocol.Frame{Type: "transfer_chunk", ID: "owner", TransferID: "late", Sequence: seq, DataBase64: "QQ=="}); e != nil {
			t.Fatal(e)
		}
	}
	if e := engine.Handle(protocol.Frame{Type: "transfer_chunk", ID: "owner", TransferID: "late", Sequence: 4, DataBase64: "QQ=="}); !errors.Is(e, ErrProtocol) {
		t.Fatal("discard exceeded prior credit", e)
	}
	if e := engine.Handle(protocol.Frame{Type: "transfer_cancel", ID: "foreign", TransferID: "late"}); !errors.Is(e, ErrProtocol) {
		t.Fatal("foreign discard", e)
	}
	if e := engine.Handle(protocol.Frame{Type: "ack", TransferID: "late"}); !errors.Is(e, ErrProtocol) {
		t.Fatal("wrong direction discard", e)
	}
	for range 10 {
		if e := engine.Handle(protocol.Frame{Type: "transfer_cancel", ID: "owner", TransferID: "late"}); e != nil {
			t.Fatal(e)
		}
	}
	if !engine.recent["late"].expiry.Equal(expiry) || engine.Retained() != 0 {
		t.Fatal("discard extended retention")
	}
	engine.recent["late"].expiry = time.Now().Add(-time.Second)
	if e := engine.Handle(protocol.Frame{Type: "transfer_end", ID: "owner", TransferID: "late"}); !errors.Is(e, ErrProtocol) {
		t.Fatal("expired discard", e)
	}
}
func TestTransferRecentCapacityRefusesBeforeReaderAcquisition(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	engine := NewTransfers(ctx, nil)
	for n := 0; n < 4096; n++ {
		engine.recent[fmt.Sprintf("old%d", n)] = &retiredTransfer{expiry: time.Now().Add(time.Minute)}
	}
	opened := false
	if _, e := engine.SendOpen(ctx, "request", "new", func() (io.ReadCloser, error) { opened = true; return io.NopCloser(bytes.NewReader(nil)), nil }, nil); !errors.Is(e, ErrCapacity) {
		t.Fatal(e)
	}
	if opened || len(engine.live) != 0 || len(engine.recent) != 4096 || engine.Retained() != 0 {
		t.Fatal("refusal acquired reader or mutated state")
	}
}

func TestTransferCloseWaitsForReaderAcquisitionAndRefusesNewState(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	engine := NewTransfers(ctx, func(context.Context, protocol.Frame, bool, string, func()) error { return nil })
	entered, release, opened := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(opened)
		done, e := engine.SendOpen(ctx, "request", "opening", func() (io.ReadCloser, error) {
			close(entered)
			<-release
			return io.NopCloser(bytes.NewReader(nil)), nil
		}, nil)
		if e == nil {
			<-done
		}
	}()
	<-entered
	stopped := make(chan struct{})
	go func() { engine.Close(); close(stopped) }()
	// Observe the closed admission flag rather than relying on scheduler timing.
	for {
		engine.mu.Lock()
		closed := engine.closed
		engine.mu.Unlock()
		if closed {
			break
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-stopped:
		close(release)
		t.Fatal("Close preceded reader acquisition stop")
	default:
	}
	if _, e := engine.SendOpen(ctx, "request", "late", func() (io.ReadCloser, error) { t.Error("closed engine acquired reader"); return nil, io.EOF }, nil); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	close(release)
	<-opened
	<-stopped
	if engine.Retained() != 0 {
		t.Fatal("Close leaked buffer")
	}
}

func TestTransferNetworkWriteLockDefersInitialACKAndSenderCredit(t *testing.T) {
	for _, kind := range []string{"initial", "ack", "chunk"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			nc := &deadlineTrackingConn{writeStarted: make(chan struct{}), releaseWrite: make(chan struct{})}
			close(nc.releaseWrite)
			waiting := make(chan struct{})
			var once sync.Once
			conn := &Conn{c: nc, br: bufio.NewReader(nc), beforeWriteLock: func(byte) { once.Do(func() { close(waiting) }) }}
			conn.writeMu.Lock()
			var unlockOnce sync.Once
			unlock := func() { unlockOnce.Do(func() { conn.writeMu.Unlock() }) }
			defer unlock()
			engine := NewTransfers(ctx, func(ctx context.Context, f protocol.Frame, _ bool, _ string, grant func()) error {
				return conn.WriteTextWithDeadlineStart(protocol.MustJSON(f), time.Second, func() error {
					if e := ctx.Err(); e != nil {
						return e
					}
					if grant != nil {
						grant()
					}
					return nil
				})
			})
			defer func() { cancel(); unlock(); engine.Close() }()
			if kind == "initial" {
				engine.ReceiveReserved(ctx, "owner", "wire", func([]byte) error { return nil }, func(protocol.Frame) error { return nil }, func(error) {})
				go conn.WriteTextWithDeadlineStart([]byte(`{}`), time.Second, func() error {
					if e := ctx.Err(); e != nil {
						return e
					}
					return engine.Grant("wire")
				})
			}
			if kind == "ack" {
				engine.Receive(ctx, "owner", "wire", func([]byte) error { return nil }, func(protocol.Frame) error { return nil }, func(error) {})
				for seq := int64(0); seq < 4; seq++ {
					if e := engine.Handle(protocol.Frame{Type: "transfer_chunk", ID: "owner", TransferID: "wire", Sequence: seq, DataBase64: "QQ=="}); e != nil {
						t.Fatal(e)
					}
				}
			}
			if kind == "chunk" {
				engine.Send(ctx, "owner", "wire", io.NopCloser(bytes.NewReader([]byte("A"))), nil)
			}
			<-waiting
			frame := protocol.Frame{Type: "transfer_chunk", ID: "owner", TransferID: "wire", DataBase64: "QQ=="}
			if kind == "ack" {
				frame.Sequence = 4
			}
			if kind == "chunk" {
				frame.Type = "ack"
				frame.ID = ""
			}
			before := engine.Retained()
			if e := engine.Handle(frame); !errors.Is(e, ErrProtocol) {
				t.Fatal("peer consumed unwritten state", e)
			}
			if engine.Retained() != before {
				t.Fatal("refusal mutated buffer")
			}
			engine.mu.Lock()
			v := engine.live["wire"]
			bad := v == nil || v.acked != 0 || v.sent != 0 || (kind == "initial" && v.granted)
			engine.mu.Unlock()
			if bad {
				t.Fatal("credit changed outside network lock")
			}
			cancel()
			unlock()
			engine.Close()
			if engine.Retained() != 0 {
				t.Fatal("failure retained credit")
			}
		})
	}
}
