//go:build integration

package it

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/mysql"
	agentpkg "github.com/viewlegacy/onprest/internal/agent"
)

func TestMySQLTLSModesClientCertificateRotationAndReconnectAgainstRealDatabase(t *testing.T) {
	if !selectedDBForTest(t, "mysql") {
		return
	}
	certDir := t.TempDir()
	authority := newTestTLSAuthority(t, "mysql-tls-root")
	renewedAuthority := newTestTLSAuthority(t, "mysql-renewed-tls-root")
	valid := authority.issueServer(t, "mysql.internal", time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour))
	renewed := renewedAuthority.issueServer(t, "mysql.internal", time.Now().Add(-time.Hour), time.Now().Add(48*time.Hour))
	expired := renewedAuthority.issueServer(t, "mysql.internal", time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour))
	client := authority.issueClient(t, "mtls-user", time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour))
	serverCert, serverKey := valid.writeFiles(t, certDir, "server")
	clientCert, clientKey := client.writeFiles(t, certDir, "client")
	trustBundle := writeCertificateBundle(t, certDir, "trust-bundle.pem", authority.caFile, renewedAuthority.caFile)
	wrongAuthority := newTestTLSAuthority(t, "mysql-wrong-root")
	configFile := filepath.Join(certDir, "my.cnf")
	writeFile(t, configFile, `[mysqld]
ssl-ca=/etc/mysql/certs/ca.pem
ssl-cert=/etc/mysql/certs/server.pem
ssl-key=/etc/mysql/certs/server.key
require_secure_transport=OFF
`)
	hostPort, err := freeHostPort()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	ctr, err := mysql.Run(ctx, "mysql:8.0.36",
		mysql.WithDatabase(itDBName), mysql.WithUsername(itDBUser), mysql.WithPassword(itDBPassword),
		mysql.WithConfigFile(configFile), publishContainerPort(hostPort, "3306/tcp"),
		testcontainers.WithFiles(
			testcontainers.ContainerFile{HostFilePath: trustBundle, ContainerFilePath: "/etc/mysql/certs/ca.pem", FileMode: 0o444},
			testcontainers.ContainerFile{HostFilePath: serverCert, ContainerFilePath: "/etc/mysql/certs/server.pem", FileMode: 0o444},
			testcontainers.ContainerFile{HostFilePath: serverKey, ContainerFilePath: "/etc/mysql/certs/server.key", FileMode: 0o444},
		),
	)
	if err != nil {
		if os.Getenv("ONPREST_IT_REQUIRE_CONTAINERS") == "1" {
			t.Fatal("start TLS MySQL container failed")
		}
		t.Skip("skip TLS MySQL container: unavailable")
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_ = ctr.Terminate(cleanupCtx)
	})
	port, err := strconv.Atoi(hostPort)
	if err != nil {
		t.Fatal(err)
	}
	base := agentpkg.DatabaseDef{Driver: "mysql", Host: "localhost", Port: port, Name: itDBName, User: itDBUser, Password: itDBPassword}

	disable := base
	disable.TLS.Mode = "disable"
	disableDB := openAndPingDatabaseTLS(t, disable)
	assertMySQLConnectionEncrypted(t, disableDB, false)
	disableDB.Close()

	require := base
	require.TLS.Mode = "require"
	requireDB := openAndPingDatabaseTLS(t, require)
	assertMySQLConnectionEncrypted(t, requireDB, true)
	requireDB.Close()

	verified := base
	verified.TLS = agentpkg.DatabaseTLSDef{Mode: "verify-full", CAFile: trustBundle, ServerName: "mysql.internal"}
	verifiedDB := openAndPingDatabaseTLS(t, verified)
	verifiedDB.SetMaxOpenConns(1)
	verifiedDB.SetMaxIdleConns(1)
	assertMySQLConnectionEncrypted(t, verifiedDB, true)
	rootVerified := verified
	rootVerified.User = "root"
	rootDB := openAndPingDatabaseTLS(t, rootVerified)
	defer rootDB.Close()

	wrongCA := verified
	wrongCA.TLS.CAFile = wrongAuthority.caFile
	assertDatabaseTLSPingFails(t, wrongCA, "wrong CA")
	wrongName := verified
	wrongName.TLS.ServerName = "wrong.internal"
	assertDatabaseTLSPingFails(t, wrongName, "wrong hostname")

	if _, err := rootDB.ExecContext(t.Context(), "CREATE USER IF NOT EXISTS 'mtls_user'@'%' IDENTIFIED BY 'mtls-password' REQUIRE X509"); err != nil {
		t.Fatal("MySQL client-certificate user setup failed")
	}
	if _, err := rootDB.ExecContext(t.Context(), "GRANT SELECT ON `"+itDBName+"`.* TO 'mtls_user'@'%'"); err != nil {
		t.Fatal("MySQL client-certificate grant setup failed")
	}
	clientDefinition := verified
	clientDefinition.User, clientDefinition.Password = "mtls_user", "mtls-password"
	clientDefinition.TLS.CertFile, clientDefinition.TLS.KeyFile = clientCert, clientKey
	clientDB := openAndPingDatabaseTLS(t, clientDefinition)
	clientDB.Close()
	withoutClient := clientDefinition
	withoutClient.TLS.CertFile, withoutClient.TLS.KeyFile = "", ""
	assertDatabaseTLSPingFails(t, withoutClient, "required client certificate missing")

	renewedCert, renewedKey := renewed.writeFiles(t, certDir, "renewed")
	oldConnectionID := mysqlConnectionID(t, verifiedDB)
	reloadMySQLTLS(t, ctr, rootDB, renewedCert, renewedKey)
	killMySQLConnection(t, rootDB, oldConnectionID)
	newConnectionID := waitForMySQLNewConnection(t, verifiedDB, oldConnectionID)
	if newConnectionID == oldConnectionID {
		t.Fatal("MySQL TLS rotation reused the terminated physical connection")
	}
	assertMySQLConnectionEncrypted(t, verifiedDB, true)
	renewedOnly := verified
	renewedOnly.TLS.CAFile = renewedAuthority.caFile
	renewedOnlyDB := openAndPingDatabaseTLS(t, renewedOnly)
	assertMySQLConnectionEncrypted(t, renewedOnlyDB, true)
	renewedOnlyDB.Close()
	oldOnly := verified
	oldOnly.TLS.CAFile = authority.caFile
	assertDatabaseTLSPingFails(t, oldOnly, "old CA after renewed certificate")

	expiredCert, expiredKey := expired.writeFiles(t, certDir, "expired")
	reloadMySQLTLS(t, ctr, rootDB, expiredCert, expiredKey)
	killMySQLConnection(t, rootDB, newConnectionID)
	assertOpenDatabaseTLSPingFails(t, verifiedDB, "expired certificate after forced reconnect")
	verifiedDB.Close()
	expiredRequireDB := openAndPingDatabaseTLS(t, require)
	assertMySQLConnectionEncrypted(t, expiredRequireDB, true)
	expiredRequireDB.Close()
}

