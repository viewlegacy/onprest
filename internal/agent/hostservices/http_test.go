package hostservices

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"github.com/viewlegacy/onprest/internal/agent/artifacts"
	"github.com/viewlegacy/onprest/internal/agent/providers"
	"github.com/viewlegacy/onprest/internal/contract"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func fixture(t *testing.T, quota int64) (*Service, *artifacts.Store) {
	t.Helper()
	store, e := artifacts.Open(artifacts.Options{Directory: filepath.Join(t.TempDir(), "spool"), QuotaBytes: quota, MaxPending: 8, MaxObjects: 128, TransferTimeout: time.Minute})
	if e != nil {
		t.Fatal(e)
	}
	s, e := WasmOFF(Options{context.Background(), store, 1 << 20, 1 << 20, 64})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if e := s.Close(); e != nil {
			t.Error(e)
		}
		if e := store.Close(); e != nil {
			t.Error(e)
		}
	})
	return s, store
}
func wantCode(t *testing.T, e error, want string) {
	t.Helper()
	var f *providers.Failure
	if !errors.As(e, &f) || f.Code != want {
		t.Fatalf("error %v want %s", e, want)
	}
}
func request(t *testing.T, s *Service, address string, length int64, effects *providers.Effects) *Request {
	t.Helper()
	r, e := s.Open(context.Background(), "POST", address, http.Header{"Content-Type": {"application/octet-stream"}}, length, effects)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}
func complete(t *testing.T, r *Request, b []byte) *Response {
	t.Helper()
	if e := r.CopyBody(io.NopCloser(bytes.NewReader(b))); e != nil {
		t.Fatal(e)
	}
	resp, e := r.Do()
	if e != nil {
		t.Fatal(e)
	}
	return resp
}
func empty(t *testing.T, s *Service, store *artifacts.Store) {
	t.Helper()
	if store.Usage() != 0 {
		t.Fatal("quota retained", store.Usage())
	}
	s.mu.Lock()
	n := len(s.requests)
	s.mu.Unlock()
	if n != 0 {
		t.Fatal("requests retained", n)
	}
}
func TestHTTPBodyBarrierBytesAndStatus(t *testing.T) {
	for _, size := range []int{0, 1, 65535, 65536, 65537, 180123} {
		for _, known := range []bool{false, true} {
			t.Run(fmt.Sprintf("size_%d_known_%t", size, known), func(t *testing.T) {
				s, store := fixture(t, 4<<20)
				var sends atomic.Int64
				b := bytes.Repeat([]byte{0, 255, 'x', '\r', '\n'}, (size+4)/5)[:size]
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					sends.Add(1)
					got, e := io.ReadAll(r.Body)
					if e != nil || !bytes.Equal(got, b) || r.ContentLength != int64(size) {
						t.Error("bytes/length", len(got), e)
					}
					w.WriteHeader(418)
					_, _ = w.Write(got)
				}))
				defer server.Close()
				length := int64(-1)
				if known {
					length = int64(size)
				}
				effects := &providers.Effects{}
				r := request(t, s, server.URL, length, effects)
				if _, e := r.Do(); e == nil {
					t.Fatal("receiving sent")
				}
				if sends.Load() != 0 {
					t.Fatal("before ready")
				}
				resp := complete(t, r, b)
				if resp.Status != 418 {
					t.Fatal(resp.Status)
				}
				got, e := io.ReadAll(resp.Body)
				if e != nil || !bytes.Equal(got, b) {
					t.Fatal("response mismatch", e)
				}
				if _, e = resp.Body.Read(make([]byte, 1)); e != io.EOF {
					t.Fatal("repeat EOF", e)
				}
				if _, e = r.Do(); e == nil {
					t.Fatal("duplicate do")
				}
				if _, e = r.Write([]byte{1}); e == nil {
					t.Fatal("late write")
				}
				if e = resp.Body.Close(); e != nil {
					t.Fatal(e)
				}
				if sends.Load() != 1 || effects.Outcome() != "unknown" {
					t.Fatal("send/outcome", sends.Load(), effects.Outcome())
				}
				empty(t, s, store)
			})
		}
	}
}

type brokenReader struct {
	io.Reader
	readError, closeError bool
	closed                int
	read                  bool
}

