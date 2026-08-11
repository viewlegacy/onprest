package ws

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	guid                = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	maxMessageSize      = 16 << 20
	controlWriteTimeout = 5 * time.Second
)

type Conn struct {
	c        net.Conn
	br       *bufio.Reader
	isClient bool

	writeMu            sync.Mutex
	beforeWriteLock    func(byte)
	closeFrameOnce     sync.Once
	closeOnce          sync.Once
	transportCloseOnce sync.Once
	closeErr           error
	transportCloseErr  error

	handlerMu   sync.RWMutex
	pongHandler func()
}

func Accept(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return nil, errors.New("missing websocket upgrade")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if !validHandshakeKey(key) {
		return nil, errors.New("missing or invalid websocket key")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("response writer cannot hijack")
	}
	nc, rw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	accept := acceptKey(key)
	_, err = fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept)
	if err != nil {
		_ = nc.Close()
		return nil, err
	}
	if err := rw.Flush(); err != nil {
		_ = nc.Close()
		return nil, err
	}
	return &Conn{c: nc, br: rw.Reader}, nil
}

func Dial(handshakeTimeout time.Duration, rawurl string, headers http.Header) (*Conn, error) {
	u, err := url.Parse(rawurl)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return nil, fmt.Errorf("unsupported websocket scheme %q", u.Scheme)
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		if u.Scheme == "wss" {
			host += ":443"
		} else {
			host += ":80"
		}
	}
	dialer := net.Dialer{Timeout: handshakeTimeout}
	var nc net.Conn
	if u.Scheme == "wss" {
		nc, err = tls.DialWithDialer(&dialer, "tcp", host, &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12})
	} else {
		nc, err = dialer.Dial("tcp", host)
	}
	if err != nil {
		return nil, err
	}
	if handshakeTimeout > 0 {
		if err := nc.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
			_ = nc.Close()
			return nil, err
		}
	}
	key := headers.Get("Sec-WebSocket-Key")
	if key == "" {
		key, err = NewHandshakeKey()
		if err != nil {
			_ = nc.Close()
			return nil, err
		}
	} else if !validHandshakeKey(key) {
		_ = nc.Close()
		return nil, errors.New("invalid Sec-WebSocket-Key")
	}
	path := u.RequestURI()
	if path == "" {
		path = "/"
	}
	req := http.Header{}
	req.Set("Host", u.Host)
	req.Set("Upgrade", "websocket")
	req.Set("Connection", "Upgrade")
	req.Set("Sec-WebSocket-Version", "13")
	req.Set("Sec-WebSocket-Key", key)
	for k, values := range headers {
		if isWebSocketHandshakeHeader(k) {
			continue
		}
		for _, v := range values {
			req.Add(k, v)
		}
	}
	if _, err := fmt.Fprintf(nc, "GET %s HTTP/1.1\r\n", path); err != nil {
		_ = nc.Close()
		return nil, err
	}
	for k, values := range req {
		for _, v := range values {
			if _, err := fmt.Fprintf(nc, "%s: %s\r\n", k, v); err != nil {
				_ = nc.Close()
				return nil, err
			}
		}
	}
	if _, err := fmt.Fprint(nc, "\r\n"); err != nil {
		_ = nc.Close()
		return nil, err
	}
	br := bufio.NewReader(nc)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		_ = nc.Close()
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		_ = nc.Close()
		return nil, fmt.Errorf("websocket upgrade failed: %s", resp.Status)
	}
	if !headerHasToken(resp.Header, "Connection", "upgrade") || !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") {
		_ = nc.Close()
		return nil, errors.New("invalid websocket upgrade response")
	}
	if resp.Header.Get("Sec-WebSocket-Accept") != acceptKey(key) {
		_ = nc.Close()
		return nil, errors.New("invalid websocket accept key")
	}
	if err := nc.SetDeadline(time.Time{}); err != nil {
		_ = nc.Close()
		return nil, err
	}
	return &Conn{c: nc, br: br, isClient: true}, nil
}

func (c *Conn) ReadText() ([]byte, error) {
	var message []byte
	fragmented := false
	for {
		fin, op, payload, err := c.readFrame()
		if err != nil {
			return nil, err
		}
		switch op {
		case 0:
			if !fragmented {
				return nil, errors.New("unexpected websocket continuation frame")
			}
			if len(message)+len(payload) > maxMessageSize {
				return nil, errors.New("websocket message too large")
			}
			message = append(message, payload...)
			if fin {
				return message, nil
			}
		case 1:
			if fragmented {
				return nil, errors.New("new websocket data frame during fragmented message")
			}
			if fin {
				return payload, nil
			}
			fragmented = true
			message = append(message, payload...)
		case 2:
			return nil, errors.New("binary websocket messages are not supported")
		case 8:
			if len(payload) == 1 {
				return nil, errors.New("invalid websocket close payload")
			}
			c.writeClose(payload)
			return nil, io.EOF
		case 9:
			if err := c.writeFrameWithDeadline(10, payload, controlWriteTimeout); err != nil {
				return nil, err
			}
		case 10:
			c.handlerMu.RLock()
			handler := c.pongHandler
			c.handlerMu.RUnlock()
			if handler != nil {
				handler()
			}
		default:
			return nil, fmt.Errorf("unsupported websocket opcode %d", op)
		}
	}
}

func (c *Conn) WriteText(payload []byte) error { return c.writeFrame(1, payload) }

func (c *Conn) WriteTextWithDeadline(payload []byte, timeout time.Duration) error {
	return c.writeFrameWithDeadline(1, payload, timeout)
}

func (c *Conn) WritePing(payload []byte) error {
	return c.WritePingWithDeadline(payload, 0)
}

