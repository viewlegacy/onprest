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
	renewedAuthority := newTestTLSAuthority(t, "postgres-renewed-tls-root")
	renewed := renewedAuthority.issueServer(t, "localhost", time.Now().Add(-time.Hour), time.Now().Add(48*time.Hour))
	expired := renewedAuthority.issueServer(t, "localhost", time.Now().Add(-48*time.Hour), time.Now().Add(-24*time.Hour))
	renewedCert, renewedKey := renewed.writeFiles(t, certDir, "renewed")
	expiredCert, expiredKey := expired.writeFiles(t, certDir, "expired")
	trustBundle := writeCertificateBundle(t, certDir, "trust-bundle.pem", caFile, renewedAuthority.caFile)
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
			t.Fatal("start TLS PostgreSQL container failed")
		}
		t.Skip("skip TLS PostgreSQL container: unavailable")
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
	verifyFullDef.TLS = agentpkg.DatabaseTLSDef{Mode: "verify-full", CAFile: trustBundle}
	assertPostgresTLSConnection(t, verifyFullDef, true)
	overrideDef := verifyFullDef
	overrideDef.Host = "127.0.0.1"
	overrideDef.TLS.ServerName = "localhost"
	assertPostgresTLSConnection(t, overrideDef, true)

	wrongCADef := verifyCADef
	wrongCADef.TLS.CAFile = wrongCA
	assertPostgresTLSPingFails(t, wrongCADef, "verify-ca with wrong private CA")
	wrongCADef = verifyFullDef
	wrongCADef.TLS.CAFile = wrongCA
	assertPostgresTLSPingFails(t, wrongCADef, "verify-full with wrong private CA")

	hostnameMismatchDef := verifyFullDef
	hostnameMismatchDef.Host = "127.0.0.1"
	assertPostgresTLSPingFails(t, hostnameMismatchDef, "verify-full hostname mismatch")
	hostnameMismatchDef.TLS.ServerName = "wrong.example.invalid"
	assertPostgresTLSPingFails(t, hostnameMismatchDef, "verify-full server_name mismatch")

	adminDB := openPostgresTLSDB(t, verifyFullDef)
	defer adminDB.Close()
	if _, err := adminDB.ExecContext(t.Context(), `create role clientcert_user login password 'clientcert-password'`); err != nil {
		t.Fatal("PostgreSQL client-certificate role setup failed")
	}
	clientDef := verifyFullDef
	clientDef.User = "clientcert_user"
	clientDef.Password = "clientcert-password"
	clientDef.TLS.CertFile = clientCert
	clientDef.TLS.KeyFile = clientKey
	assertPostgresTLSConnection(t, clientDef, true)
	clientDef.TLS.CertFile = ""
	clientDef.TLS.KeyFile = ""
	assertPostgresTLSPingFails(t, clientDef, "client certificate required by pg_hba")

	rotationDB := openPostgresTLSDB(t, verifyFullDef)
	defer rotationDB.Close()
	rotationDB.SetMaxOpenConns(1)
	rotationDB.SetMaxIdleConns(1)
	oldBackendPID := postgresBackendPID(t, rotationDB)
	ctxCopy, copyCancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer copyCancel()
	replacePostgresServerCertificate(t, ctxCopy, ctr, renewedCert, renewedKey)
	reloadPostgresTLS(t, ctxCopy, adminDB)
	terminatePostgresBackend(t, ctxCopy, adminDB, oldBackendPID)
	newBackendPID := waitForPostgresNewBackend(t, rotationDB, oldBackendPID)
	if newBackendPID == oldBackendPID {
		t.Fatal("PostgreSQL TLS rotation reused the terminated physical connection")
	}
	assertPostgresTLSActive(t, rotationDB, true)
	renewedOnlyDef := verifyFullDef
	renewedOnlyDef.TLS.CAFile = renewedAuthority.caFile
	assertPostgresTLSConnection(t, renewedOnlyDef, true)
	oldOnlyDef := verifyFullDef
	oldOnlyDef.TLS.CAFile = caFile
	assertPostgresTLSPingFails(t, oldOnlyDef, "verify-full old CA after renewed certificate")

	replacePostgresServerCertificate(t, ctxCopy, ctr, expiredCert, expiredKey)
	reloadPostgresTLS(t, ctxCopy, adminDB)
	terminatePostgresBackend(t, ctxCopy, adminDB, newBackendPID)
	assertOpenDatabaseTLSPingFails(t, rotationDB, "PostgreSQL expired certificate after forced reconnect")
	rotationDB.Close()
	assertPostgresTLSConnection(t, requireDef, true)
}

