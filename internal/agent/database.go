package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	mssql "github.com/denisenkom/go-mssqldb"
	"github.com/denisenkom/go-mssqldb/msdsn"
	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/stdlib"
	goora "github.com/sijms/go-ora/v2"
)

// OpenDatabase creates the driver-specific connector used by normal startup,
// validate, and doctor. It is exported only within the repository's internal
// tree so integration tests exercise the same transport adapter as production.
func OpenDatabase(database DatabaseDef) (*sql.DB, error) {
	switch database.Driver {
	case "postgres":
		return openPostgresDatabase(database)
	case "mysql":
		return openMySQLDatabase(database)
	case "sqlserver":
		config, _, err := msdsn.Parse(database.DSN())
		if err != nil {
			return nil, err
		}
		if database.TLS.Snapshot && database.tlsMode() != "disable" {
			config.TLSConfig, err = databaseTLSConfig(database)
			if err != nil {
				return nil, err
			}
			config.TLSConfig.DynamicRecordSizingDisabled = true
			config.HostInCertificateProvided = true
		}
		return sql.OpenDB(mssql.NewConnectorConfig(config)), nil
	case "oracle":
		return openOracleDatabase(database)
	default:
		return nil, fmt.Errorf("unsupported database driver %q", database.Driver)
	}
}

func openDatabase(database DatabaseDef) (*sql.DB, error) { return OpenDatabase(database) }

func openPostgresDatabase(database DatabaseDef) (*sql.DB, error) {
	config, err := postgresConnectionConfig(database)
	if err != nil {
		return nil, err
	}
	afterConnect := func(ctx context.Context, conn *pgx.Conn) error {
		// lib/pq represented timestamp without time zone in an unnamed UTC
		// location, while timestamptz used PostgreSQL's session TimeZone. Set
		// both codecs explicitly so the pgx migration preserves those public
		// string coercions and does not inherit the agent process timezone.
		conn.TypeMap().RegisterType(&pgtype.Type{
			Name: "timestamp", OID: pgtype.TimestampOID,
			Codec: &pgtype.TimestampCodec{ScanLocation: time.FixedZone("", 0)},
		})
		conn.TypeMap().RegisterType(&pgtype.Type{
			Name: "timestamptz", OID: pgtype.TimestamptzOID,
			Codec: &pgtype.TimestamptzCodec{ScanLocation: postgresSessionLocation(ctx, conn)},
		})
		return nil
	}
	return stdlib.OpenDB(*config, stdlib.OptionAfterConnect(afterConnect)), nil
}

func postgresConnectionConfig(database DatabaseDef) (*pgx.ConnConfig, error) {
	if database.TLS.Snapshot {
		// pgx merges libpq environment settings before parsing the connection
		// string, including service-file reads. Only source-snapshot settings
		// may affect a production connection or its session behavior.
		for _, name := range postgresUnsupportedEnvironment {
			if os.Getenv(name) != "" {
				return nil, fmt.Errorf("PostgreSQL ambient setting %s is unsupported; configure sources.databases instead", name)
			}
		}
	}
	config, err := pgx.ParseConfig(database.DSN())
	if err != nil {
		return nil, err
	}
	if database.TLS.Snapshot {
		// An omitted or explicit empty password never falls back to pgpass.
		config.Password = database.Password
	}
	if database.TLS.Snapshot && database.tlsMode() != "disable" {
		tlsConfig, err := databaseTLSConfig(database)
		if err != nil {
			return nil, err
		}
		if database.tlsMode() == "verify-ca" {
			roots := tlsConfig.RootCAs
			tlsConfig.InsecureSkipVerify = true
			tlsConfig.VerifyConnection = func(state tls.ConnectionState) error {
				if len(state.PeerCertificates) == 0 {
					return errors.New("missing server certificate")
				}
				intermediates := x509.NewCertPool()
				for _, cert := range state.PeerCertificates[1:] {
					intermediates.AddCert(cert)
				}
				_, err := state.PeerCertificates[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates})
				return err
			}
		}
		config.Config.TLSConfig = tlsConfig
		for _, fallback := range config.Config.Fallbacks {
			fallback.TLSConfig = tlsConfig.Clone()
		}
	}
	if database.tlsMode() == "verify-full" && config.Config.TLSConfig != nil {
		config.Config.TLSConfig.ServerName = database.tlsServerName()
	}
	return config, nil
}

// These pgx/libpq settings have no captured source equivalent. Reject them
// before parsing instead of reading an external service file or admitting
// unhashed connection, authentication, protocol, or session settings.
var postgresUnsupportedEnvironment = [...]string{
	"PGAPPNAME", "PGCONNECT_TIMEOUT", "PGSSLSNI", "PGSSLPASSWORD",
	"PGSSLNEGOTIATION", "PGTARGETSESSIONATTRS", "PGSERVICE", "PGSERVICEFILE",
	"PGTZ", "PGOPTIONS", "PGMINPROTOCOLVERSION", "PGMAXPROTOCOLVERSION",
	"PGCHANNELBINDING", "PGREQUIREAUTH",
}

