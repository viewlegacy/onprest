package ws

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"github.com/viewlegacy/onprest/internal/protocol"
	"io"
	"sync"
	"time"
)

var ErrProtocol = errors.New("transfer protocol violation")
var ErrNotFound = errors.New("transfer not found")
var ErrCapacity = errors.New("transfer capacity unavailable")

type retiredTransfer struct {
	expiry                    time.Time
	request                   string
	sending, canceled, ending bool
	next, acked, sent         int64
}

type receiveItem struct {
	frame protocol.Frame
	body  []byte
}
type wireTransfer struct {
	id, request       string
	sending           bool
	ctx               context.Context
	cancel            context.CancelFunc
	queue             chan receiveItem
	next, acked, sent int64
	ending            bool
	granted           bool
	expiry            time.Time
	done              chan error
	stopped           chan struct{}
}

// Transfers enforces both sides of the four-chunk window and the shared
// connection's 32 retained chunks. No transport reader waits for disk or credit.
type Transfers struct {
	ctx      context.Context
	mu       sync.Mutex
	live     map[string]*wireTransfer
	recent   map[string]*retiredTransfer
	retained int
	closed   bool
	notify   chan struct{}
	send     func(context.Context, protocol.Frame, bool, string, func()) error
	wg       sync.WaitGroup
}

func NewTransfers(ctx context.Context, send func(context.Context, protocol.Frame, bool, string, func()) error) *Transfers {
	return &Transfers{ctx: ctx, live: map[string]*wireTransfer{}, recent: map[string]*retiredTransfer{}, notify: make(chan struct{}), send: send}
}
func (t *Transfers) signal() {
	t.mu.Lock()
	close(t.notify)
	t.notify = make(chan struct{})
	t.mu.Unlock()
}
func (t *Transfers) add(ctx context.Context, request, id string, sending bool) (*wireTransfer, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, context.Canceled
	}
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	if e := t.ctx.Err(); e != nil {
		return nil, e
	}
	now := time.Now()
	for k, d := range t.recent {
		if !now.Before(d.expiry) {
			delete(t.recent, k)
		}
	}
	if len(t.live) >= protocol.ConnectionWindow/protocol.TransferWindow || len(t.live)+len(t.recent) >= 4096 {
		return nil, ErrCapacity
	}
	if !protocol.ValidID(request) || !protocol.ValidID(id) || t.live[id] != nil || t.recent[id] != nil {
		return nil, ErrProtocol
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, ErrProtocol
	}
	c, cancel := context.WithCancel(ctx)
	v := &wireTransfer{id: id, request: request, sending: sending, ctx: c, cancel: cancel, queue: make(chan receiveItem, 5), expiry: deadline, done: make(chan error, 1), stopped: make(chan struct{})}
	t.live[id] = v
	t.wg.Add(1)
	return v, nil
}
func (t *Transfers) retire(v *wireTransfer, err error) {
	t.mu.Lock()
	if t.live[v.id] == v {
		delete(t.live, v.id)
		t.recent[v.id] = &retiredTransfer{expiry: v.expiry.Add(5 * time.Second), request: v.request, sending: v.sending, canceled: err != nil || v.ctx.Err() != nil, ending: v.ending, next: v.next, acked: v.acked, sent: v.sent}
		t.retained -= int(v.next - v.acked)
	}
	t.mu.Unlock()
	v.cancel()
	v.done <- err
	close(v.done)
	close(v.stopped)
	t.signal()
}

