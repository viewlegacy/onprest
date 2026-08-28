package agent

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const preflightTestDriverName = "onprest_preflight_test"

var preflightDriver = &preflightDriverState{}

func init() { sql.Register(preflightTestDriverName, preflightDriverImpl{}) }

type preflightDriverState struct {
	mu       sync.Mutex
	events   []string
	pingErr  error
	queryErr error
	block    bool
}

func (s *preflightDriverState) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events, s.pingErr, s.queryErr, s.block = nil, nil, nil, false
}
func (s *preflightDriverState) add(event string) {
	s.mu.Lock()
	s.events = append(s.events, event)
	s.mu.Unlock()
}
func (s *preflightDriverState) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

type preflightDriverImpl struct{}

func (preflightDriverImpl) Open(string) (driver.Conn, error) {
	preflightDriver.add("open")
	return preflightConn{}, nil
}

type preflightConn struct{}

func (preflightConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unsupported") }
func (preflightConn) Begin() (driver.Tx, error)           { return nil, errors.New("unsupported") }
func (preflightConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	preflightDriver.add("begin")
	return preflightTx{}, nil
}
func (preflightConn) Close() error {
	preflightDriver.add("close")
	return nil
}
func (preflightConn) Ping(context.Context) error {
	preflightDriver.add("ping")
	return preflightDriver.pingErr
}
func (preflightConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	preflightDriver.add("explain-exec")
	if preflightDriver.queryErr != nil {
		return nil, preflightDriver.queryErr
	}
	return driver.RowsAffected(0), nil
}
func (preflightConn) QueryContext(ctx context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
	preflightDriver.add("explain")
	if preflightDriver.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if preflightDriver.queryErr != nil {
		return nil, preflightDriver.queryErr
	}
	return preflightRows{}, nil
}

type preflightTx struct{}

func (preflightTx) Commit() error { return nil }
func (preflightTx) Rollback() error {
	preflightDriver.add("rollback")
	return nil
}

type preflightRows struct{}

func (preflightRows) Columns() []string         { return []string{"plan"} }
func (preflightRows) Close() error              { return nil }
func (preflightRows) Next([]driver.Value) error { return io.EOF }

func withPreflightSeams(t *testing.T, cf *CapabilityFile) {
	t.Helper()
	oldLoad, oldOpen := loadCapabilityForPreparation, openDatabaseForPreparation
	loadCapabilityForPreparation = func(string) (*CapabilityFile, error) { preflightDriver.add("load"); return cf, nil }
	openDatabaseForPreparation = func(DatabaseDef) (*sql.DB, error) { return sql.Open(preflightTestDriverName, "") }
	t.Cleanup(func() { loadCapabilityForPreparation, openDatabaseForPreparation = oldLoad, oldOpen })
}

func memoryPreparationLog(events *[]string, content *bytes.Buffer) detailLogFactory {
	return func(LoggingDef) (*preparationDetailLog, error) {
		*events = append(*events, "detail")
		return &preparationDetailLog{
			Writer:         content,
			Sync:           func() error { *events = append(*events, "sync"); return nil },
			Close:          func() error { *events = append(*events, "log-close"); return nil },
			CommitFailure:  func() error { *events = append(*events, "commit"); return nil },
			CleanupSuccess: func() error { *events = append(*events, "cleanup"); return nil },
			AbortTemporary: func() (string, error) { *events = append(*events, "abort"); return "", nil },
			PublicPath:     "/safe/onprest-agent.validate.log",
		}, nil
	}
}

func TestPrepareAgentUsesSingleOrderedPreflightAndTransfersResources(t *testing.T) {
	preflightDriver.reset()
	cf := validCapabilityFile()
	cf.Database.Password = "PASSWORD_SENTINEL"
	withPreflightSeams(t, cf)
	var logEvents []string
	var detail bytes.Buffer
	prepared, err := prepareAgent(context.Background(), Config{CapabilityFile: "ignored"}, memoryPreparationLog(&logEvents, &detail))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(preflightDriver.snapshot(), ","); got != "load,open,ping,explain" {
		t.Fatalf("preflight order=%s", got)
	}
	if strings.Join(logEvents, ",") != "detail" {
		t.Fatalf("detail lifecycle=%v", logEvents)
	}
	if err := prepared.finishValidationSuccess(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(preflightDriver.snapshot(), ","), "close") || strings.Join(logEvents, ",") != "detail,cleanup" {
		t.Fatalf("driver=%v log=%v", preflightDriver.snapshot(), logEvents)
	}
}

func TestPrepareAgentFailureClosesResourcesAndKeepsPrivateDetailOutOfError(t *testing.T) {
	preflightDriver.reset()
	private := "PASSWORD_SENTINEL select * from private_table"
	preflightDriver.queryErr = errors.New(private)
	cf := validCapabilityFile()
	cf.Database.Password = "PASSWORD_SENTINEL"
	cap := cf.Capabilities["get_customer"]
	cap.SQL = "select * from private_table"
	cf.Capabilities["get_customer"] = cap
	withPreflightSeams(t, cf)
	var events []string
	var detail bytes.Buffer
	_, err := prepareAgent(context.Background(), Config{}, memoryPreparationLog(&events, &detail))
	pe, ok := err.(*preparationError)
	if !ok || pe.stage != validationStageCapabilityExplain || pe.capability != "get_customer" || pe.detailLogPath == "" {
		t.Fatalf("error=%#v", err)
	}
	if strings.Contains(pe.Error(), private) || strings.Contains(pe.Error(), "PASSWORD_SENTINEL") {
		t.Fatalf("public error leaked: %q", pe.Error())
	}
	if strings.Contains(detail.String(), "PASSWORD_SENTINEL") || strings.Contains(detail.String(), "select * from private_table") {
		t.Fatalf("detail log leaked secret or SQL: %s", detail.String())
	}
	for _, want := range []string{"[REDACTED]", "[SQL REDACTED]"} {
		if !strings.Contains(detail.String(), want) {
			t.Fatalf("detail missing %q: %s", want, detail.String())
		}
	}
	if !strings.Contains(strings.Join(preflightDriver.snapshot(), ","), "close") || strings.Join(events, ",") != "detail,sync,log-close,commit" {
		t.Fatalf("driver=%v log=%v", preflightDriver.snapshot(), events)
	}
}

func TestRuntimePreparationFailurePreservesStartupDetailRecordContract(t *testing.T) {
	preflightDriver.reset()
	preflightDriver.queryErr = errors.New("private explain detail")
	cf := validCapabilityFile()
	withPreflightSeams(t, cf)
	var events []string
	var detail bytes.Buffer
	factory := memoryPreparationLog(&events, &detail)
	_, err := prepareAgent(context.Background(), Config{}, func(logging LoggingDef) (*preparationDetailLog, error) {
		log, err := factory(logging)
		if log != nil {
			log.Runtime = true
		}
		return log, err
	})
	if err == nil {
		t.Fatal("runtime preparation unexpectedly succeeded")
	}
	got := detail.String()
	for _, want := range []string{`"error_code":"AGENT_STARTUP_FAILED"`, `"message":"agent startup failed"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("runtime detail missing %s: %s", want, got)
		}
	}
	if strings.Contains(got, `"stage"`) || strings.Contains(got, `"message":"capability EXPLAIN validation failed"`) {
		t.Fatalf("runtime detail used validate record contract: %s", got)
	}
}

func TestPrepareAgentConfigFailureDoesNotCreateLogOrOpenDatabase(t *testing.T) {
	preflightDriver.reset()
	oldLoad := loadCapabilityForPreparation
	loadCapabilityForPreparation = func(string) (*CapabilityFile, error) {
		return nil, errors.New("parse capability.yaml: password: SECRET")
	}
	t.Cleanup(func() { loadCapabilityForPreparation = oldLoad })
	factoryCalls := 0
	_, err := prepareAgent(context.Background(), Config{}, func(LoggingDef) (*preparationDetailLog, error) { factoryCalls++; return nil, errors.New("unexpected") })
	pe, ok := err.(*preparationError)
	if !ok || pe.stage != validationStageConfig || pe.Error() != "capability configuration is invalid" || factoryCalls != 0 {
		t.Fatalf("error=%#v factoryCalls=%d", err, factoryCalls)
	}
}

func TestPrepareAgentCancellationStopsExplainAndClosesResources(t *testing.T) {
	preflightDriver.reset()
	preflightDriver.block = true
	cf := validCapabilityFile()
	withPreflightSeams(t, cf)
	var events []string
	var detail bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(10*time.Millisecond, cancel)
	_, err := prepareAgent(ctx, Config{}, memoryPreparationLog(&events, &detail))
	pe, ok := err.(*preparationError)
	if !ok || pe.stage != validationStageCanceled || pe.Error() != "validation canceled" {
		t.Fatalf("error=%#v", err)
	}
	if !strings.Contains(strings.Join(preflightDriver.snapshot(), ","), "close") {
		t.Fatalf("DB was not closed: %v", preflightDriver.snapshot())
	}
}

func TestValidateConfigurationUsesProductionPrepareAndNeverEmitsRuntimeEvents(t *testing.T) {
	preflightDriver.reset()
	cf := validCapabilityFile()
	withPreflightSeams(t, cf)
	oldExe := executablePath
	dir := t.TempDir()
	executablePath = func() (string, error) { return filepath.Join(dir, "onprest-agent"), nil }
	t.Cleanup(func() { executablePath = oldExe })
	outcome := validateConfiguration(context.Background(), Config{CapabilityFile: "ignored"})
	if outcome.err != nil || outcome.report.DatabaseDriver != "postgres" || outcome.report.Capabilities != len(cf.Capabilities) {
		t.Fatalf("outcome=%#v", outcome)
	}
	if err := outcome.release(); err != nil {
		t.Fatal(err)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "*.validate.log")); len(matches) != 0 {
		t.Fatalf("success left validation log: %v", matches)
	}
}

func TestNormalStartupAndValidateUseSameFullYAMLStaticValidation(t *testing.T) {
	preflightDriver.reset()
	oldOpen, oldExecutable := openDatabaseForPreparation, executablePath
	openDatabaseForPreparation = func(DatabaseDef) (*sql.DB, error) {
		return sql.Open(preflightTestDriverName, "")
	}
	executableDir := t.TempDir()
	executablePath = func() (string, error) { return filepath.Join(executableDir, "onprest-agent"), nil }
	t.Cleanup(func() {
		openDatabaseForPreparation, executablePath = oldOpen, oldExecutable
	})

	base := func(driverName string) string {
		port := map[string]int{"postgres": 5432, "mysql": 3306, "sqlserver": 1433, "oracle": 1521}[driverName]
		return fmt.Sprintf(`service:
  title: Static parity fixture
  version: 1.0.0
gateway:
  url: ws://127.0.0.1:8080/ws/agent
  agent_private_key: %s
database:
  driver: %s
  host: 127.0.0.1
  port: %d
  name: fixture
  user: fixture
  password: fixture
logging:
  max_size: 1MB
  max_files: 1
capabilities:
  get_value:
    sql: select :id as id
    params:
      id:
        type: integer
        required: true
    policy:
      readonly: true
      timeout: 1s
      max_rows: 1
      max_bytes: 1KB
    result:
      id: {type: integer}
`, testAgentPrivateKey, driverName, port)
	}
	replace := func(content, old, new string) string {
		t.Helper()
		if !strings.Contains(content, old) {
			t.Fatalf("fixture replacement source not found: %q", old)
		}
		return strings.Replace(content, old, new, 1)
	}
	paramDefinition := func(definition string) string {
		content := base("postgres")
		old := "      id:\n        type: integer\n        required: true"
		definition = "      value:\n        " + strings.ReplaceAll(definition, "\n", "\n        ")
		content = replace(content, old, definition)
		return replace(content, "select :id as id", "select :value as id")
	}

	mergeFixture := replace(base("postgres"), `      id:
        type: integer
        required: true`, `      first: &first
        type: string
        default: inherited
      second: &second
        type: string
        description: merged
      alias_value: *first
      value:
        <<: [*first, *second]
        default: direct`)
	mergeFixture = replace(mergeFixture, "select :id as id", "select :value as id")

	tests := []struct {
		name      string
		content   string
		path      string
		wantValid bool
	}{
		{name: "valid example", path: filepath.Join("..", "..", "examples", "capability.postgres.yaml"), wantValid: true},
		{name: "4DB generated fixture/postgres", content: base("postgres"), wantValid: true},
		{name: "4DB generated fixture/mysql", content: base("mysql"), wantValid: true},
		{name: "4DB generated fixture/sqlserver", content: base("sqlserver"), wantValid: true},
		{name: "4DB generated fixture/oracle", content: base("oracle"), wantValid: true},
		{name: "YAML anchor alias merge sequence direct override", content: mergeFixture, wantValid: true},
		{name: "unknown field", content: "unknown_top_level: true\n" + base("postgres")},
		{name: "multiple YAML documents", content: base("postgres") + "---\nservice: {}\n"},
		{name: "explicit default null", content: paramDefinition("type: string\ndefault: null")},
		{name: "invalid enum", content: paramDefinition("type: integer\nenum: [1, wrong]")},
		{name: "invalid default", content: paramDefinition("type: integer\ndefault: wrong")},
		{name: "invalid range", content: paramDefinition("type: integer\nminimum: 5\nmaximum: 1")},
		{name: "invalid length", content: paramDefinition("type: string\nminLength: 5\nmaxLength: 1")},
		{name: "invalid pattern", content: paramDefinition("type: string\npattern: '['")},
		{name: "invalid format", content: paramDefinition("type: string\nformat: hostname")},
		{name: "max_rows zero", content: replace(base("postgres"), "max_rows: 1", "max_rows: 0")},
		{name: "invalid timeout", content: replace(base("postgres"), "timeout: 1s", "timeout: immediately")},
		{name: "invalid max_bytes", content: replace(base("postgres"), "max_bytes: 1KB", "max_bytes: unlimited")},
		{name: "unsupported SQL operation", content: replace(base("postgres"), "select :id as id", "with value as (select :id as id) select id from value")},
		{name: "multiple statement", content: replace(base("postgres"), "select :id as id", "select :id as id; update fixture set id = 2")},
		{name: "DML with result", content: replace(replace(base("postgres"), "select :id as id", "update fixture set id = :id"), "readonly: true", "readonly: false")},
		{name: "RETURNING", content: replace(replace(base("postgres"), "select :id as id", "insert into fixture(id) values (:id) returning id"), "readonly: true", "readonly: false")},
		{name: "OUTPUT", content: replace(replace(base("sqlserver"), "select :id as id", "update fixture set id = :id output inserted.id"), "readonly: true", "readonly: false")},
		{name: "invalid gateway URL", content: replace(base("postgres"), "ws://127.0.0.1:8080/ws/agent", "ws://gateway.example.com/ws/agent")},
		{name: "invalid gateway private key", content: replace(base("postgres"), testAgentPrivateKey, "not-a-private-key")},
		{name: "invalid database TLS combination", content: replace(base("postgres"), "  password: fixture", "  password: fixture\n  tls:\n    mode: verify-full\n    cert_file: client.pem")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			preflightDriver.reset()
			path := tc.path
			if path == "" {
				path = writeCapabilityFixture(t, tc.content)
			}

			runner, startupErr := NewRunner(context.Background(), Config{CapabilityFile: path}, io.Discard)
			startupValid, startupStage := startupErr == nil, validationStage("")
			if startupErr != nil {
				pe, ok := startupErr.(*preparationError)
				if !ok {
					t.Fatalf("normal startup returned non-classifiable error %T: %v", startupErr, startupErr)
				}
				startupStage = pe.stage
			}
			if runner != nil {
				dbCloseErr := runner.db.Close()
				logCloseErr := runner.detailLogCloser.Close()
				if dbCloseErr != nil || logCloseErr != nil {
					t.Fatalf("close normal startup resources: database=%v detail_log=%v", dbCloseErr, logCloseErr)
				}
			}

			outcome := validateConfiguration(context.Background(), Config{CapabilityFile: path})
			validateValid, validateStage := outcome.err == nil, validationStage("")
			if outcome.err != nil {
				validateStage = outcome.err.stage
			}
			if err := outcome.release(); err != nil {
				t.Fatalf("release validation lock: %v", err)
			}

			if startupValid != tc.wantValid || validateValid != tc.wantValid || startupValid != validateValid || startupStage != validateStage {
				t.Fatalf("want valid=%t; normal startup valid=%t stage=%q; validate valid=%t stage=%q", tc.wantValid, startupValid, startupStage, validateValid, validateStage)
			}
			if !tc.wantValid && startupStage != validationStageConfig {
				t.Fatalf("static fixture failure stage=%q, want %q", startupStage, validationStageConfig)
			}
			if tc.wantValid {
				events := strings.Join(preflightDriver.snapshot(), ",")
				if strings.Count(events, "ping") != 2 || strings.Count(events, "explain")+strings.Count(events, "explain-exec") < 2 || strings.Count(events, "close") != 2 {
					t.Fatalf("success fixture did not reach Ping/EXPLAIN and close both production-path databases: %v", preflightDriver.snapshot())
				}
			}
		})
	}
}

type faultWriter struct {
	short bool
	err   error
}

func (w faultWriter) Write(p []byte) (int, error) {
	if w.short && len(p) > 0 {
		return len(p) - 1, nil
	}
	if w.err != nil {
		return 0, w.err
	}
	return len(p), nil
}

func TestPreparationDiagnosticFaultsBecomeSafeDetailLogErrors(t *testing.T) {
	fault := errors.New("PRIVATE_IO_SENTINEL")
	tests := []struct {
		name        string
		writer      io.Writer
		sync        func() error
		close       func() error
		commit      func() error
		abort       func() (string, error)
		wantCleanup string
	}{
		{name: "write", writer: faultWriter{err: fault}},
		{name: "short write", writer: faultWriter{short: true}},
		{name: "sync", writer: io.Discard, sync: func() error { return fault }},
		{name: "close", writer: io.Discard, close: func() error { return fault }},
		{name: "replace", writer: io.Discard, commit: func() error { return fault }},
		{name: "abort remove", writer: faultWriter{err: fault}, abort: func() (string, error) {
			return "/safe/.onprest-agent.validate.0123456789abcdef0123456789abcdef.tmp", fault
		}, wantCleanup: "/safe/.onprest-agent.validate.0123456789abcdef0123456789abcdef.tmp"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			noop := func() error { return nil }
			abort := func() (string, error) { return "", nil }
			if tc.sync == nil {
				tc.sync = noop
			}
			if tc.close == nil {
				tc.close = noop
			}
			if tc.commit == nil {
				tc.commit = noop
			}
			if tc.abort != nil {
				abort = tc.abort
			}
			p := &agentPreparation{cf: validCapabilityFile(), detailLog: &preparationDetailLog{
				Writer: tc.writer, Sync: tc.sync, Close: tc.close, CommitFailure: tc.commit,
				AbortTemporary: abort, PublicPath: "/safe/onprest-agent.validate.log",
			}}
			err := p.recordFailure(&preparationError{stage: validationStageDatabasePing, publicMessage: "database is unreachable", detailErr: fault})
			pe, ok := err.(*preparationError)
			if !ok || pe.stage != validationStageDetailLog || pe.Error() != validationDiagnosticFailureMessage || pe.detailLogPath != "" || pe.cleanupPath != tc.wantCleanup {
				t.Fatalf("error=%#v", err)
			}
			if strings.Contains(pe.Error(), fault.Error()) {
				t.Fatalf("public error leaked raw I/O error: %q", pe.Error())
			}
		})
	}
}

func TestValidationSuccessCleanupFailureUsesDedicatedContract(t *testing.T) {
	p := &agentPreparation{detailLog: &preparationDetailLog{
		CleanupSuccess: func() error { return errors.New("PRIVATE_REMOVE_SENTINEL") },
		AbortTemporary: func() (string, error) { return "", nil },
	}}
	err := p.finishValidationSuccess()
	pe, ok := err.(*preparationError)
	if !ok || pe.stage != validationStageDetailLog || pe.Error() != validationCleanupFailureMessage || strings.Contains(pe.Error(), "PRIVATE_REMOVE_SENTINEL") {
		t.Fatalf("error=%#v", err)
	}
}

func TestValidationDatabaseCloseFailureIsRecordedWithoutDoubleClose(t *testing.T) {
	preflightDriver.reset()
	cf := validCapabilityFile()
	withPreflightSeams(t, cf)
	oldClose := closeDatabaseForPreparation
	closeDatabaseForPreparation = func(db *sql.DB) error {
		if err := oldClose(db); err != nil {
			return err
		}
		return errors.New("PRIVATE_DB_CLOSE_SENTINEL")
	}
	t.Cleanup(func() { closeDatabaseForPreparation = oldClose })
	var events []string
	var detail bytes.Buffer
	prepared, err := prepareAgent(context.Background(), Config{}, memoryPreparationLog(&events, &detail))
	if err != nil {
		t.Fatal(err)
	}
	if prepared.db == nil {
		t.Fatal("prepare returned no database ownership")
	}
	err = prepared.finishValidationSuccess()
	pe, ok := err.(*preparationError)
	if !ok || pe.stage != validationStageInternal || pe.Error() != "validation could not complete" {
		t.Fatalf("error=%#v", err)
	}
	if got := strings.Count(strings.Join(preflightDriver.snapshot(), ","), "close"); got != 1 {
		t.Fatalf("database close calls=%d events=%v", got, preflightDriver.snapshot())
	}
	if !strings.Contains(detail.String(), "PRIVATE_DB_CLOSE_SENTINEL") || strings.Join(events, ",") != "detail,sync,log-close,commit" {
		t.Fatalf("detail=%q events=%v", detail.String(), events)
	}
}

func TestBusyValidationDoesNotLoadConfigOrTouchDetailFiles(t *testing.T) {
	oldExe, oldLoad := executablePath, loadCapabilityForPreparation
	dir := t.TempDir()
	executablePath = func() (string, error) { return filepath.Join(dir, "onprest-agent"), nil }
	loads := 0
	loadCapabilityForPreparation = func(string) (*CapabilityFile, error) { loads++; return validCapabilityFile(), nil }
	t.Cleanup(func() { executablePath, loadCapabilityForPreparation = oldExe, oldLoad })
	first, err := newValidationLogSession()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	outcome := validateConfiguration(context.Background(), Config{})
	if outcome.err == nil || outcome.err.stage != validationStageBusy || loads != 0 {
		t.Fatalf("outcome=%#v loads=%d", outcome, loads)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, ".onprest-agent.validate.*.tmp")); len(matches) != 0 {
		t.Fatalf("busy validation touched temporary files: %v", matches)
	}
}
