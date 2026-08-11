//go:build integration

package it

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/denisenkom/go-mssqldb"
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	_ "github.com/sijms/go-ora/v2"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/mssql"
	"github.com/testcontainers/testcontainers-go/modules/mysql"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	itDBName       = "onprest_it"
	itDBUser       = "onprest"
	itDBPassword   = "onprest_secret"
	itReadOnlyUser = "onprest_readonly"
)

type integrationDBFixture struct {
	once     sync.Once
	cfg      postgresConfig
	readonly postgresConfig
	cleanup  func(context.Context) error
	err      error
}

var (
	postgresFixture  integrationDBFixture
	mysqlFixture     integrationDBFixture
	sqlServerFixture integrationDBFixture
	oracleFixture    integrationDBFixture
)

func allDBFixtures() []*integrationDBFixture {
	return []*integrationDBFixture{&postgresFixture, &mysqlFixture, &sqlServerFixture, &oracleFixture}
}

func postgresContainerConfig(t *testing.T) postgresConfig {
	t.Helper()
	if !selectedDBForTest(t, "postgres") {
		t.Skip("PostgreSQL integration tests are not selected")
	}
	return containerDBConfig(t, "postgres")
}

func postgresReadOnlyContainerConfig(t *testing.T) postgresConfig {
	t.Helper()
	if !selectedDBForTest(t, "postgres") {
		t.Skip("PostgreSQL read-only integration tests are not selected")
	}
	cfg := containerDBConfig(t, "postgres")
	if postgresFixture.readonly.Host == "" {
		return cfg
	}
	return postgresFixture.readonly
}

func selectedContainerDBConfig(t *testing.T, driver string) postgresConfig {
	t.Helper()
	if !selectedDBForTest(t, driver) {
		t.Skipf("%s integration tests are not selected", driver)
	}
	return containerDBConfig(t, driver)
}

func containerDBConfig(t *testing.T, driver string) postgresConfig {
	t.Helper()
	fixture := fixtureForDriver(driver)
	if fixture == nil {
		t.Fatalf("unsupported integration DB driver %q", driver)
	}
	fixture.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), startupTimeout(driver))
		defer cancel()
		fixture.cfg, fixture.readonly, fixture.cleanup, fixture.err = startContainerDB(ctx, driver)
	})
	if fixture.err != nil {
		if os.Getenv("ONPREST_IT_REQUIRE_CONTAINERS") == "1" {
			t.Fatalf("start %s testcontainer: %v", driver, fixture.err)
		}
		t.Skipf("skip %s testcontainer integration: %v", driver, fixture.err)
	}
	return fixture.cfg
}

func fixtureForDriver(driver string) *integrationDBFixture {
	switch driver {
	case "postgres":
		return &postgresFixture
	case "mysql":
		return &mysqlFixture
	case "sqlserver":
		return &sqlServerFixture
	case "oracle":
		return &oracleFixture
	default:
		return nil
	}
}

func startupTimeout(driver string) time.Duration {
	switch driver {
	case "oracle":
		return 12 * time.Minute
	case "sqlserver":
		return 8 * time.Minute
	default:
		return 2 * time.Minute
	}
}

func startContainerDB(ctx context.Context, driver string) (postgresConfig, postgresConfig, func(context.Context) error, error) {
	switch driver {
	case "postgres":
		return startPostgresContainer(ctx)
	case "mysql":
		cfg, cleanup, err := startMySQLContainer(ctx)
		return cfg, postgresConfig{}, cleanup, err
	case "sqlserver":
		cfg, cleanup, err := startSQLServerContainer(ctx)
		return cfg, postgresConfig{}, cleanup, err
	case "oracle":
		cfg, cleanup, err := startOracleContainer(ctx)
		return cfg, postgresConfig{}, cleanup, err
	default:
		return postgresConfig{}, postgresConfig{}, nil, fmt.Errorf("unsupported DB driver %q", driver)
	}
}

func startPostgresContainer(ctx context.Context) (postgresConfig, postgresConfig, func(context.Context) error, error) {
	hostPort, err := freeHostPort()
	if err != nil {
		return postgresConfig{}, postgresConfig{}, nil, err
	}
	ctr, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase(itDBName),
		postgres.WithUsername(itDBUser),
		postgres.WithPassword(itDBPassword),
		publishContainerPort(hostPort, "5432/tcp"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(2*time.Minute)),
	)
	if err != nil {
		return postgresConfig{}, postgresConfig{}, nil, err
	}
	cleanup := func(ctx context.Context) error { return ctr.Terminate(ctx) }
	cfg, err := configFromContainer(ctx, ctr.Container, hostPort, itDBName, itDBUser, itDBPassword)
	if err != nil {
		_ = cleanup(ctx)
		return postgresConfig{}, postgresConfig{}, nil, err
	}
	readonly := cfg
	readonly.User = itReadOnlyUser
	readonly.Password = itDBPassword
	if err := createPostgresReadOnlyUser(ctx, cfg, readonly); err != nil {
		_ = cleanup(ctx)
		return postgresConfig{}, postgresConfig{}, nil, err
	}
	return cfg, readonly, cleanup, nil
}