func (r *brokenReader) Read(p []byte) (int, error) {
	if r.Reader != nil {
		return r.Reader.Read(p)
	}
	if !r.read {
		r.read = true
		copy(p, "prefix")
		return 6, nil
	}
	return 0, errors.New("private-reader-secret")
}
func (r *brokenReader) Close() error {
	r.closed++
	if r.closeError {
		return errors.New("private-close-secret")
	}
	return nil
}
func TestHTTPBodyFailuresNeverSendAndReclaim(t *testing.T) {
	for _, name := range []string{"reader", "close", "short_length", "long_length", "write_chunk", "request_limit", "quota_open", "quota_write", "cancel", "deadline", "ready_twice"} {
		t.Run(name, func(t *testing.T) {
			s, store := fixture(t, 64)
			s.options.MaxRequestBytes = 32
			var sends atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sends.Add(1) }))
			defer server.Close()
			length := int64(-1)
			if name == "short_length" {
				length = 7
			}
			if name == "long_length" {
				length = 1
			}
			effects := &providers.Effects{}
			r := request(t, s, server.URL, length, effects)
			var e error
			switch name {
			case "reader", "close":
				b := &brokenReader{}
				if name == "close" {
					b.Reader = strings.NewReader("body")
					b.closeError = true
				}
				e = r.CopyBody(b)
				if b.closed != 1 {
					t.Fatal("reader close", b.closed)
				}
			case "short_length", "long_length":
				e = r.CopyBody(io.NopCloser(strings.NewReader("body")))
			case "write_chunk":
				_, e = r.Write(make([]byte, chunkBytes+1))
			case "request_limit":
				_, e = r.Write(make([]byte, 33))
				wantCode(t, e, "LIMIT_EXCEEDED")
			case "quota_open", "quota_write":
				lease, x := store.Reserve(64)
				if x != nil {
					t.Fatal(x)
				}
				if name == "quota_open" {
					_, e = s.Open(context.Background(), "POST", server.URL, nil, 1, effects)
				} else {
					_, e = r.Write([]byte{1})
				}
				lease.Release()
				wantCode(t, e, "LIMIT_EXCEEDED")
			case "cancel":
				r.cancel()
				e = r.Ready()
			case "deadline":
				ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				defer cancel()
				_, e = s.Open(ctx, "POST", server.URL, nil, 0, effects)
			case "ready_twice":
				if x := r.Ready(); x != nil {
					t.Fatal(x)
				}
				e = r.Ready()
			}
			if e == nil {
				t.Fatal("expected failure")
			}
			_ = r.Close()
			if sends.Load() != 0 || effects.Outcome() != "not_started" {
				t.Fatal("failure sent", sends.Load(), effects.Outcome())
			}
			empty(t, s, store)
		})
	}
}
func TestHTTPResponseLimitsPartialFailureAndQuota(t *testing.T) {
	for _, mode := range []string{"known_minus", "known_exact", "known_plus", "unknown_minus", "unknown_exact", "unknown_plus", "quota", "partial"} {
		t.Run(mode, func(t *testing.T) {
			s, store := fixture(t, 128)
			s.options.MaxResponseBytes = 16
			size := 16
			if strings.HasSuffix(mode, "minus") {
				size--
			}
			if strings.HasSuffix(mode, "plus") {
				size++
			}
			var sends atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sends.Add(1)
				if mode == "partial" {
					c, _, e := w.(http.Hijacker).Hijack()
					if e != nil {
						return
					}
					_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\na")
					_ = c.Close()
					return
				}
				if strings.HasPrefix(mode, "known") || mode == "quota" {
					w.Header().Set("Content-Length", fmt.Sprint(size))
				} else {
					w.(http.Flusher).Flush()
				}
				_, _ = w.Write(bytes.Repeat([]byte{'x'}, size))
			}))
			defer server.Close()
			r := request(t, s, server.URL, 0, &providers.Effects{})
			if e := r.Ready(); e != nil {
				t.Fatal(e)
			}
			var lease *artifacts.Reservation
			if mode == "quota" {
				lease, _ = store.Reserve(128)
			}
			resp, e := r.Do()
			if e == nil {
				_, e = io.ReadAll(resp.Body)
				_ = resp.Body.Close()
			}
			if strings.HasSuffix(mode, "plus") || mode == "quota" {
				wantCode(t, e, "LIMIT_EXCEEDED")
			} else if mode == "partial" {
				if e == nil {
					t.Fatal("partial success")
				}
			} else if e != nil {
				t.Fatal(e)
			}
			if lease != nil {
				lease.Release()
			}
			_ = r.Close()
			if sends.Load() != 1 {
				t.Fatal("retry", sends.Load())
			}
			empty(t, s, store)
		})
	}
}
func TestHTTPCancelSendReadAndServiceLifetime(t *testing.T) {
	for _, mode := range []string{"headers", "read", "service", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			s, store := fixture(t, 1<<20)
			started := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "read" {
					w.(http.Flusher).Flush()
				}
				close(started)
				<-r.Context().Done()
			}))
			defer server.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "deadline" {
				ctx, cancel = context.WithTimeout(context.Background(), 300*time.Millisecond)
				defer cancel()
			}
			effects := &providers.Effects{}
			r, e := s.Open(ctx, "POST", server.URL, nil, 0, effects)
			if e != nil {
				t.Fatal(e)
			}
			if e = r.Ready(); e != nil {
				t.Fatal(e)
			}
			done := make(chan error, 1)
			go func() {
				resp, x := r.Do()
				if x == nil {
					_, x = resp.Body.Read(make([]byte, 1))
					_ = resp.Body.Close()
				}
				done <- x
			}()
			<-started
			if mode == "service" {
				if e = s.Close(); e != nil {
					t.Fatal(e)
				}
			} else if mode != "deadline" {
				cancel()
			}
			select {
			case e = <-done:
				if e == nil {
					t.Fatal("cancel succeeded")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("cancel stuck")
			}
			if effects.Outcome() != "unknown" {
				t.Fatal(effects.Outcome())
			}
			_ = r.Close()
			empty(t, s, store)
		})
	}
}
func TestHTTPNoRetryAndNoAmbientProxy(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		c, _, _ := w.(http.Hijacker).Hijack()
		_ = c.Close()
	}))
	defer server.Close()
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	s, store := fixture(t, 1<<20)
	r := request(t, s, server.URL, 0, &providers.Effects{})
	_ = r.Ready()
	_, e := r.Do()
	if e == nil {
		t.Fatal("expected reset")
	}
	if _, e = r.Do(); e == nil {
		t.Fatal("retried")
	}
	if calls.Load() != 1 {
		t.Fatal("attempts", calls.Load())
	}
	empty(t, s, store)
}
func preparedSource(t *testing.T, base, extra, dir string) *providers.Snapshot {
	t.Helper()
	cfg, e := contract.Compile([]byte("schema_version: 1\ngateway: {url: wss://gateway.example/ws/agent, agent_private_key_file: ./key}\nsources:\n  http:\n    upstream: {base_url: '" + base + "', allowed_methods: [GET, POST], " + extra + "}\ncapabilities:\n  http:\n    executor: {type: http, source: upstream, method: POST, path: /body, response: {kind: json}}\n    output: {data: {}}\n"))
	if e != nil {
		t.Fatal(e)
	}
	snapshot, e := providers.Resolve(context.Background(), cfg, filepath.Join(dir, "agent.yaml"), "http", providers.ResolveOptions{})
	if e != nil {
		t.Fatal(e)
	}
	return snapshot
}
func fixed(t *testing.T, s *Service, base, extra string) *Service {
	t.Helper()
	f, e := FixedSource(preparedSource(t, base, extra, t.TempDir()), s.options)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}
