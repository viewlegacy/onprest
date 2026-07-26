//go:build integration

package it

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	agentpkg "github.com/viewlegacy/onprest/internal/agent"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

func TestPostgresTLSVerifyFullConnectsToRealDatabaseAndRejectsWrongCA(t *testing.T) {
	if !selectedDBForTest(t, "postgres") {
		return
	}
	certDir := t.TempDir()
	caFile, serverCert, serverKey := createPostgresTLSFiles(t, certDir, "trusted-ca")
	wrongCA, _, _ := createPostgresTLSFiles(t, filepath.Join(certDir, "wrong"), "wrong-ca")
	configFile := filepath.Join(certDir, "postgres-ssl.conf")
	writeFile(t, configFile, `listen_addresses = '*'
ssl = on
ssl_ca_file = '/tmp/testcontainers-go/postgres/ca_cert.pem'
ssl_cert_file = '/tmp/testcontainers-go/postgres/server.cert'
ssl_key_file = '/tmp/testcontainers-go/postgres/server.key'
`)
	hostPort, err := freeHostPort()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ctr, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase(itDBName),
		postgres.WithUsername(itDBUser),
		postgres.WithPassword(itDBPassword),
		postgres.WithConfigFile(configFile),
		postgres.WithSSLCert(caFile, serverCert, serverKey),
		publishContainerPort(hostPort, "5432/tcp"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(2*time.Minute)),
	)
	if err != nil {
		if os.Getenv("ONPREST_IT_REQUIRE_CONTAINERS") == "1" {
			t.Fatalf("start TLS PostgreSQL container: %v", err)
		}
		t.Skipf("skip TLS PostgreSQL container: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_ = ctr.Terminate(cleanupCtx)
	})
	port, err := net.LookupPort("tcp", hostPort)
	if err != nil {
		t.Fatal(err)
	}
	dbDef := agentpkg.DatabaseDef{
		Driver: "postgres", Host: "localhost", Port: port, Name: itDBName, User: itDBUser, Password: itDBPassword,
		TLS: agentpkg.DatabaseTLSDef{Mode: "verify-full", CAFile: caFile},
	}
	db, err := sql.Open("postgres", dbDef.DSN())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer pingCancel()
	if err := db.PingContext(pingCtx); err != nil {
		t.Fatalf("verified TLS ping: %v\nDSN=%s", err, dbDef.DSN())
	}
	var tlsActive bool
	if err := db.QueryRowContext(pingCtx, `select ssl from pg_stat_ssl where pid = pg_backend_pid()`).Scan(&tlsActive); err != nil {
		t.Fatal(err)
	}
	if !tlsActive {
		t.Fatal("PostgreSQL connection did not negotiate TLS")
	}

	badDef := dbDef
	badDef.TLS.CAFile = wrongCA
	badDB, err := sql.Open("postgres", badDef.DSN())
	if err != nil {
		t.Fatal(err)
	}
	defer badDB.Close()
	badCtx, badCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer badCancel()
	if err := badDB.PingContext(badCtx); err == nil {
		t.Fatal("verify-full accepted a server certificate signed by a different CA")
	}
}

func createPostgresTLSFiles(t *testing.T, dir, commonName string) (string, string, string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(now.UnixNano()),
		Subject: pkix.Name{CommonName: commonName},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	serverKeyValue, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(now.UnixNano() + 1),
		Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: []string{"localhost"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caTemplate, &serverKeyValue.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(dir, "ca.pem")
	certPath := filepath.Join(dir, "server.pem")
	keyPath := filepath.Join(dir, "server.key")
	writePEMFile(t, caPath, "CERTIFICATE", caDER, 0o644)
	writePEMFile(t, certPath, "CERTIFICATE", serverDER, 0o644)
	writePEMFile(t, keyPath, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(serverKeyValue), 0o600)
	return caPath, certPath, keyPath
}

func writePEMFile(t *testing.T, path, typ string, data []byte, mode os.FileMode) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		t.Fatal(err)
	}
	if err := pem.Encode(file, &pem.Block{Type: typ, Bytes: data}); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