// Receive reserves a receiver before its accept/open frame grants initial credit.
func (t *Transfers) Receive(ctx context.Context, request, id string, write func([]byte) error, end func(protocol.Frame) error, failed func(error)) error {
	_, e := t.receive(ctx, request, id, write, end, failed, true)
	return e
}
func (t *Transfers) ReceiveReserved(ctx context.Context, request, id string, write func([]byte) error, end func(protocol.Frame) error, failed func(error)) error {
	_, e := t.receive(ctx, request, id, write, end, failed, false)
	return e
}
func (t *Transfers) ReceiveReservedDone(ctx context.Context, request, id string, write func([]byte) error, end func(protocol.Frame) error, failed func(error)) (<-chan error, error) {
	return t.receive(ctx, request, id, write, end, failed, false)
}
func (t *Transfers) Grant(id string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	v := t.live[id]
	if v == nil || v.sending || v.granted {
		return ErrProtocol
	}
	v.granted = true
	return nil
}
func (t *Transfers) receive(ctx context.Context, request, id string, write func([]byte) error, end func(protocol.Frame) error, failed func(error), granted bool) (<-chan error, error) {
	v, e := t.add(ctx, request, id, false)
	if e != nil {
		return nil, e
	}
	t.mu.Lock()
	v.granted = granted
	t.mu.Unlock()
	go func() {
		defer t.wg.Done()
		var err error
		defer func() {
			t.retire(v, err)
			if err != nil {
				failed(err)
			}
		}()
		for {
			select {
			case item := <-v.queue:
				if item.frame.Type == "transfer_end" {
					err = end(item.frame)
					return
				}
				if err = write(item.body); err != nil {
					return
				}
				item.body = nil
				grant := func() {
					t.mu.Lock()
					if t.live[v.id] == v {
						v.acked++
						t.retained--
					}
					t.mu.Unlock()
					t.signal()
				}
				if err = t.send(v.ctx, protocol.Frame{Type: "ack", TransferID: v.id, Sequence: item.frame.Sequence}, true, "", grant); err != nil {
					return
				}
			case <-v.ctx.Done():
				err = v.ctx.Err()
				return
			case <-t.ctx.Done():
				err = t.ctx.Err()
				return
			}
		}
	}()
	return v.done, nil
}