func TestHTTPFixedSecretsSnapshotAndTLS(t *testing.T) {
	var auth atomic.Value
	var sends atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sends.Add(1)
		auth.Store(r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, "response")
	}))
	defer server.Close()
	s, store := fixture(t, 1<<20)
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	writeProtectedTestFile(t, path, []byte("private-token"))
	inline := preparedSource(t, server.URL, "allowed_cidrs: [127.0.0.0/8], credential: private-token", dir)
	file := preparedSource(t, server.URL, "allowed_cidrs: [127.0.0.0/8], credential_file: ./token", dir)
	if inline.DefinitionHash() != file.DefinitionHash() {
		t.Fatal("source mode hash")
	}
	_ = os.WriteFile(path, []byte("changed-token"), 0600)
	changed := preparedSource(t, server.URL, "allowed_cidrs: [127.0.0.0/8], credential_file: ./token", dir)
	if changed.DefinitionHash() == file.DefinitionHash() {
		t.Fatal("secret hash unchanged")
	}
	source, _ := file.Source(providers.SourceKey{Group: "http", Name: "upstream"})
	source["credential"] = "mutated"
	for _, snapshot := range []*providers.Snapshot{inline, file, changed} {
		f, e := FixedSource(snapshot, s.options)
		if e != nil {
			t.Fatal(e)
		}
		roots := x509.NewCertPool()
		roots.AddCert(server.Certificate())
		f.tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
		r := request(t, f, server.URL, 0, &providers.Effects{})
		resp := complete(t, r, nil)
		b, e := io.ReadAll(resp.Body)
		if e != nil || string(b) != "response" {
			t.Fatal(e, string(b))
		}
		_ = resp.Body.Close()
		want := "Bearer private-token"
		if snapshot == changed {
			want = "Bearer changed-token"
		}
		if auth.Load() != want {
			t.Fatal("credential snapshot", auth.Load())
		}
		_ = f.Close()
	}
	f := fixed(t, s, server.URL, "allowed_cidrs: [127.0.0.0/8]")
	r := request(t, f, server.URL, 0, &providers.Effects{})
	_ = r.Ready()
	_, e := r.Do()
	if e == nil {
		t.Fatal("untrusted TLS accepted")
	}
	if sends.Load() != 3 {
		t.Fatal("TLS rejection sent HTTP")
	}
	empty(t, s, store)
}
func TestHTTPAuthorizationContextsDNSAndIP(t *testing.T) {
	s, store := fixture(t, 1<<20)
	var sends atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sends.Add(1) }))
	defer server.Close()
	f := fixed(t, s, "http://upstream.example", "allowed_cidrs: [10.0.0.0/8]")
	var resolutions atomic.Int64
	var dialed atomic.Value
	f.lookup = func(context.Context, string) ([]netip.Addr, error) {
		if resolutions.Add(1) == 1 {
			return []netip.Addr{netip.MustParseAddr("10.1.2.3")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	f.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		dialed.Store(address)
		return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(server.URL, "http://"))
	}
	r := request(t, f, "http://upstream.example/body", 0, &providers.Effects{})
	_ = r.Ready()
	_, e := r.Do()
	wantCode(t, e, "PERMISSION_DENIED")
	if sends.Load() != 0 || dialed.Load() != nil {
		t.Fatal("rebinding sent")
	}
	f.lookup = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("10.1.2.3")}, nil
	}
	r = request(t, f, "http://upstream.example/body", 0, nil)
	resp := complete(t, r, nil)
	_ = resp.Body.Close()
	if sends.Load() != 1 || dialed.Load() != "10.1.2.3:80" {
		t.Fatal("unpinned dial", dialed.Load())
	}
	for _, address := range []string{"http://other.example/body", "http://upstream.example:81/body"} {
		_, e = f.Open(context.Background(), "POST", address, nil, 0, nil)
		wantCode(t, e, "PERMISSION_DENIED")
	}
	for _, method := range []string{"DELETE", "post"} {
		_, e = f.Open(context.Background(), method, "http://upstream.example/body", nil, 0, nil)
		wantCode(t, e, "PERMISSION_DENIED")
	}
	for _, tc := range []struct {
		ip    string
		allow bool
	}{{"10.1.2.3", true}, {"172.16.2.1", true}, {"192.168.1.1", true}, {"8.8.8.8", true}, {"127.0.0.1", false}, {"::1", false}, {"169.254.169.254", false}, {"fe80::1", false}, {"224.0.0.1", false}, {"ff02::1", false}, {"0.0.0.0", false}, {"::ffff:127.0.0.1", false}} {
		t.Run(tc.ip, func(t *testing.T) {
			if ipAllowed(netip.MustParseAddr(tc.ip), nil, false, false) != tc.allow {
				t.Fatal(tc.ip)
			}
		})
	}
	for _, ip := range []string{"127.0.0.1", "169.254.169.254", "::1", "fe80::1"} {
		a := netip.MustParseAddr(ip)
		if !ipAllowed(a, []netip.Prefix{netip.PrefixFrom(a, a.BitLen())}, true, false) {
			t.Fatal("explicit rejected", ip)
		}
	}
	on, e := WasmON([]any{map[string]any{"origin": server.URL, "methods": []string{"POST"}, "path_prefix": "/allowed", "allowed_cidrs": []string{"127.0.0.0/8"}}}, s.options)
	if e != nil {
		t.Fatal(e)
	}
	defer on.Close()
	for _, path := range []string{"/allowed", "/allowed/child"} {
		r = request(t, on, server.URL+path, 0, nil)
		resp = complete(t, r, nil)
		_ = resp.Body.Close()
	}
	for _, path := range []string{"/allowed-other", "/other", "/allowed/../other", "/allowed/%2e%2e/other"} {
		_, e = on.Open(context.Background(), "POST", server.URL+path, nil, 0, nil)
		wantCode(t, e, "PERMISSION_DENIED")
	}
	deny, e := WasmON(nil, s.options)
	if e != nil {
		t.Fatal(e)
	}
	defer deny.Close()
	_, e = deny.Open(context.Background(), "POST", server.URL, nil, 0, nil)
	wantCode(t, e, "PERMISSION_DENIED")
	_ = f.Close()
	_ = on.Close()
	empty(t, s, store)
}
func TestHTTPExplicitRedirectChainCredentialAndHopGuards(t *testing.T) {
	var calls, credentials atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") == "Bearer token" {
			credentials.Add(1)
		}
		if r.URL.Path == "/a" {
			http.Redirect(w, r, "/b", 302)
			return
		}
		if r.URL.Path == "/b" {
			http.Redirect(w, r, "/c", 307)
			return
		}
		_, _ = io.WriteString(w, "done")
	}))
	defer server.Close()
	s, store := fixture(t, 1<<20)
	f := fixed(t, s, server.URL, "allowed_cidrs: [127.0.0.0/8], credential: token")
	target := server.URL + "/a"
	for hop := 0; hop < 3; hop++ {
		r := request(t, f, target, 0, nil)
		resp := complete(t, r, nil)
		if hop < 2 {
			next, e := f.RedirectTarget(context.Background(), "POST", target, resp.Headers.Get("Location"), hop+1, true, 2)
			if e != nil {
				t.Fatal(e)
			}
			target = next
		} else {
			b, e := io.ReadAll(resp.Body)
			if e != nil || string(b) != "done" {
				t.Fatal(e, string(b))
			}
		}
		_ = resp.Body.Close()
	}
	if calls.Load() != 3 || credentials.Load() != 3 {
		t.Fatal("chain count/credential", calls.Load(), credentials.Load())
	}
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("cross origin sent") }))
	defer other.Close()
	for _, tc := range []struct {
		location string
		hop      int
		enabled  bool
	}{{"/b", 1, false}, {"/b", 3, true}, {other.URL, 1, true}, {"http://user:secret@example.com", 1, true}} {
		_, e := f.RedirectTarget(context.Background(), "POST", server.URL+"/a", tc.location, tc.hop, tc.enabled, 2)
		if e == nil {
			t.Fatal("bad redirect accepted", tc)
		}
	}
	_ = f.Close()
	empty(t, s, store)
}
func TestHTTPParallelDoAndIndependentEffects(t *testing.T) {
	s, store := fixture(t, 1<<20)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); _, _ = io.WriteString(w, "ok") }))
	defer server.Close()
	effects := &providers.Effects{}
	effects.Start()
	effects.Begin().Committed()
	r := request(t, s, server.URL, 0, effects)
	_ = r.Ready()
	var wg sync.WaitGroup
	var success atomic.Int64
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, e := r.Do()
			if e == nil {
				success.Add(1)
				_, _ = io.ReadAll(resp.Body)
				_ = resp.Body.Close()
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 || success.Load() != 1 || effects.Outcome() != "unknown" {
		t.Fatal(calls.Load(), success.Load(), effects.Outcome())
	}
	prior := &providers.Effects{}
	prior.Start()
	prior.Begin().Committed()
	r = request(t, s, server.URL, 2, prior)
	_ = r.CopyBody(io.NopCloser(strings.NewReader("short")))
	_ = r.Close()
	if prior.Outcome() != "committed" {
		t.Fatal("previous effect erased")
	}
	empty(t, s, store)
}
func TestHTTPHeaderAndControlNegativeBoundaries(t *testing.T) {
	s, store := fixture(t, 1<<20)
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	for _, headers := range []http.Header{{"Host": {"other"}}, {"Connection": {"keep-alive"}}, {"Transfer-Encoding": {"chunked"}}, {"Content-Length": {"1"}}, {"Bad Name": {"x"}}, {"X": {"value\r\nInjection: bad"}}, {"X": {"x"}, "x": {"x"}}, {"X": {"\xff"}}, {"X": {strings.Repeat("x", 65536)}}} {
		_, e := s.Open(context.Background(), "POST", server.URL, headers, 0, nil)
		if e == nil {
			t.Fatal("bad header accepted")
		}
	}
	f := fixed(t, s, server.URL, "allowed_cidrs: [127.0.0.0/8], headers: {X-Fixed: abc}, credential: token")
	for _, headers := range []http.Header{{"Authorization": {"attacker"}}, {"X-Fixed": {"abc"}}} {
		_, e := f.Open(context.Background(), "POST", server.URL, headers, 0, nil)
		if e == nil {
			t.Fatal("fixed overwritten")
		}
	}
	for _, address := range []string{"file:///etc/passwd", "http://user:pass@host/", "http://host/#fragment", "http://host/%zz", "http://[fe80::1%25eth0]/"} {
		_, e := s.Open(context.Background(), "POST", address, nil, 0, nil)
		if e == nil {
			t.Fatal("bad URL", address)
		}
	}
	empty(t, s, store)
}