func postgresBackendPID(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var pid int64
	if err := db.QueryRowContext(ctx, "select pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal("PostgreSQL backend PID query failed")
	}
	return pid
}

func terminatePostgresBackend(t *testing.T, ctx context.Context, admin *sql.DB, pid int64) {
	t.Helper()
	var terminated bool
	if err := admin.QueryRowContext(ctx, "select pg_terminate_backend($1)", pid).Scan(&terminated); err != nil || !terminated {
		t.Fatal("PostgreSQL physical connection termination failed")
	}
}

func waitForPostgresNewBackend(t *testing.T, db *sql.DB, previousPID int64) int64 {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		var pid int64
		err := db.QueryRowContext(ctx, "select pg_backend_pid()").Scan(&pid)
		cancel()
		if err == nil && pid != previousPID {
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatal("PostgreSQL verify-full pool did not establish a new physical connection")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func replacePostgresServerCertificate(t *testing.T, ctx context.Context, ctr *postgres.PostgresContainer, certFile, keyFile string) {
	t.Helper()
	for source, target := range map[string]string{certFile: "/tmp/testcontainers-go/postgres/server.cert", keyFile: "/tmp/testcontainers-go/postgres/server.key"} {
		if err := ctr.CopyFileToContainer(ctx, source, target, 0o600); err != nil {
			t.Fatal("PostgreSQL TLS certificate replacement failed")
		}
	}
	if exit, _, err := ctr.Exec(ctx, []string{"sh", "-c", "chown postgres:postgres /tmp/testcontainers-go/postgres/server.cert /tmp/testcontainers-go/postgres/server.key"}); err != nil || exit != 0 {
		t.Fatal("PostgreSQL TLS certificate ownership update failed")
	}
}

func reloadPostgresTLS(t *testing.T, ctx context.Context, admin *sql.DB) {
	t.Helper()
	var reloaded bool
	if err := admin.QueryRowContext(ctx, "select pg_reload_conf()").Scan(&reloaded); err != nil || !reloaded {
		t.Fatal("PostgreSQL TLS certificate reload failed")
	}
}

func openPostgresTLSDB(t *testing.T, def agentpkg.DatabaseDef) *sql.DB {
	t.Helper()
	db, err := agentpkg.OpenDatabase(def)
	if err != nil {
		t.Fatalf("PostgreSQL %s TLS adapter creation failed", def.TLS.Mode)
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
		t.Fatalf("PostgreSQL %s TLS connection failed", def.TLS.Mode)
	}
	assertPostgresTLSActive(t, db, wantTLS)
}

func assertPostgresTLSActive(t *testing.T, db *sql.DB, wantTLS bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var tlsActive bool
	if err := db.QueryRowContext(ctx, `select ssl from pg_stat_ssl where pid = pg_backend_pid()`).Scan(&tlsActive); err != nil {
		t.Fatal("PostgreSQL TLS state query failed")
	}
	if tlsActive != wantTLS {
		t.Fatalf("PostgreSQL TLS active=%t, want %t", tlsActive, wantTLS)
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

func createPostgresServerTLSFiles(t *testing.T, caDir, outputDir string, notBefore, notAfter time.Time) (string, string) {
	t.Helper()
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		t.Fatal(err)
	}
	caCertPEM, err := os.ReadFile(filepath.Join(caDir, "ca.pem"))
	if err != nil {
		t.Fatal(err)
	}
	caBlock, _ := pem.Decode(caCertPEM)
	caCert, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	caKeyPEM, err := os.ReadFile(filepath.Join(caDir, "ca.key"))
	if err != nil {
		t.Fatal(err)
	}
	caKeyBlock, _ := pem.Decode(caKeyPEM)
	caKey, err := x509.ParsePKCS1PrivateKey(caKeyBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	serverKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: notBefore, NotAfter: notAfter, DNSNames: []string{"localhost"},
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, caCert, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath := filepath.Join(outputDir, "server.pem"), filepath.Join(outputDir, "server.key")
	writePEMFile(t, certPath, "CERTIFICATE", der, 0o600)
	writePEMFile(t, keyPath, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(serverKey), 0o600)
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

func writeCertificateBundle(t *testing.T, dir, name string, paths ...string) string {
	t.Helper()
	var bundle []byte
	for _, path := range paths {
		certificate, err := os.ReadFile(path)
		if err != nil {
			t.Fatal("TLS CA bundle source read failed")
		}
		bundle = append(bundle, certificate...)
		bundle = append(bundle, '\n')
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, bundle, 0o600); err != nil {
		t.Fatal("TLS CA bundle write failed")
	}
	return path
}
