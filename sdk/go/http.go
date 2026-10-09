package onprest

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
)

// Transport sends one HTTP hop through the Agent's authorization and spool.
// The zero value is ready for use with requests derived from a Run context.
type Transport struct{}

// NewHTTPClient returns an independent client with redirects disabled.
func NewHTTPClient() *http.Client {
	return &http.Client{Transport: &Transport{}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
func (*Transport) RoundTrip(req *http.Request) (response *http.Response, err error) {
	if req == nil {
		return nil, errors.New("invalid HTTP request")
	}
	var closeBody func() error
	if req.Body != nil {
		var once sync.Once
		var closeErr error
		closeBody = func() error { once.Do(func() { closeErr = req.Body.Close() }); return closeErr }
		stop := context.AfterFunc(req.Context(), func() { _ = closeBody() })
		defer func() {
			stop()
			c := closeBody()
			if err == nil && c != nil {
				if response != nil && response.Body != nil {
					_ = response.Body.Close()
				}
				response = nil
				err = c
			}
		}()
	}
	if req.URL == nil {
		return nil, errors.New("invalid HTTP URL")
	}
	ctx := req.Context()
	e, err := resolve(ctx)
	if err != nil {
		return nil, err
	}
	method := req.Method
	if method == "" {
		method = "GET"
	}
	length := req.ContentLength
	if req.Body == nil {
		length = 0
	} else if length == 0 {
		length = -1
	}
	headers := req.Header
	if headers == nil {
		headers = http.Header{}
	}
	v, err := e.businessCall(ctx, "http.open", map[string]any{"method": method, "url": req.URL.String(), "headers": headers, "content_length": length})
	if err != nil {
		return nil, err
	}
	id := integer(v["request_handle"])
	body := e.own(ctx, integer(v["body_handle"]), "http_write", "", true)
	abort := func() error {
		_, err := e.businessCall(e.ctx, "http.abort", map[string]any{"request_handle": id})
		return err
	}
	defer func() {
		if err != nil {
			_ = abort()
		}
	}()
	if req.Body != nil {
		_, err = io.Copy(body, req.Body)
		if err != nil {
			_ = abort()
			body.mu.Lock()
			body.closed = true
			body.mu.Unlock()
			e.mu.Lock()
			delete(e.owned, body)
			e.mu.Unlock()
			return nil, err
		}
	}
	if err = body.Close(); err != nil {
		return nil, err
	}
	v, err = e.businessCall(ctx, "http.do", map[string]any{"request_handle": id})
	if err != nil {
		return nil, err
	}
	headers = http.Header{}
	if m, ok := v["headers"].(map[string]any); ok {
		for k, v := range m {
			a, ok := v.([]any)
			if !ok {
				return nil, errors.New("invalid HTTP response headers")
			}
			for _, v := range a {
				s, ok := v.(string)
				if !ok {
					return nil, errors.New("invalid HTTP response header")
				}
				headers.Add(k, s)
			}
		}
	}
	reader := e.own(ctx, integer(v["body_handle"]), "http_read", "", false)
	reader.after = abort
	status := int(integer(v["status"]))
	return &http.Response{StatusCode: status, Status: strconv.Itoa(status) + " " + http.StatusText(status), Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Header: headers, Body: reader, ContentLength: integer(v["content_length"]), Request: req}, nil
}

var _ http.RoundTripper = (*Transport)(nil)
