//go:build integration

package it

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/network"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	agentpkg "github.com/viewlegacy/onprest/internal/agent"
)

func TestSQLServerTLSRequireAndVerifyFullAgainstRealDatabase(t *testing.T) {
	if !selectedDBForTest(t, "sqlserver") {
		return
	}
	certDir := t.TempDir()
	caFile, serverCert, serverKey := createPostgresTLSFiles(t, certDir, "sqlserver-trusted-ca")
	renewedCert, renewedKey := createPostgresServerTLSFiles(t, certDir, filepath.Join(certDir, "renewed"), time.Now().Add(-time.Hour), time.Now().Add(48*time.Hour))
	expiredCert, expiredKey := createPostgresServerTLSFiles(t, certDir, filepath.Join(certDir, "expired"), time.Now().Add(-48*time.Hour), time.Now().Add(-24*time.Hour))
	wrongCA, _, _ := createPostgresTLSFiles(t, filepath.Join(certDir, "wrong"), "sqlserver-wrong-ca")
	configFile := writeFile(t, filepath.Join(certDir, "mssql.conf"), `[network]
tlscert = /var/opt/mssql/certs/server.pem
tlskey = /var/opt/mssql/certs/server.key
tlsprotocols = 1.2
forceencryption = 0
`)
	hostPort, err := freeHostPort()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image: "mcr.microsoft.com/mssql/server:2022-CU14-ubuntu-22.04",
			Env: map[string]string{
				"ACCEPT_EULA":       "Y",
				"MSSQL_SA_PASSWORD": "Strong@Passw0rd",
			},
			ExposedPorts:       []string{"1433/tcp"},
			HostConfigModifier: hostPortBindingModifier(hostPort, "1433/tcp", nil),
			Files: []testcontainers.ContainerFile{
				{HostFilePath: configFile, ContainerFilePath: "/var/opt/mssql/mssql.conf", FileMode: 0o444},
				{HostFilePath: serverCert, ContainerFilePath: "/var/opt/mssql/certs/server.pem", FileMode: 0o444},
				{HostFilePath: serverKey, ContainerFilePath: "/var/opt/mssql/certs/server.key", FileMode: 0o444},
			},
			WaitingFor: wait.ForAll(
				wait.ForListeningPort("1433/tcp"),
				wait.ForLog("SQL Server is now ready for client connections"),
				wait.ForSQL("1433/tcp", "sqlserver", func(host string, port network.Port) string {
					mappedPort := int(port.Num())
					return (agentpkg.DatabaseDef{
						Driver:   "sqlserver",
						Host:     host,
						Port:     mappedPort,
						Name:     "master",
						User:     "sa",
						Password: "Strong@Passw0rd",
						TLS:      agentpkg.DatabaseTLSDef{Mode: "require"},
					}).DSN()
				}).WithStartupTimeout(3*time.Minute),
			).WithDeadline(3 * time.Minute),
		},
		Started: true,
	})
	if err != nil {
		if os.Getenv("ONPREST_IT_REQUIRE_CONTAINERS") == "1" {
			t.Fatal("start TLS SQL Server container failed")
		}
		t.Skip("skip TLS SQL Server container: unavailable")
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
	base := agentpkg.DatabaseDef{
		Driver: "sqlserver", Host: "127.0.0.1", Port: port, Name: "master", User: "sa", Password: "Strong@Passw0rd",
	}
	disableDef := base
	disableDef.TLS = agentpkg.DatabaseTLSDef{Mode: "disable"}
	disableDB := openAndPingSQLServerTLS(t, disableDef)
	assertSQLServerConnectionEncryption(t, disableDB, false)
	disableDB.Close()

	requireDef := base
	requireDef.TLS = agentpkg.DatabaseTLSDef{Mode: "require"}
	requireDB := openAndPingSQLServerTLS(t, requireDef)
	assertSQLServerConnectionEncryption(t, requireDB, true)
	requireDB.Close()

	verifiedDef := base
	verifiedDef.TLS = agentpkg.DatabaseTLSDef{Mode: "verify-full", CAFile: caFile, ServerName: "localhost"}
	verifiedDB := openAndPingSQLServerTLS(t, verifiedDef)
	assertSQLServerConnectionEncryption(t, verifiedDB, true)

	wrongCADef := verifiedDef
	wrongCADef.TLS.CAFile = wrongCA
	assertSQLServerTLSPingFails(t, wrongCADef, "wrong CA")

	connectionHostDef := verifiedDef
	connectionHostDef.TLS.ServerName = ""
	assertSQLServerTLSPingFails(t, connectionHostDef, "connection host mismatch without override")

	wrongHostnameDef := verifiedDef
	wrongHostnameDef.TLS.ServerName = "wrong.example.invalid"
	assertSQLServerTLSPingFails(t, wrongHostnameDef, "wrong hostname")

	// Restarting the server drops every physical connection. The same pool must
	// reconnect with verify-full after the certificate is replaced; it may not
	// retry with require or plaintext.
	restartSQLServerWithCertificate(t, ctr, renewedCert, renewedKey)
	pingSQLServerUntilSuccess(t, verifiedDB)
	assertSQLServerConnectionEncryption(t, verifiedDB, true)
	verifiedDB.Close()

	restartSQLServerWithCertificate(t, ctr, expiredCert, expiredKey)
	expiredRequireDB, err := agentpkg.OpenDatabase(requireDef)
	if err != nil {
		t.Fatal("SQL Server require TLS adapter creation failed")
	}
	defer expiredRequireDB.Close()
	pingSQLServerUntilSuccess(t, expiredRequireDB)
	assertSQLServerConnectionEncryption(t, expiredRequireDB, true)
	assertSQLServerTLSPingFails(t, verifiedDef, "expired certificate")

	if strings.Contains(requireDef.DSN(), "certificate=") || !strings.Contains(requireDef.DSN(), "TrustServerCertificate=true") {
		t.Fatal("SQL Server require mode unexpectedly performs CA verification")
	}
	if !strings.Contains(verifiedDef.DSN(), "TrustServerCertificate=false") {
		t.Fatal("SQL Server verify-full mode does not require certificate verification")
	}
}

