//go:build integration

package it

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentpkg "github.com/viewlegacy/onprest/internal/agent"
)

func TestOracleTLSModesVerificationRotationAndReconnectAgainstRealDatabase(t *testing.T) {
	if !selectedDBForTest(t, "oracle") {
		return
	}
	database := selectedContainerDBConfig(t, "oracle")
	target := net.JoinHostPort(database.Host, database.Port)
	authority := newTestTLSAuthority(t, "oracle-tls-root")
	valid := authority.issueServer(t, "oracle.internal", time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour))
	renewed := authority.issueServer(t, "oracle.internal", time.Now().Add(-time.Hour), time.Now().Add(48*time.Hour))
	expired := authority.issueServer(t, "oracle.internal", time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour))
	wrongAuthority := newTestTLSAuthority(t, "oracle-wrong-root")

	proxy := newRotatingTLSProxy(t, target, valid.certificate)
	host, portText, err := net.SplitHostPort(proxy.Addr())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	base := agentpkg.DatabaseDef{Driver: "oracle", Host: host, Port: port, Name: database.Name, User: database.User, Password: database.Password}

	t.Run("disable is explicit plaintext", func(t *testing.T) {
		directPort, err := strconv.Atoi(database.Port)
		if err != nil {
			t.Fatal(err)
		}
		definition := base
		definition.Host, definition.Port = database.Host, directPort
		definition.TLS.Mode = "disable"
		db := openAndPingDatabaseTLS(t, definition)
		defer db.Close()
		assertOracleQuery(t, db)
	})

	t.Run("require encrypts without authenticating", func(t *testing.T) {
		proxy.Rotate(expired.certificate)
		definition := base
		definition.TLS.Mode = "require"
		db := openAndPingDatabaseTLS(t, definition)
		defer db.Close()
		assertOracleQuery(t, db)
		if proxy.Handshakes() == 0 {
			t.Fatal("Oracle require did not complete a TLS handshake")
		}
	})

	proxy.Rotate(valid.certificate)
	verified := base
	verified.TLS = agentpkg.DatabaseTLSDef{Mode: "verify-full", CAFile: authority.caFile, ServerName: "oracle.internal"}
	t.Run("verify full checks CA and server name", func(t *testing.T) {
		db := openAndPingDatabaseTLS(t, verified)
		defer db.Close()
		assertOracleQuery(t, db)
	})

	t.Run("wrong CA is rejected", func(t *testing.T) {
		definition := verified
		definition.TLS.CAFile = wrongAuthority.caFile
		assertDatabaseTLSPingFails(t, definition, "wrong CA")
	})

	t.Run("host mismatch without override is rejected", func(t *testing.T) {
		definition := verified
		definition.TLS.ServerName = ""
		assertDatabaseTLSPingFails(t, definition, "hostname mismatch")
	})

	t.Run("expired certificate is rejected", func(t *testing.T) {
		proxy.Rotate(expired.certificate)
		assertDatabaseTLSPingFails(t, verified, "expired certificate")
	})

	t.Run("certificate rotation and connection loss reconnect with verification", func(t *testing.T) {
		proxy.Rotate(valid.certificate)
		db := openAndPingDatabaseTLS(t, verified)
		defer db.Close()
		before := proxy.Handshakes()
		proxy.Rotate(renewed.certificate)
		proxy.DropConnections()
		deadline := time.Now().Add(20 * time.Second)
		for {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			err := db.PingContext(ctx)
			cancel()
			if err == nil && proxy.Handshakes() > before && proxy.LastSerial() == renewed.serial {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("Oracle verify-full pool did not recover with the renewed certificate")
			}
			time.Sleep(100 * time.Millisecond)
		}
		assertOracleQuery(t, db)
	})
}

func openAndPingDatabaseTLS(t *testing.T, definition agentpkg.DatabaseDef) *sql.DB {
	t.Helper()
	db, err := agentpkg.OpenDatabase(definition)
	if err != nil {
		t.Fatalf("%s %s TLS adapter creation failed", definition.Driver, definition.TLS.Mode)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Fatalf("%s %s TLS ping failed", definition.Driver, definition.TLS.Mode)
	}
	return db
}

func assertDatabaseTLSPingFails(t *testing.T, definition agentpkg.DatabaseDef, reason string) {
	t.Helper()
	db, err := agentpkg.OpenDatabase(definition)
	if err != nil {
		t.Fatalf("TLS negative-case adapter creation failed before PingContext: %s", reason)
	}
	defer db.Close()
	assertOpenDatabaseTLSPingFails(t, db, reason)
}

func assertOpenDatabaseTLSPingFails(t *testing.T, db *sql.DB, reason string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err == nil {
		t.Fatalf("verify-full accepted %s", reason)
	}
}

func assertOracleQuery(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var value int
	if err := db.QueryRowContext(ctx, "select 1 from dual").Scan(&value); err != nil || value != 1 {
		t.Fatal("Oracle TLS query failed")
	}
}

type testTLSAuthority struct {
	certificate *x509.Certificate
	privateKey  *rsa.PrivateKey
	caFile      string
}