func startMySQLContainer(ctx context.Context) (postgresConfig, func(context.Context) error, error) {
	hostPort, err := freeHostPort()
	if err != nil {
		return postgresConfig{}, nil, err
	}
	ctr, err := mysql.Run(ctx, "mysql:8.0.36",
		mysql.WithDatabase(itDBName),
		mysql.WithUsername(itDBUser),
		mysql.WithPassword(itDBPassword),
		publishContainerPort(hostPort, "3306/tcp"),
	)
	if err != nil {
		return postgresConfig{}, nil, err
	}
	cleanup := func(ctx context.Context) error { return ctr.Terminate(ctx) }
	cfg, err := configFromContainer(ctx, ctr.Container, hostPort, itDBName, itDBUser, itDBPassword)
	if err != nil {
		_ = cleanup(ctx)
		return postgresConfig{}, nil, err
	}
	return cfg, cleanup, nil
}

func startSQLServerContainer(ctx context.Context) (postgresConfig, func(context.Context) error, error) {
	hostPort, err := freeHostPort()
	if err != nil {
		return postgresConfig{}, nil, err
	}
	ctr, err := mssql.Run(ctx, "mcr.microsoft.com/mssql/server:2022-CU14-ubuntu-22.04",
		mssql.WithAcceptEULA(),
		mssql.WithPassword("Strong@Passw0rd"),
		mssql.WithInitSQL(strings.NewReader("CREATE DATABASE "+itDBName+";\nGO\n")),
		publishContainerPort(hostPort, "1433/tcp"),
	)
	if err != nil {
		return postgresConfig{}, nil, err
	}
	cleanup := func(ctx context.Context) error { return ctr.Terminate(ctx) }
	cfg, err := configFromContainer(ctx, ctr.Container, hostPort, itDBName, "sa", "Strong@Passw0rd")
	if err != nil {
		_ = cleanup(ctx)
		return postgresConfig{}, nil, err
	}
	return cfg, cleanup, nil
}

func startOracleContainer(ctx context.Context) (postgresConfig, func(context.Context) error, error) {
	hostPort, err := freeHostPort()
	if err != nil {
		return postgresConfig{}, nil, err
	}
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image: "gvenzl/oracle-free:23-slim-faststart",
			Env: map[string]string{
				"ORACLE_PASSWORD":   itDBPassword,
				"APP_USER":          itDBUser,
				"APP_USER_PASSWORD": itDBPassword,
			},
			ExposedPorts:       []string{"1521/tcp"},
			HostConfigModifier: hostPortBindingModifier(hostPort, "1521/tcp", nil),
			WaitingFor: wait.ForAll(
				wait.ForListeningPort("1521/tcp"),
				wait.ForLog("DATABASE IS READY TO USE!"),
			).WithDeadline(8 * time.Minute),
		},
		Started: true,
	})
	if err != nil {
		return postgresConfig{}, nil, err
	}
	cleanup := func(ctx context.Context) error { return ctr.Terminate(ctx) }
	cfg, err := configFromContainer(ctx, ctr, hostPort, "FREEPDB1", itDBUser, itDBPassword)
	if err != nil {
		_ = cleanup(ctx)
		return postgresConfig{}, nil, err
	}
	return cfg, cleanup, nil
}

func configFromContainer(ctx context.Context, ctr testcontainers.Container, hostPort, name, user, password string) (postgresConfig, error) {
	host, err := ctr.Host(ctx)
	if err != nil {
		return postgresConfig{}, err
	}
	if _, err := strconv.Atoi(hostPort); err != nil {
		return postgresConfig{}, err
	}
	return postgresConfig{Host: normalizeContainerHost(host), Port: hostPort, Name: name, User: user, Password: password}, nil
}

func publishContainerPort(hostPort, containerPort string) testcontainers.CustomizeRequestOption {
	return func(req *testcontainers.GenericContainerRequest) error {
		if err := testcontainers.WithExposedPorts(containerPort)(req); err != nil {
			return err
		}
		req.HostConfigModifier = hostPortBindingModifier(hostPort, containerPort, req.HostConfigModifier)
		return nil
	}
}