func TestHTTPServiceBoundsAndConcurrentCancelCleanup(t *testing.T) {
	s, store := fixture(t, 1<<20)
	s.options.MaxRequests = 1
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	r := request(t, s, server.URL, 12, nil)
	if store.Usage() != 12 {
		t.Fatal("known length not reserved", store.Usage())
	}
	_, e := s.Open(context.Background(), "POST", server.URL, nil, 0, nil)
	wantCode(t, e, "LIMIT_EXCEEDED")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = r.Close() }()
	}
	wg.Wait()
	empty(t, s, store)
	ctx, cancel := context.WithCancel(context.Background())
	s2, e := WasmOFF(Options{ctx, store, 1 << 20, 1 << 20, 64})
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 32; i++ {
		r = request(t, s2, server.URL, 1, nil)
		if _, e = r.Write([]byte{1}); e != nil {
			t.Fatal(e)
		}
	}
	cancel()
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e := s2.Close(); e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	empty(t, s2, store)
	if _, e = s2.Open(context.Background(), "POST", server.URL, nil, 0, nil); e == nil {
		t.Fatal("closed service accepted")
	}
}

func TestHTTPTLSHostVerificationAndDeniedDNS(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer server.Close()
	s, store := fixture(t, 1<<20)
	s.lookup = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	s.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(server.URL, "https://"))
	}
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	s.tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	r := request(t, s, "https://wrong-certificate.example/body", 0, nil)
	_ = r.Ready()
	_, e := r.Do()
	if e == nil || calls.Load() != 0 {
		t.Fatal("host verification bypass", e, calls.Load())
	}
	empty(t, s, store)
	deny, e := WasmON(nil, s.options)
	if e != nil {
		t.Fatal(e)
	}
	defer deny.Close()
	var lookups atomic.Int64
	deny.lookup = func(context.Context, string) ([]netip.Addr, error) {
		lookups.Add(1)
		return nil, errors.New("should not resolve")
	}
	_, e = deny.Open(context.Background(), "POST", "https://ungranted.example/", nil, 0, nil)
	wantCode(t, e, "PERMISSION_DENIED")
	if lookups.Load() != 0 {
		t.Fatal("ungranted DNS")
	}
}