type testServerCertificate struct {
	certificate tls.Certificate
	serial      string
}

func newTestTLSAuthority(t *testing.T, name string) testTLSAuthority {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(now.UnixNano()), Subject: pkix.Name{CommonName: name},
		NotBefore: now.Add(-24 * time.Hour), NotAfter: now.Add(7 * 24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return testTLSAuthority{certificate: certificate, privateKey: key, caFile: caFile}
}

func (a testTLSAuthority) issueServer(t *testing.T, dnsName string, notBefore, notAfter time.Time) testServerCertificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	serial := big.NewInt(time.Now().UnixNano())
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: dnsName}, DNSNames: []string{dnsName},
		NotBefore: notBefore, NotAfter: notAfter,
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, a.certificate, &key.PublicKey, a.privateKey)
	if err != nil {
		t.Fatal(err)
	}
	certificate := tls.Certificate{
		Certificate: [][]byte{der, a.certificate.Raw}, PrivateKey: key,
	}
	return testServerCertificate{certificate: certificate, serial: serial.String()}
}

func (a testTLSAuthority) issueClient(t *testing.T, commonName string, notBefore, notAfter time.Time) testServerCertificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	serial := big.NewInt(time.Now().UnixNano())
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: commonName},
		NotBefore: notBefore, NotAfter: notAfter,
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, a.certificate, &key.PublicKey, a.privateKey)
	if err != nil {
		t.Fatal(err)
	}
	return testServerCertificate{
		certificate: tls.Certificate{Certificate: [][]byte{der, a.certificate.Raw}, PrivateKey: key},
		serial:      serial.String(),
	}
}

func (c testServerCertificate) writeFiles(t *testing.T, dir, prefix string) (string, string) {
	t.Helper()
	certPath := filepath.Join(dir, prefix+".pem")
	keyPath := filepath.Join(dir, prefix+".key")
	var certificates []byte
	for _, der := range c.certificate.Certificate {
		certificates = append(certificates, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	if err := os.WriteFile(certPath, certificates, 0o600); err != nil {
		t.Fatal(err)
	}
	key, ok := c.certificate.PrivateKey.(*rsa.PrivateKey)
	if !ok {
		t.Fatal("fixture private key is not RSA")
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

type rotatingTLSProxy struct {
	listener    net.Listener
	target      string
	current     atomic.Pointer[tls.Certificate]
	handshakes  atomic.Int64
	lastSerial  atomic.Value
	mu          sync.Mutex
	connections map[net.Conn]struct{}
}

func newRotatingTLSProxy(t *testing.T, target string, certificate tls.Certificate) *rotatingTLSProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxy := &rotatingTLSProxy{listener: listener, target: target, connections: map[net.Conn]struct{}{}}
	proxy.current.Store(&certificate)
	go proxy.serve()
	t.Cleanup(func() {
		_ = proxy.listener.Close()
		proxy.DropConnections()
	})
	return proxy
}

func (p *rotatingTLSProxy) Addr() string      { return p.listener.Addr().String() }
func (p *rotatingTLSProxy) Handshakes() int64 { return p.handshakes.Load() }
func (p *rotatingTLSProxy) LastSerial() string {
	value := p.lastSerial.Load()
	if value == nil {
		return ""
	}
	return value.(string)
}
func (p *rotatingTLSProxy) Rotate(certificate tls.Certificate) { p.current.Store(&certificate) }

func (p *rotatingTLSProxy) DropConnections() {
	p.mu.Lock()
	connections := make([]net.Conn, 0, len(p.connections))
	for connection := range p.connections {
		connections = append(connections, connection)
	}
	p.mu.Unlock()
	for _, connection := range connections {
		_ = connection.Close()
	}
}

func (p *rotatingTLSProxy) serve() {
	for {
		raw, err := p.listener.Accept()
		if err != nil {
			return
		}
		go p.proxy(raw)
	}
}

func (p *rotatingTLSProxy) proxy(raw net.Conn) {
	p.track(raw, true)
	defer func() { p.track(raw, false); _ = raw.Close() }()
	config := &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		selected := p.current.Load()
		if len(selected.Certificate) > 0 {
			if certificate, err := x509.ParseCertificate(selected.Certificate[0]); err == nil {
				p.lastSerial.Store(certificate.SerialNumber.String())
			}
		}
		return selected, nil
	}}
	secure := tls.Server(raw, config)
	if err := secure.Handshake(); err != nil {
		return
	}
	p.handshakes.Add(1)
	upstream, err := net.DialTimeout("tcp", p.target, 5*time.Second)
	if err != nil {
		return
	}
	p.track(upstream, true)
	defer func() { p.track(upstream, false); _ = upstream.Close() }()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(upstream, secure) }()
	go func() { defer wg.Done(); _, _ = io.Copy(secure, upstream) }()
	wg.Wait()
}

func (p *rotatingTLSProxy) track(connection net.Conn, add bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if add {
		p.connections[connection] = struct{}{}
	} else {
		delete(p.connections, connection)
	}
}