func hostPortBindingModifier(hostPort, containerPort string, previous func(*container.HostConfig)) func(*container.HostConfig) {
	port := network.MustParsePort(containerPort)
	return func(hostConfig *container.HostConfig) {
		if previous != nil {
			previous(hostConfig)
		}
		if hostConfig.PortBindings == nil {
			hostConfig.PortBindings = network.PortMap{}
		}
		hostConfig.PortBindings[port] = []network.PortBinding{{
			HostIP:   netip.MustParseAddr("127.0.0.1"),
			HostPort: hostPort,
		}}
	}
}

func freeHostPort() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer ln.Close()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		return "", err
	}
	return port, nil
}

func normalizeContainerHost(host string) string {
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		return "127.0.0.1"
	}
	return host
}

func createPostgresReadOnlyUser(ctx context.Context, admin, readonly postgresConfig) error {
	db, err := sql.Open(sqlDriverName("postgres"), postgresDSN(admin))
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, "CREATE USER "+itReadOnlyUser+" WITH PASSWORD '"+itDBPassword+"'"); err != nil && !strings.Contains(err.Error(), "already exists") {
		return err
	}
	if _, err := db.ExecContext(ctx, "GRANT CONNECT ON DATABASE "+itDBName+" TO "+itReadOnlyUser); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, "GRANT USAGE ON SCHEMA public TO "+itReadOnlyUser); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, "GRANT SELECT ON ALL TABLES IN SCHEMA public TO "+itReadOnlyUser); err != nil {
		return err
	}
	return nil
}

func postgresDSN(db postgresConfig) string {
	return fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable", db.User, db.Password, net.JoinHostPort(db.Host, db.Port), db.Name)
}

func integrationDBDSN(driver string, db postgresConfig) string {
	hostPort := net.JoinHostPort(db.Host, db.Port)
	switch driver {
	case "postgres":
		return postgresDSN(db)
	case "mysql":
		return fmt.Sprintf("%s:%s@tcp(%s)/%s", db.User, db.Password, hostPort, db.Name)
	case "sqlserver":
		return fmt.Sprintf("sqlserver://%s:%s@%s?database=%s&encrypt=disable", db.User, db.Password, hostPort, db.Name)
	case "oracle":
		u := url.URL{
			Scheme: "oracle",
			User:   url.UserPassword(db.User, db.Password),
			Host:   hostPort,
			Path:   "/" + db.Name,
		}
		return u.String()
	default:
		return ""
	}
}

func seedCustomerTable(t *testing.T, driver string, cfg postgresConfig) {
	t.Helper()
	db, err := sql.Open(sqlDriverName(driver), integrationDBDSN(driver, cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, stmt := range customerSeedStatements(driver) {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s seed statement failed: %v\n%s", driver, err, stmt)
		}
	}
}

func sqlDriverName(driver string) string {
	if driver == "postgres" {
		return "pgx"
	}
	return driver
}

func customerSeedStatements(driver string) []string {
	switch driver {
	case "postgres":
		return []string{
			"drop table if exists onprest_it_customers",
			"create table onprest_it_customers (id integer primary key, name text not null, email text not null)",
			"insert into onprest_it_customers (id, name, email) values (7, 'Ada', 'ada@example.com')",
			"insert into onprest_it_customers (id, name, email) values (8, 'Grace', 'grace@example.com')",
		}
	case "mysql":
		return []string{
			"drop table if exists onprest_it_customers",
			"create table onprest_it_customers (id int primary key, name varchar(100) not null, email varchar(200) not null)",
			"insert into onprest_it_customers (id, name, email) values (7, 'Ada', 'ada@example.com')",
			"insert into onprest_it_customers (id, name, email) values (8, 'Grace', 'grace@example.com')",
		}
	case "sqlserver":
		return []string{
			"if object_id('dbo.onprest_it_customers', 'U') is not null drop table dbo.onprest_it_customers",
			"create table dbo.onprest_it_customers (id int primary key, name nvarchar(100) not null, email nvarchar(200) not null)",
			"insert into dbo.onprest_it_customers (id, name, email) values (7, 'Ada', 'ada@example.com')",
			"insert into dbo.onprest_it_customers (id, name, email) values (8, 'Grace', 'grace@example.com')",
		}
	case "oracle":
		return []string{
			"begin execute immediate 'drop table onprest_it_customers purge'; exception when others then if sqlcode != -942 then raise; end if; end;",
			"create table onprest_it_customers (id number(10) primary key, name varchar2(100) not null, email varchar2(200) not null)",
			"insert into onprest_it_customers (id, name, email) values (7, 'Ada', 'ada@example.com')",
			"insert into onprest_it_customers (id, name, email) values (8, 'Grace', 'grace@example.com')",
		}
	default:
		return nil
	}
}