// SendOpen reserves bounded transfer state before acquiring a reader.
func (t *Transfers) SendOpen(ctx context.Context, request, id string, open func() (io.ReadCloser, error), bytes func(context.Context, int) error) (<-chan error, error) {
	v, e := t.add(ctx, request, id, true)
	if e != nil {
		return nil, e
	}
	reader, e := open()
	if e != nil {
		if reader != nil {
			reader.Close()
		}
		t.retire(v, e)
		t.wg.Done()
		return nil, e
	}
	return t.sendReader(v, request, id, reader, bytes), nil
}
func (t *Transfers) Send(ctx context.Context, request, id string, reader io.ReadCloser, bytes func(context.Context, int) error) (<-chan error, error) {
	v, e := t.add(ctx, request, id, true)
	if e != nil {
		return nil, e
	}
	return t.sendReader(v, request, id, reader, bytes), nil
}
func (t *Transfers) sendReader(v *wireTransfer, request, id string, reader io.ReadCloser, bytes func(context.Context, int) error) <-chan error {
	go func() {
		defer t.wg.Done()
		var closeOnce sync.Once
		closeReader := func() { closeOnce.Do(func() { reader.Close() }) }
		stopped := make(chan struct{})
		go func() {
			select {
			case <-v.ctx.Done():
				closeReader()
			case <-t.ctx.Done():
				closeReader()
			case <-stopped:
			}
		}()
		var err error
		defer func() { close(stopped); closeReader(); t.retire(v, err) }()
		hash := sha256.New()
		var size int64
		buf := make([]byte, protocol.ChunkBytes)

		for {
			// Reserve before Read: even a blocked reader or a byte-budget waiter owns
			// one of the connection's 32 retained chunks, rather than an extra buffer.
			for {
				t.mu.Lock()
				if v.next-v.acked < protocol.TransferWindow && t.retained < protocol.ConnectionWindow {
					v.next++
					t.retained++
					t.mu.Unlock()
					break
				}
				wake := t.notify
				t.mu.Unlock()
				select {
				case <-wake:
				case <-v.ctx.Done():
					err = v.ctx.Err()
					return
				case <-t.ctx.Done():
					err = t.ctx.Err()
					return
				}
			}
			n, re := reader.Read(buf)
			if n > 0 {
				if bytes != nil {
					if err = bytes(v.ctx, n); err != nil {
						return
					}
				}
				t.mu.Lock()
				seq := v.next - 1
				t.mu.Unlock()
				hash.Write(buf[:n])
				size += int64(n)
				err = t.send(v.ctx, protocol.Frame{Type: "transfer_chunk", ID: request, TransferID: id, Sequence: seq, DataBase64: base64.StdEncoding.EncodeToString(buf[:n])}, false, id, func() {
					t.mu.Lock()
					if t.live[v.id] == v {
						v.sent++
					}
					t.mu.Unlock()
				})
				if err != nil {
					return
				}
			} else {
				t.mu.Lock()
				v.next--
				t.retained--
				t.mu.Unlock()
				t.signal()
			}
			if re != nil {
				if re != io.EOF {
					err = re
					return
				}
				break
			}
			if n == 0 {
				err = io.ErrNoProgress
				return
			}
		}
		// Drain all acknowledgements before the terminal frame, making end a barrier.
		for {
			t.mu.Lock()
			ready := v.sent == v.acked
			wake := t.notify
			t.mu.Unlock()
			if ready {
				break
			}
			select {
			case <-wake:
			case <-v.ctx.Done():
				err = v.ctx.Err()
				return
			case <-t.ctx.Done():
				err = t.ctx.Err()
				return
			}
		}
		err = t.send(v.ctx, protocol.Frame{Type: "transfer_end", ID: request, TransferID: id, Size: size, SHA256: hex.EncodeToString(hash.Sum(nil))}, false, id, nil)
	}()
	return v.done
}
func (t *Transfers) Handle(f protocol.Frame) error {
	t.mu.Lock()
	v := t.live[f.TransferID]
	if v == nil {
		defer t.mu.Unlock()
		if r := t.recent[f.TransferID]; r != nil && r.canceled && time.Now().Before(r.expiry) {
			if f.Type == "ack" && r.sending && f.Sequence == r.acked && r.acked < r.sent {
				r.acked++
				return nil
			}
			if f.ID != r.request {
				return ErrProtocol
			}
			if f.Type == "transfer_cancel" {
				return nil
			}
			if !r.sending && !r.ending {
				if f.Type == "transfer_chunk" && f.Sequence == r.next && r.next-r.acked < protocol.TransferWindow {
					r.next++
					return nil
				}
				if f.Type == "transfer_end" {
					r.ending = true
					return nil
				}
			}
		}
		if f.Type == "transfer_cancel" {
			return ErrNotFound
		}
		return ErrProtocol
	}
	if f.Type == "ack" {
		if !v.sending || f.Sequence != v.acked || v.acked >= v.sent {
			t.mu.Unlock()
			return ErrProtocol
		}
		v.acked++
		t.retained--
		t.mu.Unlock()
		t.signal()
		return nil
	}
	if f.ID != v.request {
		t.mu.Unlock()
		return ErrProtocol
	}
	if f.Type == "transfer_cancel" {
		v.cancel()
		t.mu.Unlock()
		return nil
	}
	if v.sending || v.ending || !v.granted {
		t.mu.Unlock()
		return ErrProtocol
	}
	item := receiveItem{frame: f}
	switch f.Type {
	case "transfer_chunk":
		if f.Sequence != v.next || v.next-v.acked >= protocol.TransferWindow || t.retained >= protocol.ConnectionWindow {
			t.mu.Unlock()
			return ErrProtocol
		}
		b, e := f.Chunk()
		if e != nil {
			t.mu.Unlock()
			return ErrProtocol
		}
		item.body = b
		v.next++
		t.retained++
	case "transfer_end":
		v.ending = true
	default:
		t.mu.Unlock()
		return ErrProtocol
	}
	select {
	case v.queue <- item:
		t.mu.Unlock()
		return nil
	default:
		t.mu.Unlock()
		return ErrProtocol
	}
}
func (t *Transfers) CancelRequest(id string) {
	t.mu.Lock()
	for _, v := range t.live {
		if v.request == id {
			v.cancel()
		}
	}
	t.mu.Unlock()
	t.signal()
}
func (t *Transfers) Close() {
	t.mu.Lock()
	t.closed = true
	for _, v := range t.live {
		v.cancel()
	}
	t.mu.Unlock()
	t.signal()
	t.wg.Wait()
}
func (t *Transfers) Retained() int { t.mu.Lock(); defer t.mu.Unlock(); return t.retained }
func (t *Transfers) WaitRequest(id string) {
	t.mu.Lock()
	var done []<-chan struct{}
	for _, v := range t.live {
		if v.request == id {
			done = append(done, v.stopped)
		}
	}
	t.mu.Unlock()
	for _, ch := range done {
		<-ch
	}
}

func (t *Transfers) Cancel(id string) {
	t.mu.Lock()
	if v := t.live[id]; v != nil {
		v.cancel()
	}
	t.mu.Unlock()
	t.signal()
}
func (t *Transfers) Wait(id string) {
	t.mu.Lock()
	v := t.live[id]
	t.mu.Unlock()
	if v != nil {
		<-v.stopped
	}
}