func mysqlConnectionID(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var id int64
	if err := db.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&id); err != nil {
		t.Fatal("MySQL connection ID query failed")
	}
	return id
}

func killMySQLConnection(t *testing.T, admin *sql.DB, id int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := admin.ExecContext(ctx, "KILL CONNECTION "+strconv.FormatInt(id, 10)); err != nil {
		t.Fatal("MySQL physical connection termination failed")
	}
}

func waitForMySQLNewConnection(t *testing.T, db *sql.DB, previousID int64) int64 {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		var id int64
		err := db.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&id)
		cancel()
		if err == nil && id != previousID {
			return id
		}
		if time.Now().After(deadline) {
			t.Fatal("MySQL verify-full pool did not establish a new physical connection")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func assertMySQLConnectionEncrypted(t *testing.T, db *sql.DB, want bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var name, cipher string
	if err := db.QueryRowContext(ctx, "SHOW STATUS LIKE 'Ssl_cipher'").Scan(&name, &cipher); err != nil {
		t.Fatal("MySQL TLS cipher query failed")
	}
	if got := cipher != ""; got != want {
		t.Fatalf("MySQL TLS encrypted=%t cipher=%q, want %t", got, cipher, want)
	}
}

func reloadMySQLTLS(t *testing.T, ctr *mysql.MySQLContainer, admin *sql.DB, certFile, keyFile string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	for source, target := range map[string]string{certFile: "/etc/mysql/certs/server.pem", keyFile: "/etc/mysql/certs/server.key"} {
		if err := ctr.CopyFileToContainer(ctx, source, target, 0o444); err != nil {
			t.Fatal("MySQL TLS certificate replacement failed")
		}
	}
	if _, err := admin.ExecContext(ctx, "ALTER INSTANCE RELOAD TLS"); err != nil {
		t.Fatal("MySQL TLS certificate reload failed")
	}
}