func (c *Conn) WritePingWithDeadline(payload []byte, timeout time.Duration) error {
	if len(payload) > 125 {
		return errors.New("websocket ping payload too large")
	}
	return c.writeFrameWithDeadline(9, payload, timeout)
}

func (c *Conn) SetReadDeadline(deadline time.Time) error { return c.c.SetReadDeadline(deadline) }

func (c *Conn) SetWriteDeadline(deadline time.Time) error { return c.c.SetWriteDeadline(deadline) }

func (c *Conn) SetPongHandler(handler func()) {
	c.handlerMu.Lock()
	c.pongHandler = handler
	c.handlerMu.Unlock()
}

func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		forceClose := time.AfterFunc(250*time.Millisecond, func() { _ = c.closeTransport() })
		c.writeClose([]byte{0x03, 0xe8})
		forceClose.Stop()
		c.closeErr = c.closeTransport()
	})
	return c.closeErr
}

func (c *Conn) closeTransport() error {
	c.transportCloseOnce.Do(func() { c.transportCloseErr = c.c.Close() })
	return c.transportCloseErr
}

func (c *Conn) writeClose(payload []byte) {
	c.closeFrameOnce.Do(func() { _ = c.writeFrame(8, payload) })
}

func (c *Conn) readFrame() (bool, byte, []byte, error) {
	var h [2]byte
	if _, err := io.ReadFull(c.br, h[:]); err != nil {
		return false, 0, nil, err
	}
	fin := h[0]&0x80 != 0
	if h[0]&0x70 != 0 {
		return false, 0, nil, errors.New("websocket extensions are not supported")
	}
	op := h[0] & 0x0f
	masked := h[1]&0x80 != 0
	if masked == c.isClient {
		if c.isClient {
			return false, 0, nil, errors.New("server websocket frame must not be masked")
		}
		return false, 0, nil, errors.New("client websocket frame must be masked")
	}
	l := uint64(h[1] & 0x7f)
	if l == 126 {
		var b [2]byte
		if _, err := io.ReadFull(c.br, b[:]); err != nil {
			return false, 0, nil, err
		}
		l = uint64(binary.BigEndian.Uint16(b[:]))
		if l < 126 {
			return false, 0, nil, errors.New("non-canonical websocket frame length")
		}
	} else if l == 127 {
		var b [8]byte
		if _, err := io.ReadFull(c.br, b[:]); err != nil {
			return false, 0, nil, err
		}
		l = binary.BigEndian.Uint64(b[:])
		if l&(uint64(1)<<63) != 0 || l <= 65535 {
			return false, 0, nil, errors.New("invalid websocket frame length")
		}
	}
	if op >= 8 && (!fin || l > 125) {
		return false, 0, nil, errors.New("invalid websocket control frame")
	}
	if l > maxMessageSize {
		return false, 0, nil, errors.New("websocket frame too large")
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(c.br, mask[:]); err != nil {
			return false, 0, nil, err
		}
	}
	payload := make([]byte, int(l))
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return false, 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return fin, op, payload, nil
}

func (c *Conn) writeFrame(op byte, payload []byte) error {
	return c.writeFrameWithDeadline(op, payload, 0)
}

func (c *Conn) writeFrameWithDeadline(op byte, payload []byte, timeout time.Duration) error {
	if op >= 8 && len(payload) > 125 {
		return errors.New("websocket control payload too large")
	}
	if c.beforeWriteLock != nil {
		c.beforeWriteLock(op)
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if timeout > 0 {
		if err := c.c.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
			return err
		}
		defer c.c.SetWriteDeadline(time.Time{})
	}

	maskBit := byte(0)
	if c.isClient {
		maskBit = 0x80
	}
	headerLen := 2
	switch {
	case len(payload) < 126:
	case len(payload) <= 65535:
		headerLen += 2
	default:
		headerLen += 8
	}
	if c.isClient {
		headerLen += 4
	}
	frame := make([]byte, headerLen+len(payload))
	frame[0] = 0x80 | op
	pos := 2
	switch {
	case len(payload) < 126:
		frame[1] = maskBit | byte(len(payload))
	case len(payload) <= 65535:
		frame[1] = maskBit | 126
		binary.BigEndian.PutUint16(frame[pos:pos+2], uint16(len(payload)))
		pos += 2
	default:
		frame[1] = maskBit | 127
		binary.BigEndian.PutUint64(frame[pos:pos+8], uint64(len(payload)))
		pos += 8
	}
	if c.isClient {
		mask := frame[pos : pos+4]
		if _, err := rand.Read(mask); err != nil {
			return err
		}
		pos += 4
		for i := range payload {
			frame[pos+i] = payload[i] ^ mask[i%4]
		}
	} else {
		copy(frame[pos:], payload)
	}
	return writeFull(c.c, frame)
}

func writeFull(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if n > 0 {
			p = p[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func acceptKey(key string) string {
	sum := sha1.Sum([]byte(key + guid))
	return base64.StdEncoding.EncodeToString(sum[:])
}

func NewHandshakeKey() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b[:]), nil
}

func validHandshakeKey(key string) bool {
	b, err := base64.StdEncoding.DecodeString(key)
	return err == nil && len(b) == 16
}

func isWebSocketHandshakeHeader(key string) bool {
	switch http.CanonicalHeaderKey(key) {
	case "Host", "Upgrade", "Connection", "Sec-Websocket-Version", "Sec-Websocket-Key":
		return true
	default:
		return false
	}
}

func headerHasToken(header http.Header, name, token string) bool {
	for _, value := range header.Values(name) {
		for _, part := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}
