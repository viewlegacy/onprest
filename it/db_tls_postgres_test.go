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

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	agentpkg "github.com/viewlegacy/onprest/internal/agent"
)

func TestPostgresTLSModesPrivateCAClientCertificateAndHostnameVerification(t *testing.T) {
	if !selectedDBForTest(t, "postgres") {
		return
	}
	certDir := t.TempDir()
	caFile, serverCert, serverKey := createPostgresTLSFiles(t, certDir, "trusted-ca")
	clientCert, clientKey := createPostgresClientTLSFiles(t, certDir, "clientcert_user")
	wrongCA, _, _ := createPostgresTLSFiles(t, filepath.Join(certDir, "wrong"), "wrong-ca")
	hbaFile := filepath.Join(certDir, "pg_hba.conf")
	writeFile(t, hbaFile, `local all all trust
hostssl all clientcert_user all scram-sha-256 clientcert=verify-full
hostssl all all all scram-sha-256
hostnossl all all all scram-sha-256
`)
	configFile := filepath.Join(certDir, "postgres-ssl.conf")
	writeFile(t, configFile, `listen_addresses = '*'
ssl = on
ssl_ca_file = '/tmp/testcontainers-go/postgres/ca_cert.pem'
ssl_cert_file = '/tmp/testcontainers-go/postgres/server.cert'
ssl_key_file = '/tmp/testcontainers-go/postgres/server.key'
hba_file = '/tmp/testcontainers-go/postgres/pg_hba.conf'
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
		testcontainers.WithFiles(testcontainers.ContainerFile{HostFilePath: hbaFile, ContainerFilePath: "/tmp/testcontainers-go/postgres/pg_hba.conf", FileMode: 0o644}),
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
	baseDef := agentpkg.DatabaseDef{
		Driver: "postgres", Host: "localhost", Port: port, Name: itDBName, User: itDBUser, Password: itDBPassword,
	}

	disableDef := baseDef
	disableDef.TLS.Mode = "disable"
	assertPostgresTLSConnection(t, disableDef, false)

	requireDef := baseDef
	requireDef.Host = "127.0.0.1"
	requireDef.TLS.Mode = "require"
	assertPostgresTLSConnection(t, requireDef, true)

	verifyCADef := baseDef
	verifyCADef.Host = "127.0.0.1"
	verifyCADef.TLS = agentpkg.DatabaseTLSDef{Mode: "verify-ca", CAFile: caFile}
	assertPostgresTLSConnection(t, verifyCADef, true)

	verifyFullDef := baseDef
	verifyFullDef.TLS = agentpkg.DatabaseTLSDef{Mode: "verify-full", CAFile: caFile}
	assertPostgresTLSConnection(t, verifyFullDef, true)

	wrongCADef := verifyCADef
	wrongCADef.TLS.CAFile = wrongCA
	assertPostgresTLSPingFails(t, wrongCADef, "verify-ca with wrong private CA")
	wrongCADef = verifyFullDef
	wrongCADef.TLS.CAFile = wrongCA
	assertPostgresTLSPingFails(t, wrongCADef, "verify-full with wrong private CA")

	hostnameMismatchDef := verifyFullDef
	hostnameMismatchDef.Host = "127.0.0.1"
	assertPostgresTLSPingFails(t, hostnameMismatchDef, "verify-full hostname mismatch")

	adminDB := openPostgresTLSDB(t, verifyFullDef)
	if _, err := adminDB.ExecContext(t.Context(), `create role clientcert_user login password 'clientcert-password'`); err != nil {
		t.Fatal(err)
	}
	adminDB.Close()
	clientDef := verifyFullDef
	clientDef.User = "clientcert_user"
	clientDef.Password = "clientcert-password"
	clientDef.TLS.CertFile = clientCert
	clientDef.TLS.KeyFile = clientKey
	assertPostgresTLSConnection(t, clientDef, true)
	clientDef.TLS.CertFile = ""
	clientDef.TLS.KeyFile = ""
	assertPostgresTLSPingFails(t, clientDef, "client certificate required by pg_hba")
}

func openPostgresTLSDB(t *testing.T, def agentpkg.DatabaseDef) *sql.DB {
	t.Helper()
	db, err := sql.Open(sqlDriverName("postgres"), def.DSN())
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func assertPostgresTLSConnection(t *testing.T, def agentpkg.DatabaseDef, wantTLS bool) {
	t.Helper()
	db := openPostgresTLSDB(t, def)
	defer db.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("PostgreSQL %s connection failed: %v", def.TLS.Mode, err)
	}
	var tlsActive bool
	if err := db.QueryRowContext(ctx, `select ssl from pg_stat_ssl where pid = pg_backend_pid()`).Scan(&tlsActive); err != nil {
		t.Fatal(err)
	}
	if tlsActive != wantTLS {
		t.Fatalf("PostgreSQL %s TLS active=%t, want %t", def.TLS.Mode, tlsActive, wantTLS)
	}
}

func assertPostgresTLSPingFails(t *testing.T, def agentpkg.DatabaseDef, reason string) {
	t.Helper()
	db := openPostgresTLSDB(t, def)
	defer db.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err == nil {
		t.Fatalf("PostgreSQL connection unexpectedly succeeded: %s", reason)
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
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
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
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:    []string{"localhost"},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caTemplate, &serverKeyValue.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(dir, "ca.pem")
	caKeyPath := filepath.Join(dir, "ca.key")
	certPath := filepath.Join(dir, "server.pem")
	keyPath := filepath.Join(dir, "server.key")
	writePEMFile(t, caPath, "CERTIFICATE", caDER, 0o644)
	writePEMFile(t, caKeyPath, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(caKey), 0o600)
	writePEMFile(t, certPath, "CERTIFICATE", serverDER, 0o644)
	writePEMFile(t, keyPath, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(serverKeyValue), 0o600)
	return caPath, certPath, keyPath
}

func createPostgresClientTLSFiles(t *testing.T, dir, commonName string) (string, string) {
	t.Helper()
	caCertPEM, err := os.ReadFile(filepath.Join(dir, "ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	caBlock, _ := pem.Decode(caCertPEM)
	if caBlock == nil {
		t.Fatal("decode CA certificate")
	}
	caCert, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	caKeyPEM, err := os.ReadFile(filepath.Join(dir, "ca.key"))
	if err != nil {
		t.Fatal(err)
	}
	caKeyBlock, _ := pem.Decode(caKeyPEM)
	if caKeyBlock == nil {
		t.Fatal("decode CA key")
	}
	caKey, err := x509.ParsePKCS1PrivateKey(caKeyBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	clientKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(now.UnixNano() + 2),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, caCert, &clientKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(dir, "client.pem")
	keyPath := filepath.Join(dir, "client.key")
	writePEMFile(t, certPath, "CERTIFICATE", der, 0o600)
	writePEMFile(t, keyPath, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(clientKey), 0o600)
	return certPath, keyPath
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
