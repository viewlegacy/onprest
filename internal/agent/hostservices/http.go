package hostservices

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/viewlegacy/onprest/internal/agent/artifacts"
	"github.com/viewlegacy/onprest/internal/agent/providers"
	"github.com/viewlegacy/onprest/internal/contract"
)

const chunkBytes = 64 << 10

// Options are private runtime limits. The store must be the execution's shared
// artifact store; a separate HTTP disk budget would bypass the common quota.
type Options struct {
	Context                           context.Context
	Store                             *artifacts.Store
	MaxRequestBytes, MaxResponseBytes int64
	MaxRequests                       int
}
type Service struct {
	mu        sync.Mutex
	ctx       context.Context
	cancel    context.CancelFunc
	stop      func() bool
	store     *artifacts.Store
	auth      authorization
	options   Options
	requests  map[*Request]bool
	closed    bool
	closeDone chan struct{}
	closeErr  error
	// Tests inject DNS/dial/trust at this internal boundary, never through YAML.
	lookup    func(context.Context, string) ([]netip.Addr, error)
	dial      func(context.Context, string, string) (net.Conn, error)
	tlsConfig *tls.Config
}

func newService(o Options, a authorization) (*Service, error) {
	if o.Context == nil || o.Store == nil || o.MaxRequestBytes < 1 || o.MaxResponseBytes < 1 || o.MaxRequests < 1 {
		return nil, invalid(nil)
	}
	ctx, cancel := context.WithCancel(o.Context)
	s := &Service{ctx: ctx, cancel: cancel, store: o.Store, auth: a, options: o, requests: map[*Request]bool{}, closeDone: make(chan struct{}), tlsConfig: &tls.Config{MinVersion: tls.VersionTLS12}, dial: (&net.Dialer{}).DialContext}
	// LookupNetIP's network argument is fixed rather than supplied by callers.
	s.lookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
		return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	}
	s.mu.Lock()
	s.stop = context.AfterFunc(ctx, func() { _ = s.Close() })
	s.mu.Unlock()
	return s, nil
}

// Request belongs to exactly one service lifetime. It is the private analogue
// of a request handle; ABI handle ownership is wired separately by the Wasm host bridge.
type Request struct {
	mu                        sync.Mutex
	service                   *Service
	ctx                       context.Context
	cancel                    context.CancelFunc
	stopLifetime, stopCleanup func() bool
	method                    string
	url                       *url.URL
	headers                   http.Header
	expected, size            int64
	hash                      hash.Hash
	state                     string
	stage, responseStage      contract.ArtifactStage
	reader                    io.ReadCloser
	upstream                  io.ReadCloser
	transport                 *http.Transport
	responseBytes             int64
	effects                   *providers.Effects
	closeErr                  error
}
type Response struct {
	Status        int
	Headers       http.Header
	ContentLength int64
	Body          io.ReadCloser
}

func validToken(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range []byte(s) {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c))) {
			return false
		}
	}
	return true
}
func (s *Service) requestHeaders(in http.Header) (http.Header, error) {
	out := s.auth.headers.Clone()
	if out == nil {
		out = make(http.Header)
	}
	seen := map[string]bool{}
	for key, values := range in {
		low := strings.ToLower(key)
		if !validToken(key) || seen[low] || strings.Contains("|host|connection|proxy-authorization|proxy-authenticate|keep-alive|upgrade|te|trailer|content-length|transfer-encoding|", "|"+low+"|") || s.auth.mode == "fixed" && (low == "authorization" || len(out.Values(key)) > 0) {
			return nil, invalid(nil)
		}
		seen[low] = true
		for _, value := range values {
			if !utf8.ValidString(value) || strings.ContainsAny(value, "\x00\r\n") {
				return nil, invalid(nil)
			}
			out.Add(key, value)
		}
	}
	return out, nil
}

