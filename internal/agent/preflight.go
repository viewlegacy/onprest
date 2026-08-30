package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

type validationStage string

const (
	validationStageConfig            validationStage = "config"
	validationStageBusy              validationStage = "busy"
	validationStageDetailLog         validationStage = "detail_log"
	validationStageDatabaseOpen      validationStage = "database_open"
	validationStageDatabasePing      validationStage = "database_ping"
	validationStageCapabilityExplain validationStage = "capability_explain"
	validationStageCanceled          validationStage = "canceled"
	validationStageInternal          validationStage = "internal"
)

const validationDiagnosticFailureMessage = "validation failed, but diagnostic log could not be recorded"
const validationCleanupFailureMessage = "validation succeeded, but validation detail log cleanup failed"

type preparationError struct {
	stage         validationStage
	capability    string
	publicMessage string
	detailErr     error
	detailLogPath string
	cleanupPath   string
}

func (e *preparationError) Error() string        { return e.publicMessage }
func (e *preparationError) privateDetail() error { return e.detailErr }

type preparationDetailLog struct {
	Writer         io.Writer
	Sync           func() error
	Close          func() error
	CommitFailure  func() error
	CleanupSuccess func() error
	AbortTemporary func() (string, error)
	TemporaryPath  string
	PublicPath     string
	Runtime        bool
}

type agentPreparation struct {
	cf        *CapabilityFile
	caps      map[string]CapabilityDef
	db        *sql.DB
	detailLog *preparationDetailLog
}

type ValidationReport struct {
	DatabaseDriver string
	Capabilities   int
}

var (
	loadCapabilityForPreparation = LoadCapabilityFile
	openDatabaseForPreparation   = openDatabase
	closeDatabaseForPreparation  = func(db *sql.DB) error { return db.Close() }
)

func newRuntimeDetailLogFactory() detailLogFactory {
	return func(logging LoggingDef) (*preparationDetailLog, error) {
		writer, err := newAgentDetailLog(logging)
		if err != nil {
			return nil, err
		}
		return &preparationDetailLog{
			Writer:         writer,
			Sync:           writer.Sync,
			Close:          writer.Close,
			CommitFailure:  func() error { return nil },
			CleanupSuccess: func() error { return nil },
			AbortTemporary: func() (string, error) { return "", nil },
			Runtime:        true,
		}, nil
	}
}

func prepareAgent(ctx context.Context, cfg Config, newDetailLog detailLogFactory) (*agentPreparation, error) {
	cf, err := loadCapabilityForPreparation(cfg.CapabilityFile)
	if err != nil {
		return nil, &preparationError{
			stage: validationStageConfig, publicMessage: safeConfigMessage(err), detailErr: err,
		}
	}
	detailLog, err := newDetailLog(cf.Logging)
	if err != nil {
		return nil, detailLogError(validationDiagnosticFailureMessage, err, "")
	}
	p := &agentPreparation{cf: cf, caps: cf.ByName(), detailLog: detailLog}

	fail := func(pe *preparationError) (*agentPreparation, error) {
		if err := p.recordFailure(pe); err != nil {
			return nil, err
		}
		return nil, pe
	}

	db, err := openDatabaseForPreparation(cf.Database)
	if err != nil {
		pe := &preparationError{stage: validationStageDatabaseOpen, publicMessage: "database could not be opened", detailErr: err}
		return fail(pe)
	}
	p.db = db

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err = db.PingContext(pingCtx)
	cancel()
	if err != nil {
		stage, message := validationStageDatabasePing, "database is unreachable"
		if ctx.Err() != nil {
			stage, message = validationStageCanceled, "validation canceled"
		}
		pe := &preparationError{stage: stage, publicMessage: message, detailErr: err}
		return fail(pe)
	}

	capability, err := p.explainAll(ctx)
	if err != nil {
		stage, message := validationStageCapabilityExplain, "capability EXPLAIN validation failed"
		if ctx.Err() != nil {
			stage, message = validationStageCanceled, "validation canceled"
			capability = ""
		}
		pe := &preparationError{stage: stage, capability: capability, publicMessage: message, detailErr: err}
		return fail(pe)
	}
	return p, nil
}

