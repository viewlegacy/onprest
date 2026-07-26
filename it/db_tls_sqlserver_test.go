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
	wrongCA, _, _ := createPostgresTLSFiles(t, filepath.Join(certDir, "wrong"), "sqlserver-wrong-ca")
	configFile := writeFile(t, filepath.Join(certDir, "mssql.conf"), `[network]
tlscert = /var/opt/mssql/certs/server.pem
tlskey = /var/opt/mssql/certs/server.key
tlsprotocols = 1.2
forceencryption = 1
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
			t.Fatalf("start TLS SQL Server container: %v", err)
		}
		t.Skipf("skip TLS SQL Server container: %v", err)
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
		Driver: "sqlserver", Host: "localhost", Port: port, Name: "master", User: "sa", Password: "Strong@Passw0rd",
	}

	requireDef := base
	requireDef.TLS = agentpkg.DatabaseTLSDef{Mode: "require"}
	requireDB := openAndPingSQLServerTLS(t, requireDef)
	assertSQLServerConnectionEncrypted(t, requireDB)
	requireDB.Close()

	verifiedDef := base
	verifiedDef.TLS = agentpkg.DatabaseTLSDef{Mode: "verify-full", CAFile: caFile, ServerName: "localhost"}
	verifiedDB := openAndPingSQLServerTLS(t, verifiedDef)
	assertSQLServerConnectionEncrypted(t, verifiedDB)
	verifiedDB.Close()

	wrongCADef := verifiedDef
	wrongCADef.TLS.CAFile = wrongCA
	assertSQLServerTLSPingFails(t, wrongCADef, "wrong CA")

	wrongHostnameDef := verifiedDef
	wrongHostnameDef.TLS.ServerName = "wrong.example.invalid"
	assertSQLServerTLSPingFails(t, wrongHostnameDef, "wrong hostname")

	if strings.Contains(requireDef.DSN(), "certificate=") || !strings.Contains(requireDef.DSN(), "TrustServerCertificate=true") {
		t.Fatalf("require mode unexpectedly performs CA verification: %s", requireDef.DSN())
	}
	if !strings.Contains(verifiedDef.DSN(), "TrustServerCertificate=false") {
		t.Fatalf("verify-full mode does not require certificate verification: %s", verifiedDef.DSN())
	}
}

func openAndPingSQLServerTLS(t *testing.T, def agentpkg.DatabaseDef) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlserver", def.DSN())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		t.Fatalf("SQL Server TLS ping failed: %v\nDSN=%s", err, def.DSN())
	}
	return db
}

func assertSQLServerConnectionEncrypted(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var encrypted string
	if err := db.QueryRowContext(ctx, "select encrypt_option from sys.dm_exec_connections where session_id = @@SPID").Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(encrypted, "TRUE") {
		t.Fatalf("SQL Server encrypt_option = %q, want TRUE", encrypted)
	}
}

func assertSQLServerTLSPingFails(t *testing.T, def agentpkg.DatabaseDef, reason string) {
	t.Helper()
	db, err := sql.Open("sqlserver", def.DSN())
	if err != nil {
		return
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err == nil {
		t.Fatalf("verify-full accepted %s", reason)
	}
}