func TestHTTPConfiguredRequestAndControlBoundaries(t *testing.T) {
	for _, length := range []int64{-1, 15, 16, 17} {
		t.Run(fmt.Sprint(length), func(t *testing.T) {
			s, store := fixture(t, 128)
			s.options.MaxRequestBytes = 16
			var sends atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sends.Add(1)
				b, e := io.ReadAll(r.Body)
				if e != nil || len(b) != 16 {
					t.Error(len(b), e)
				}
			}))
			defer server.Close()
			r, e := s.Open(context.Background(), "POST", server.URL, nil, length, nil)
			if length == 17 {
				wantCode(t, e, "LIMIT_EXCEEDED")
			} else {
				if e != nil {
					t.Fatal(e)
				}
				e = r.CopyBody(io.NopCloser(bytes.NewReader(make([]byte, 16))))
				if length == 15 {
					if e == nil {
						t.Fatal("declared length bypass")
					}
				} else {
					if e != nil {
						t.Fatal(e)
					}
					resp, e := r.Do()
					if e != nil {
						t.Fatal(e)
					}
					_ = resp.Body.Close()
				}
				_ = r.Close()
			}
			want := int64(0)
			if length == -1 || length == 16 {
				want = 1
			}
			if sends.Load() != want {
				t.Fatal(sends.Load(), want)
			}
			empty(t, s, store)
		})
	}
	s, store := fixture(t, 1<<20)
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	h := http.Header{"X": {""}}
	raw, e := contract.CanonicalJSON(map[string]any{"method": "POST", "url": server.URL, "headers": headerJSON(h), "content_length": int64(0)})
	if e != nil {
		t.Fatal(e)
	}
	for _, delta := range []int{-1, 0, 1} {
		h.Set("X", strings.Repeat("x", chunkBytes-len(raw)+delta))
		r, e := s.Open(context.Background(), "POST", server.URL, h, 0, nil)
		if delta <= 0 {
			if e != nil {
				t.Fatal("inbound control boundary", delta, e)
			}
			_ = r.Close()
		} else if e == nil {
			t.Fatal("control exceeded")
		}
	}
	empty(t, s, store)
}