func (p *agentPreparation) explainAll(parent context.Context) (string, error) {
	for _, cap := range p.cf.CapabilityList() {
		if err := parent.Err(); err != nil {
			return "", err
		}
		params := dummyParams(cap)
		query, args, err := buildSQL(p.cf.Database.Driver, cap.SQL, params)
		if err != nil {
			return cap.Name, fmt.Errorf("explain build: %w", err)
		}
		d, err := timeout(cap.Policy)
		if err != nil {
			return cap.Name, fmt.Errorf("timeout: %w", err)
		}
		ctx, cancel := context.WithTimeout(parent, d)
		err = explainQuery(ctx, p.db, p.cf.Database.Driver, query, args)
		cancel()
		if err != nil {
			return cap.Name, err
		}
	}
	return "", nil
}

func (p *agentPreparation) recordFailure(pe *preparationError) error {
	if closeErr := p.closeDatabase(); closeErr != nil {
		if pe.detailErr == nil {
			pe.detailErr = closeErr
		} else {
			pe.detailErr = errors.Join(pe.detailErr, closeErr)
		}
	}
	if p.detailLog == nil {
		return pe
	}
	detail := ""
	if pe.privateDetail() != nil {
		detail = redactPreparationDetail(pe.privateDetail().Error(), p.cf)
	}
	record := map[string]any{
		"ts": time.Now().UTC().Format(time.RFC3339Nano), "event": "agent_error",
		"capability": pe.capability, "stage": pe.stage, "message": pe.publicMessage, "detail": detail,
	}
	if p.detailLog.Runtime {
		delete(record, "stage")
		record["error_code"] = "AGENT_STARTUP_FAILED"
		record["message"] = "agent startup failed"
	}
	b, err := json.Marshal(record)
	if err == nil {
		b = append(b, '\n')
		var n int
		n, err = p.detailLog.Writer.Write(b)
		if err == nil && n != len(b) {
			err = io.ErrShortWrite
		}
	}
	if err == nil {
		err = p.detailLog.Sync()
	}
	closeErr := p.detailLog.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = p.detailLog.CommitFailure()
	}
	if err != nil {
		cleanupPath, removeErr := p.detailLog.AbortTemporary()
		if removeErr == nil {
			cleanupPath = ""
		}
		return detailLogError(validationDiagnosticFailureMessage, err, cleanupPath)
	}
	pe.detailLogPath = p.detailLog.PublicPath
	return nil
}

func (p *agentPreparation) finishValidationSuccess() error {
	if p.db != nil {
		if err := p.closeDatabase(); err != nil {
			pe := &preparationError{stage: validationStageInternal, publicMessage: "validation could not complete", detailErr: err}
			if recordErr := p.recordFailure(pe); recordErr != nil {
				return recordErr
			}
			return pe
		}
	}
	if err := p.detailLog.CleanupSuccess(); err != nil {
		cleanupPath, removeErr := p.detailLog.AbortTemporary()
		if removeErr == nil {
			cleanupPath = ""
		}
		return detailLogError(validationCleanupFailureMessage, err, cleanupPath)
	}
	return nil
}

func (p *agentPreparation) closeDatabase() error {
	if p.db == nil {
		return nil
	}
	db := p.db
	p.db = nil
	return closeDatabaseForPreparation(db)
}

func detailLogError(message string, detail error, cleanupPath string) *preparationError {
	return &preparationError{stage: validationStageDetailLog, publicMessage: message, detailErr: detail, cleanupPath: cleanupPath}
}

func safeConfigMessage(err error) string {
	message := err.Error()
	var pathErr *os.PathError
	if errors.As(err, &pathErr) || strings.HasPrefix(message, "parse capability.yaml:") {
		return "capability configuration is invalid"
	}
	return message
}

func redactPreparationDetail(detail string, cf *CapabilityFile) string {
	if cf == nil {
		return detail
	}
	for _, secret := range []string{cf.Database.Password, cf.Gateway.AgentPrivateKey, cf.Database.DSN()} {
		if secret != "" {
			detail = strings.ReplaceAll(detail, secret, "[REDACTED]")
		}
	}
	for _, cap := range cf.Capabilities {
		if cap.SQL != "" {
			detail = strings.ReplaceAll(detail, cap.SQL, "[SQL REDACTED]")
		}
	}
	return detail
}

// writeStartupDetail remains the common JSON record shape used by runtime
// request logging tests and compatibility callers. Startup preparation itself
// records failures through agentPreparation.recordFailure.
func writeStartupDetail(w io.Writer, code, detail string) {
	if w == nil {
		return
	}
	fields := map[string]any{
		"ts": time.Now().UTC().Format(time.RFC3339Nano), "event": "agent_error",
		"capability": "", "error_code": code, "message": "agent startup failed", "detail": detail,
	}
	_ = json.NewEncoder(w).Encode(fields)
}
