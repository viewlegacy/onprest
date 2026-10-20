package agent

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
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

func assertSanitizedExampleDiagnostic(t *testing.T, message, sentinel string) {
	t.Helper()
	if strings.Contains(message, sentinel) || strings.ContainsAny(message, "\r\n\x1b") || !strings.Contains(message, "<invalid-key>") {
		t.Fatalf("unsafe example key reached public diagnostic: %q", message)
	}
}
