package agent

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestDatabaseTLSConfigLoadsTrustClientPairAndServerName(t *testing.T) {
	caFile, certFile, keyFile, leaf := writeDatabaseTLSFixture(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), "db.internal")
	db := DatabaseDef{
		Driver: "mysql", Host: "127.0.0.1", Port: 3306, Name: "legacy", User: "user",
		TLS: DatabaseTLSDef{Mode: "verify-full", CAFile: caFile, CertFile: certFile, KeyFile: keyFile, ServerName: "db.internal"},
	}
	config, err := databaseTLSConfig(db)
	if err != nil {
		t.Fatal(err)
	}
	if config.InsecureSkipVerify || config.ServerName != "db.internal" || config.RootCAs == nil || len(config.Certificates) != 1 {
		t.Fatalf("TLS config = %#v", config)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: config.ServerName, Roots: config.RootCAs}); err != nil {
		t.Fatalf("loaded trust does not verify fixture: %v", err)
	}
}

func TestDatabaseTLSConfigRequireIsEncryptionOnly(t *testing.T) {
	config, err := databaseTLSConfig(DatabaseDef{Driver: "mysql", Host: "db", TLS: DatabaseTLSDef{Mode: "require"}})
	if err != nil {
		t.Fatal(err)
	}
	if !config.InsecureSkipVerify || config.RootCAs != nil || config.ServerName != "" || config.VerifyConnection != nil {
		t.Fatalf("require TLS config = %#v", config)
	}
}

func TestDatabaseTLSConfigVerifyFullUsesSystemRootsWhenCAIsOmitted(t *testing.T) {
	config, err := databaseTLSConfig(DatabaseDef{Driver: "mysql", Host: "db.internal", TLS: DatabaseTLSDef{Mode: "verify-full"}})
	if err != nil {
		t.Fatal(err)
	}
	if config.InsecureSkipVerify || config.RootCAs != nil || config.ServerName != "db.internal" {
		t.Fatalf("verify-full TLS config = %#v", config)
	}
}

func TestOracleVerifyConnectionRetainsOverrideAndRejectsInvalidPeers(t *testing.T) {
	caFile, _, _, validLeaf := writeDatabaseTLSFixture(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), "oracle.internal")
	_, _, _, expiredLeaf := writeDatabaseTLSFixture(t, time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour), "oracle.internal")
	db := DatabaseDef{Driver: "oracle", Host: "127.0.0.1", TLS: DatabaseTLSDef{Mode: "verify-full", CAFile: caFile, ServerName: "oracle.internal"}}
	config, err := databaseTLSConfig(db)
	if err != nil {
		t.Fatal(err)
	}
	if !config.InsecureSkipVerify || config.VerifyConnection == nil {
		t.Fatal("Oracle verify-full did not install explicit verification")
	}
	if err := config.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{validLeaf}}); err != nil {
		t.Fatalf("valid peer rejected: %v", err)
	}
	wrongName := db
	wrongName.TLS.ServerName = "wrong.internal"
	wrongConfig, err := databaseTLSConfig(wrongName)
	if err != nil {
		t.Fatal(err)
	}
	if err := wrongConfig.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{validLeaf}}); err == nil {
		t.Fatal("hostname mismatch accepted")
	}
	if err := config.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{expiredLeaf}}); err == nil {
		t.Fatal("expired certificate accepted")
	}
}

func TestMySQLTLSConnectorConstructionIsParallelAndRegistryFree(t *testing.T) {
	const workers = 64
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			db, err := OpenDatabase(DatabaseDef{Driver: "mysql", Host: "127.0.0.1", Port: 1, Name: "legacy", User: "user", TLS: DatabaseTLSDef{Mode: "require"}})
			if err == nil {
				err = db.Close()
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestMySQLTLSConfigurationsAreIndependent(t *testing.T) {
	firstCA, _, _, _ := writeDatabaseTLSFixture(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), "first.internal")
	secondCA, _, _, _ := writeDatabaseTLSFixture(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), "second.internal")
	first, err := databaseTLSConfig(DatabaseDef{Driver: "mysql", Host: "first.internal", TLS: DatabaseTLSDef{Mode: "verify-full", CAFile: firstCA}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := databaseTLSConfig(DatabaseDef{Driver: "mysql", Host: "second.internal", TLS: DatabaseTLSDef{Mode: "verify-full", CAFile: secondCA}})
	if err != nil {
		t.Fatal(err)
	}
	if first == second || first.RootCAs == second.RootCAs || first.ServerName == second.ServerName {
		t.Fatal("independent MySQL definitions shared TLS configuration state")
	}
	first.ServerName = "mutated.internal"
	if second.ServerName != "second.internal" {
		t.Fatal("mutating one MySQL TLS configuration affected another")
	}
}

func TestDatabaseTLSConfigRejectsUnreadableOrInvalidFiles(t *testing.T) {
	for _, path := range []string{filepath.Join(t.TempDir(), "missing.pem"), writeTestFile(t, "invalid.pem", []byte("not PEM"))} {
		db := DatabaseDef{Driver: "mysql", Host: "db", TLS: DatabaseTLSDef{Mode: "verify-full", CAFile: path}}
		if _, err := databaseTLSConfig(db); err == nil {
			t.Fatalf("accepted invalid CA %q", path)
		}
	}
	missing := filepath.Join(t.TempDir(), "missing-client.pem")
	if _, err := databaseTLSConfig(DatabaseDef{Driver: "mysql", Host: "db", TLS: DatabaseTLSDef{Mode: "verify-full", CertFile: missing, KeyFile: missing}}); err == nil {
		t.Fatal("accepted unreadable client certificate pair")
	}
}

func writeDatabaseTLSFixture(t *testing.T, notBefore, notAfter time.Time, dnsName string) (string, string, string, *x509.Certificate) {
	t.Helper()
	dir := t.TempDir()
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(notBefore.UnixNano()), Subject: pkix.Name{CommonName: "test root"},
		NotBefore: time.Now().Add(-24 * time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(notAfter.UnixNano()), Subject: pkix.Name{CommonName: dnsName},
		NotBefore: notBefore, NotAfter: notAfter, DNSNames: []string{dnsName},
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(leafDER)
	if err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(dir, "ca.pem")
	certPath := filepath.Join(dir, "certificate.pem")
	keyPath := filepath.Join(dir, "certificate.key")
	writePEM(t, caPath, "CERTIFICATE", caDER)
	writePEM(t, certPath, "CERTIFICATE", leafDER)
	writePEM(t, keyPath, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(leafKey))
	return caPath, certPath, keyPath, leaf
}

func writePEM(t *testing.T, path, kind string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: data}), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeTestFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