func restartSQLServerWithCertificate(t *testing.T, ctr testcontainers.Container, certFile, keyFile string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	stopTimeout := 30 * time.Second
	if err := ctr.Stop(ctx, &stopTimeout); err != nil {
		t.Fatal("stop SQL Server for TLS certificate replacement failed")
	}
	for source, target := range map[string]string{certFile: "/var/opt/mssql/certs/server.pem", keyFile: "/var/opt/mssql/certs/server.key"} {
		if err := ctr.CopyFileToContainer(ctx, source, target, 0o444); err != nil {
			t.Fatal("replace SQL Server TLS certificate failed")
		}
	}
	if err := ctr.Start(ctx); err != nil {
		t.Fatal("restart SQL Server after TLS certificate replacement failed")
	}
}

func pingSQLServerUntilSuccess(t *testing.T, db *sql.DB) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		err := db.PingContext(ctx)
		cancel()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("SQL Server verify-full pool did not recover after certificate replacement")
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func openAndPingSQLServerTLS(t *testing.T, def agentpkg.DatabaseDef) *sql.DB {
	t.Helper()
	db, err := agentpkg.OpenDatabase(def)
	if err != nil {
		t.Fatalf("SQL Server %s TLS adapter creation failed", def.TLS.Mode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Fatalf("SQL Server %s TLS ping failed", def.TLS.Mode)
	}
	return db
}

func assertSQLServerConnectionEncryption(t *testing.T, db *sql.DB, want bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var encrypted string
	if err := db.QueryRowContext(ctx, "select encrypt_option from sys.dm_exec_connections where session_id = @@SPID").Scan(&encrypted); err != nil {
		t.Fatal("SQL Server TLS state query failed")
	}
	if got := strings.EqualFold(encrypted, "TRUE"); got != want {
		t.Fatalf("SQL Server encrypted=%t encrypt_option=%q, want %t", got, encrypted, want)
	}
}

func assertSQLServerTLSPingFails(t *testing.T, def agentpkg.DatabaseDef, reason string) {
	t.Helper()
	db, err := agentpkg.OpenDatabase(def)
	if err != nil {
		t.Fatalf("SQL Server TLS negative-case adapter creation failed before PingContext: %s", reason)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err == nil {
		t.Fatalf("verify-full accepted %s", reason)
	}
}
