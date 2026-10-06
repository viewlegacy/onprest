//go:build linux || darwin

package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// This test proxy preserves the real signed HTTP/WS handshake and every frame
// except input ACKs. It does not replace either the Gateway or production Runner.
func droppingInputACKProxy(t *testing.T, target string, dropped, chunks *atomic.Int64) string {
	t.Helper()
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	var mu sync.Mutex
	var conns []net.Conn
	t.Cleanup(func() {
		listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range conns {
			conn.Close()
		}
	})
	go func() {
		for {
			client, e := listener.Accept()
			if e != nil {
				return
			}
			go func() {
				upstream, e := net.Dial("tcp", target)
				if e != nil {
					client.Close()
					return
				}
				mu.Lock()
				conns = append(conns, client, upstream)
				mu.Unlock()
				defer client.Close()
				defer upstream.Close()
				cr, ur := bufio.NewReader(client), bufio.NewReader(upstream)
				req, e := http.ReadRequest(cr)
				if e != nil {
					return
				}
				if e = req.Write(upstream); e != nil {
					return
				}
				resp, e := http.ReadResponse(ur, req)
				if e != nil {
					return
				}
				if e = resp.Write(client); e != nil {
					return
				}
				if resp.StatusCode != 101 {
					return
				}
				done := make(chan struct{})
				go func() { relayInputFrames(cr, upstream, true, dropped, chunks); close(done) }()
				relayInputFrames(ur, client, false, dropped, chunks)
				client.Close()
				upstream.Close()
				<-done
			}()
		}
	}()
	return listener.Addr().String()
}
func relayInputFrames(reader *bufio.Reader, destination net.Conn, drop bool, dropped, chunks *atomic.Int64) {
	for {
		header := make([]byte, 2)
		if _, e := io.ReadFull(reader, header); e != nil {
			return
		}
		n := uint64(header[1] & 127)
		if n == 126 {
			extra := make([]byte, 2)
			if _, e := io.ReadFull(reader, extra); e != nil {
				return
			}
			header = append(header, extra...)
			n = uint64(binary.BigEndian.Uint16(extra))
		} else if n == 127 {
			extra := make([]byte, 8)
			if _, e := io.ReadFull(reader, extra); e != nil {
				return
			}
			header = append(header, extra...)
			n = binary.BigEndian.Uint64(extra)
		}
		if n > 16<<20 {
			return
		}
		var mask []byte
		if header[1]&128 != 0 {
			mask = make([]byte, 4)
			if _, e := io.ReadFull(reader, mask); e != nil {
				return
			}
			header = append(header, mask...)
		}
		payload := make([]byte, int(n))
		if _, e := io.ReadFull(reader, payload); e != nil {
			return
		}
		decoded := payload
		if mask != nil {
			decoded = bytes.Clone(payload)
			for i := range decoded {
				decoded[i] ^= mask[i%4]
			}
		}
		var frame struct {
			Type string `json:"type"`
		}
		if header[0]&15 == 1 && json.Unmarshal(decoded, &frame) == nil {
			if drop && frame.Type == "ack" {
				dropped.Add(1)
				continue
			}
			if !drop && frame.Type == "transfer_chunk" {
				chunks.Add(1)
			}
		}
		if _, e := destination.Write(append(header, payload...)); e != nil {
			return
		}
	}
}
func TestInputDeliveryRealACKBackpressureStopsHTTPAndCleans(t *testing.T) {
	var dropped, chunks atomic.Int64
	f := productionInputHTTPWithRoute(t, func(addr string) string { return droppingInputACKProxy(t, addr, &dropped, &chunks) })
	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", f.base+"/api/v1/capabilities/save", pr)
	req.Header.Set("Authorization", "Bearer p10-key")
	req.Header.Set("Content-Type", writer.FormDataContentType())
	httpDone := make(chan error, 1)
	go func() {
		resp, e := http.DefaultClient.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		httpDone <- e
	}()
	var produced atomic.Int64
	writeDone := make(chan error, 1)
	go func() {
		part, e := writer.CreatePart(textproto.MIMEHeader{"Content-Disposition": {`form-data; name="document"; filename="a.bin"`}, "Content-Type": {"application/octet-stream"}})
		if e != nil {
			writeDone <- e
			return
		}
		buffer := bytes.Repeat([]byte{5}, 64<<10)
		for range 256 {
			n, e := part.Write(buffer)
			produced.Add(int64(n))
			if e != nil {
				writeDone <- e
				return
			}
		}
		writeDone <- writer.Close()
	}()
	waitTransport(t, func() bool { return dropped.Load() == 4 })
	if chunks.Load() != 4 {
		t.Fatal("ACK window", chunks.Load())
	}
	time.Sleep(100 * time.Millisecond)
	before := produced.Load()
	time.Sleep(100 * time.Millisecond)
	after := produced.Load()
	if before != after || after >= 8<<20 {
		t.Fatal("HTTP did not stop within bounded socket/chunk buffers", before, after)
	}
	if _, e := os.Stat(filepath.Join(f.dir, "exchange", "saved.bin")); !os.IsNotExist(e) {
		t.Fatal("business execution before commit")
	}
	t.Logf("withheld_ACKs=%d sent_chunks=%d HTTP_produced_bytes=%d", dropped.Load(), chunks.Load(), after)
	cancel()
	pw.Close()
	select {
	case <-httpDone:
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP cancel blocked")
	}
	select {
	case <-writeDone:
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP writer retained")
	}
	waitTransport(t, func() bool { return f.runner.store.Usage() == 0 })
}