// Open authorizes and allocates a private body stage without sending HTTP.
// A content length of -1 means unknown; zero is an explicitly empty body.
func (s *Service) Open(ctx context.Context, method, address string, headers http.Header, length int64, effects *providers.Effects) (*Request, error) {
	if length > s.options.MaxRequestBytes {
		return nil, failure("LIMIT_EXCEEDED", nil)
	}
	if ctx == nil || length < -1 {
		return nil, invalid(nil)
	}
	u, err := parseURL(address)
	if err != nil {
		return nil, err
	}
	h, err := s.requestHeaders(headers)
	if err != nil {
		return nil, err
	}
	control := map[string]any{"method": method, "url": address, "headers": headerJSON(h), "content_length": length}
	bytes, err := contract.CanonicalJSON(control)
	if err != nil || len(bytes) > chunkBytes {
		return nil, invalid(err)
	}
	requestContext, cancel := context.WithCancel(ctx)
	stopLifetime := context.AfterFunc(s.ctx, cancel)
	if s.ctx.Err() != nil {
		cancel()
	}
	if _, err = s.destination(requestContext, method, u); err != nil {
		stopLifetime()
		cancel()
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || requestContext.Err() != nil {
		stopLifetime()
		cancel()
		return nil, transportError(requestContext, requestContext.Err())
	}
	if len(s.requests) >= s.options.MaxRequests {
		stopLifetime()
		cancel()
		return nil, failure("LIMIT_EXCEEDED", nil)
	}
	stage, err := s.store.NewScratch(requestContext, "http-request.bin", "application/octet-stream", max(length, 0))
	if err != nil {
		failure := transportError(requestContext, err)
		stopLifetime()
		cancel()
		return nil, failure
	}
	r := &Request{service: s, ctx: requestContext, cancel: cancel, stopLifetime: stopLifetime, method: method, url: u, headers: h, expected: length, state: "receiving", stage: stage, hash: sha256.New(), effects: effects}
	s.requests[r] = true
	r.mu.Lock()
	r.stopCleanup = context.AfterFunc(requestContext, func() { _ = r.Close() })
	r.mu.Unlock()
	return r, nil
}
func (r *Request) Write(p []byte) (n int, err error) {
	defer func() {
		if err != nil {
			r.mu.Lock()
			failed := r.state == "failed"
			r.mu.Unlock()
			if failed {
				err = errors.Join(err, r.Close())
			}
		}
	}()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state != "receiving" {
		return 0, invalid(nil)
	}
	if r.ctx.Err() != nil {
		return 0, transportError(r.ctx, r.ctx.Err())
	}
	if len(p) > chunkBytes || int64(len(p)) > r.service.options.MaxRequestBytes-r.size || r.expected >= 0 && int64(len(p)) > r.expected-r.size {
		r.state = "failed"
		return 0, failure("LIMIT_EXCEEDED", nil)
	}
	n, err = r.stage.Write(p)
	r.size += int64(n)
	_, _ = r.hash.Write(p[:n])
	if err != nil {
		r.state = "failed"
		return n, transportError(r.ctx, err)
	}
	return n, nil
}

// Ready is body-writer close, not request close. It validates length and flushes
// the complete request before Do can send even the first HTTP header.
func (r *Request) Ready() (err error) {
	defer func() {
		if err != nil {
			r.mu.Lock()
			failed := r.state == "failed"
			r.mu.Unlock()
			if failed {
				err = errors.Join(err, r.Close())
			}
		}
	}()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state != "receiving" {
		return invalid(nil)
	}
	if r.ctx.Err() != nil {
		r.state = "failed"
		return transportError(r.ctx, r.ctx.Err())
	}
	if r.expected >= 0 && r.expected != r.size {
		r.state = "failed"
		return invalid(nil)
	}
	if err := r.stage.Close(); err != nil {
		r.state = "failed"
		return transportError(r.ctx, err)
	}
	r.state = "ready"
	return nil
}

// CopyBody consumes and closes an owned reader with one reusable finite buffer.
// Reader/Close errors abort rather than sending a successfully copied prefix.
func (r *Request) CopyBody(body io.ReadCloser) error {
	if body == nil {
		return r.failed(invalid(nil))
	}
	buf := make([]byte, chunkBytes)
	var closeOnce sync.Once
	var closeErr error
	closeBody := func() { closeOnce.Do(func() { closeErr = body.Close() }) }
	stop := context.AfterFunc(r.ctx, closeBody)
	var err error
	empty := 0
	for {
		var n int
		n, err = body.Read(buf)
		if n < 0 || n > len(buf) {
			err = io.ErrShortBuffer
			break
		}
		if n > 0 {
			empty = 0
			if _, e := r.Write(buf[:n]); e != nil {
				err = e
				break
			}
		} else {
			empty++
		}
		if err == io.EOF {
			err = nil
			break
		}
		if err != nil {
			break
		}
		if empty >= 100 {
			err = io.ErrNoProgress
			break
		}
	}
	stop()
	closeBody()
	err = errors.Join(err, closeErr)
	if err == nil && r.ctx.Err() != nil {
		err = r.ctx.Err()
	}
	if err == nil {
		err = r.Ready()
	}
	if err != nil {
		return errors.Join(transportError(r.ctx, err), r.Close())
	}
	return nil
}

func (r *Request) Do() (*Response, error) {
	r.mu.Lock()
	if r.state != "ready" {
		r.mu.Unlock()
		return nil, invalid(nil)
	}
	r.state = "sending" // Irreversible claim: another Do never retries this request.
	r.mu.Unlock()
	ip, err := r.service.destination(r.ctx, r.method, r.url)
	if err != nil {
		return nil, r.failed(err)
	}
	r.mu.Lock()
	if r.state != "sending" || r.ctx.Err() != nil {
		r.mu.Unlock()
		return nil, r.failed(transportError(r.ctx, r.ctx.Err()))
	}
	// Finish verifies the length/digest accumulated during bounded writes.
	reader, err := r.stage.Finish(contract.ArtifactMetadata{Name: "http-request.bin", MIMEType: "application/octet-stream", SizeBytes: r.size, SHA256: hex.EncodeToString(r.hash.Sum(nil))})
	if err != nil {
		r.mu.Unlock()
		return nil, r.failed(transportError(r.ctx, err))
	}
	r.reader = reader
	port := r.url.Port()
	if port == "" {
		if r.url.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, ForceAttemptHTTP2: false, DisableCompression: true, MaxResponseHeaderBytes: chunkBytes, TLSHandshakeTimeout: 10 * time.Second, TLSClientConfig: r.service.tlsConfig.Clone(), DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		return r.service.dial(ctx, "tcp", net.JoinHostPort(ip.String(), port))
	}}
	r.transport = transport
	r.mu.Unlock()
	trace := &httptrace.ClientTrace{WroteHeaders: func() {
		if r.effects != nil {
			r.effects.Start()
			r.effects.Begin()
		}
	}}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(r.ctx, trace), r.method, r.url.String(), reader)
	if err != nil {
		return nil, r.failed(invalid(err))
	}
	req.Header = r.headers.Clone()
	req.ContentLength = r.size
	if r.size == 0 {
		req.Body = http.NoBody
		_ = reader.Close()
	}
	// GetBody is deliberately nil, and every transport is fresh with keepalive
	// disabled. net/http has no reused connection on which to retry implicitly.
	response, err := transport.RoundTrip(req)
	if err != nil {
		return nil, r.failed(transportError(r.ctx, err))
	}
	r.mu.Lock()
	if r.state != "sending" || r.ctx.Err() != nil {
		r.mu.Unlock()
		_ = response.Body.Close()
		return nil, r.failed(transportError(r.ctx, r.ctx.Err()))
	}
	r.upstream = response.Body
	if response.ContentLength > r.service.options.MaxResponseBytes {
		r.mu.Unlock()
		return nil, r.failed(failure("LIMIT_EXCEEDED", nil))
	}
	control, err := contract.CanonicalJSON(map[string]any{"status": int64(response.StatusCode), "headers": headerJSON(response.Header), "content_length": response.ContentLength})
	if err != nil || len(control) > chunkBytes {
		r.mu.Unlock()
		return nil, r.failed(failure("LIMIT_EXCEEDED", err))
	}
	stage, err := r.service.store.NewScratch(r.ctx, "http-response.bin", "application/octet-stream", max(response.ContentLength, 0))
	if err != nil {
		r.mu.Unlock()
		return nil, r.failed(transportError(r.ctx, err))
	}
	r.responseStage = stage
	r.state = "response"
	r.mu.Unlock()
	return &Response{Status: response.StatusCode, Headers: response.Header.Clone(), ContentLength: response.ContentLength, Body: r}, nil
}

