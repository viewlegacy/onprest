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
	"time"
)

const guid = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

type Conn struct {
	c        net.Conn
	br       *bufio.Reader
	isClient bool
}

func Accept(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return nil, errors.New("missing websocket upgrade")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		return nil, errors.New("missing websocket key")
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

func Dial(ctxDeadline time.Duration, rawurl string, headers http.Header) (*Conn, error) {
	u, err := url.Parse(rawurl)
	if err != nil {
		return nil, err
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		if u.Scheme == "wss" {
			host += ":443"
		} else {
			host += ":80"
		}
	}
	dialer := net.Dialer{Timeout: ctxDeadline}
	var nc net.Conn
	if u.Scheme == "wss" {
		nc, err = tls.DialWithDialer(&dialer, "tcp", host, &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12})
	} else {
		nc, err = dialer.Dial("tcp", host)
	}
	if err != nil {
		return nil, err
	}
	key, err := randomKey()
	if err != nil {
		_ = nc.Close()
		return nil, err
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
	if resp.StatusCode != http.StatusSwitchingProtocols {
		_ = nc.Close()
		return nil, fmt.Errorf("websocket upgrade failed: %s", resp.Status)
	}
	if resp.Header.Get("Sec-WebSocket-Accept") != acceptKey(key) {
		_ = nc.Close()
		return nil, errors.New("invalid websocket accept key")
	}
	return &Conn{c: nc, br: br, isClient: true}, nil
}

func (c *Conn) ReadText() ([]byte, error) {
	for {
		op, payload, err := c.readFrame()
		if err != nil {
			return nil, err
		}
		switch op {
		case 1:
			return payload, nil
		case 8:
			return nil, io.EOF
		case 9:
			_ = c.writeFrame(10, payload)
		}
	}
}

func (c *Conn) WriteText(payload []byte) error {
	return c.writeFrame(1, payload)
}

func (c *Conn) Close() error {
	_ = c.writeFrame(8, nil)
	return c.c.Close()
}

func (c *Conn) readFrame() (byte, []byte, error) {
	h := make([]byte, 2)
	if _, err := io.ReadFull(c.br, h); err != nil {
		return 0, nil, err
	}
	op := h[0] & 0x0f
	masked := h[1]&0x80 != 0
	l := uint64(h[1] & 0x7f)
	if l == 126 {
		var b [2]byte
		if _, err := io.ReadFull(c.br, b[:]); err != nil {
			return 0, nil, err
		}
		l = uint64(binary.BigEndian.Uint16(b[:]))
	} else if l == 127 {
		var b [8]byte
		if _, err := io.ReadFull(c.br, b[:]); err != nil {
			return 0, nil, err
		}
		l = binary.BigEndian.Uint64(b[:])
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(c.br, mask[:]); err != nil {
			return 0, nil, err
		}
	}
	if l > 16<<20 {
		return 0, nil, errors.New("websocket frame too large")
	}
	payload := make([]byte, l)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return op, payload, nil
}

func (c *Conn) writeFrame(op byte, payload []byte) error {
	h := []byte{0x80 | op, 0}
	maskBit := byte(0)
	if c.isClient {
		maskBit = 0x80
	}
	l := len(payload)
	switch {
	case l < 126:
		h[1] = maskBit | byte(l)
	case l <= 65535:
		h[1] = maskBit | 126
		var b [2]byte
		binary.BigEndian.PutUint16(b[:], uint16(l))
		h = append(h, b[:]...)
	default:
		h[1] = maskBit | 127
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(l))
		h = append(h, b[:]...)
	}
	if c.isClient {
		var mask [4]byte
		if _, err := rand.Read(mask[:]); err != nil {
			return err
		}
		h = append(h, mask[:]...)
		masked := make([]byte, len(payload))
		for i := range payload {
			masked[i] = payload[i] ^ mask[i%4]
		}
		payload = masked
	}
	if _, err := c.c.Write(h); err != nil {
		return err
	}
	_, err := c.c.Write(payload)
	return err
}

func acceptKey(key string) string {
	sum := sha1.Sum([]byte(key + guid))
	return base64.StdEncoding.EncodeToString(sum[:])
}

func randomKey() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b[:]), nil
}