func openMySQLDatabase(database DatabaseDef) (*sql.DB, error) {
	config := mysql.NewConfig()
	config.User, config.Passwd, config.Net = database.User, database.Password, "tcp"
	config.Addr, config.DBName = net.JoinHostPort(database.Host, strconv.Itoa(database.Port)), database.Name
	if database.tlsMode() != "disable" {
		tlsConfig, err := databaseTLSConfig(database)
		if err != nil {
			return nil, err
		}
		// Passing tls.Config directly to NewConnector avoids the driver's
		// process-global named TLS registry and its collision/cleanup lifetime.
		config.TLS = tlsConfig
	}
	connector, err := mysql.NewConnector(config)
	if err != nil {
		return nil, err
	}
	return sql.OpenDB(connector), nil
}

func openOracleDatabase(database DatabaseDef) (*sql.DB, error) {
	if database.tlsMode() == "disable" {
		return sql.Open(driverName(database.Driver), database.DSN())
	}
	tlsConfig, err := databaseTLSConfig(database)
	if err != nil {
		return nil, err
	}
	return sql.OpenDB(&oracleTLSConnector{dsn: database.DSN(), tlsConfig: tlsConfig}), nil
}

// oracleTLSConnector makes a fresh go-ora connector and tls.Config for every
// physical connection. go-ora sets ServerName on the supplied config while it
// connects, so sharing one config across a database/sql pool would race.
type oracleTLSConnector struct {
	dsn       string
	tlsConfig *tls.Config
}

func (c *oracleTLSConnector) Connect(ctx context.Context) (driver.Conn, error) {
	connector, ok := goora.NewConnector(c.dsn).(*goora.OracleConnector)
	if !ok {
		return nil, errors.New("oracle connector has unexpected type")
	}
	connector.WithTLSConfig(c.tlsConfig.Clone())
	return connector.Connect(ctx)
}

func (c *oracleTLSConnector) Driver() driver.Driver { return goora.NewConnector(c.dsn).Driver() }

func databaseTLSConfig(database DatabaseDef) (*tls.Config, error) {
	config := &tls.Config{}
	if database.TLS.CertFile != "" || database.TLS.CertBytes != nil {
		var certificate tls.Certificate
		var err error
		if database.TLS.Snapshot {
			certificate, err = tls.X509KeyPair(database.TLS.CertBytes, database.TLS.KeyBytes)
		} else {
			certificate, err = tls.LoadX509KeyPair(database.TLS.CertFile, database.TLS.KeyFile)
		}
		if err != nil {
			return nil, fmt.Errorf("load database TLS client certificate: %w", err)
		}
		config.Certificates = []tls.Certificate{certificate}
	}
	if database.tlsMode() == "require" {
		// require intentionally authenticates neither the chain nor hostname.
		config.InsecureSkipVerify = true //nolint:gosec -- encryption-only mode is an explicit public contract
		return config, nil
	}
	if database.TLS.CAFile != "" || database.TLS.CABytes != nil {
		var roots *x509.CertPool
		var err error
		if database.TLS.Snapshot {
			roots = x509.NewCertPool()
			if !roots.AppendCertsFromPEM(database.TLS.CABytes) {
				err = errors.New("invalid database TLS CA")
			}
		} else {
			roots, err = loadDatabaseRootCAs(database.TLS.CAFile)
		}
		if err != nil {
			return nil, err
		}
		config.RootCAs = roots
	}
	expectedName := database.tlsServerName()
	config.ServerName = expectedName
	if database.Driver == "oracle" {
		// go-ora overwrites tls.Config.ServerName with the connection host. Use
		// VerifyConnection to retain the explicit override and full chain check.
		config.InsecureSkipVerify = true //nolint:gosec -- verification is performed below
		config.VerifyConnection = verifyDatabaseConnection(expectedName, config.RootCAs)
	}
	return config, nil
}

func loadDatabaseRootCAs(path string) (*x509.CertPool, error) {
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read database TLS CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pemBytes) {
		return nil, errors.New("database.tls.ca_file contains no certificates")
	}
	return roots, nil
}

func verifyDatabaseConnection(serverName string, roots *x509.CertPool) func(tls.ConnectionState) error {
	return func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) == 0 {
			return errors.New("database TLS peer did not provide a certificate")
		}
		intermediates := x509.NewCertPool()
		for _, certificate := range state.PeerCertificates[1:] {
			intermediates.AddCert(certificate)
		}
		_, err := state.PeerCertificates[0].Verify(x509.VerifyOptions{
			DNSName: serverName, Roots: roots, Intermediates: intermediates,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		})
		return err
	}
}