func (r *Request) Read(p []byte) (int, error) {
	r.mu.Lock()
	if r.state == "eof" {
		r.mu.Unlock()
		return 0, io.EOF
	}
	if r.state != "response" {
		r.mu.Unlock()
		return 0, invalid(nil)
	}
	if len(p) == 0 {
		r.mu.Unlock()
		return 0, nil
	}
	if len(p) > chunkBytes {
		p = p[:chunkBytes]
	}
	remaining := r.service.options.MaxResponseBytes - r.responseBytes
	// One extra byte distinguishes exact-limit EOF from an unknown-length
	// oversized body; it is never returned or stored above the policy budget.
	if remaining < int64(len(p))-1 {
		p = p[:remaining+1]
	}
	n, err := r.upstream.Read(p)
	if int64(n) > remaining {
		r.mu.Unlock()
		return 0, r.failed(failure("LIMIT_EXCEEDED", nil))
	}
	if n > 0 {
		written, writeErr := r.responseStage.Write(p[:n])
		r.responseBytes += int64(written)
		if writeErr != nil || written != n {
			r.mu.Unlock()
			return 0, r.failed(transportError(r.ctx, errors.Join(writeErr, io.ErrShortWrite)))
		}
	}
	if err != nil && err != io.EOF {
		r.mu.Unlock()
		return 0, r.failed(transportError(r.ctx, err))
	}
	if err == io.EOF {
		if closeErr := r.responseStage.Close(); closeErr != nil {
			r.mu.Unlock()
			return 0, r.failed(transportError(r.ctx, closeErr))
		}
		r.state = "eof"
	}
	r.mu.Unlock()
	return n, err
}
func (r *Request) failed(err error) error { return errors.Join(err, r.Close()) }