type blockingBody struct {
	started, closed chan struct{}
	once            sync.Once
	count           atomic.Int64
}

func (b *blockingBody) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	<-b.closed
	return 0, errors.New("reader stopped")
}
func (b *blockingBody) Close() error {
	if b.count.Add(1) == 1 {
		close(b.closed)
	}
	return nil
}
func TestHTTPBlockedBodyCancellationClosesOwnedReader(t *testing.T) {
	s, store := fixture(t, 1<<20)
	var sends atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sends.Add(1) }))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, e := s.Open(ctx, "POST", server.URL, nil, -1, nil)
	if e != nil {
		t.Fatal(e)
	}
	body := &blockingBody{started: make(chan struct{}), closed: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- r.CopyBody(body) }()
	<-body.started
	cancel()
	select {
	case e = <-done:
		wantCode(t, e, "EXECUTION_TIMEOUT")
	case <-time.After(3 * time.Second):
		t.Fatal("source reader stuck")
	}
	_ = r.Close()
	if body.count.Load() != 1 || sends.Load() != 0 {
		t.Fatal("reader/HTTP count", body.count.Load(), sends.Load())
	}
	empty(t, s, store)
}

func TestHTTPRawCompressedResponseAndHeaderQuota(t *testing.T) {
	for _, mode := range []string{"raw_gzip", "header_control"} {
		t.Run(mode, func(t *testing.T) {
			s, store := fixture(t, 1<<20)
			wire := []byte{31, 139, 8, 0, 0, 0, 0, 0, 0, 255, 3, 0, 0, 0, 0, 0, 0, 0, 0, 0}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Accept-Encoding") != "" {
					t.Error("automatic compression negotiation")
				}
				if mode == "header_control" {
					w.Header().Set("X-Large", strings.Repeat("\"", 40000))
				} else {
					w.Header().Set("Content-Encoding", "gzip")
				}
				_, _ = w.Write(wire)
			}))
			defer server.Close()
			r := request(t, s, server.URL, 0, nil)
			if e := r.Ready(); e != nil {
				t.Fatal(e)
			}
			resp, e := r.Do()
			if mode == "header_control" {
				wantCode(t, e, "LIMIT_EXCEEDED")
			} else {
				if e != nil {
					t.Fatal(e)
				}
				got, e := io.ReadAll(resp.Body)
				if e != nil || !bytes.Equal(got, wire) || resp.Headers.Get("Content-Encoding") != "gzip" {
					t.Fatal("transformed upstream bytes", e, got)
				}
				_ = resp.Body.Close()
			}
			_ = r.Close()
			empty(t, s, store)
		})
	}
}