// Close aborts receiving/sending requests or closes responses. Cancel precedes
// locking so an in-progress socket Read can exit and release its ownership.
func (r *Request) Close() error {
	r.cancel()
	r.stopLifetime()
	r.mu.Lock()
	if r.state == "closed" {
		err := r.closeErr
		r.mu.Unlock()
		return err
	}
	r.state = "closed"
	if r.stopCleanup != nil {
		r.stopCleanup()
	}
	var err error
	if r.upstream != nil {
		err = errors.Join(err, r.upstream.Close())
	}
	if r.reader != nil {
		err = errors.Join(err, r.reader.Close())
	}
	if r.responseStage != nil {
		err = errors.Join(err, r.responseStage.Abort())
	}
	if r.stage != nil {
		err = errors.Join(err, r.stage.Abort())
	}
	if r.transport != nil {
		r.transport.CloseIdleConnections()
	}
	if err != nil {
		r.closeErr = failure("EXECUTION_FAILED", err)
	}
	err = r.closeErr
	r.service.mu.Lock()
	delete(r.service.requests, r)
	r.service.mu.Unlock()
	r.mu.Unlock()
	return err
}
func (s *Service) Close() error {
	s.cancel()
	s.mu.Lock()
	if s.closed {
		done := s.closeDone
		s.mu.Unlock()
		<-done
		s.mu.Lock()
		err := s.closeErr
		s.mu.Unlock()
		return err
	}
	s.closed = true
	if s.stop != nil {
		s.stop()
	}
	requests := make([]*Request, 0, len(s.requests))
	for r := range s.requests {
		requests = append(requests, r)
	}
	s.mu.Unlock()
	var err error
	for _, r := range requests {
		err = errors.Join(err, r.Close())
	}
	s.mu.Lock()
	s.closeErr = err
	close(s.closeDone)
	s.mu.Unlock()
	return err
}

func headerJSON(h http.Header) map[string]any {
	out := map[string]any{}
	for k, v := range h {
		out[k] = append([]string(nil), v...)
	}
	return out
}
