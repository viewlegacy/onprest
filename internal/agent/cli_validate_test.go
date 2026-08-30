package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateCLIOutputAndExitContract(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		outcome    validationOutcome
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "text success", args: nil, outcome: validationOutcome{report: ValidationReport{DatabaseDriver: "postgres", Capabilities: 2}}, wantCode: 0, wantStdout: "validation succeeded: driver=postgres capabilities=2\n"},
		{name: "json success", args: []string{"--format", "json"}, outcome: validationOutcome{report: ValidationReport{DatabaseDriver: "mysql", Capabilities: 3}}, wantCode: 0, wantStdout: "{\"valid\":true,\"database_driver\":\"mysql\",\"capabilities\":3}\n"},
		{name: "text explain failure", args: nil, outcome: validationOutcome{err: &preparationError{stage: validationStageCapabilityExplain, capability: "update_customer", publicMessage: "capability EXPLAIN validation failed", detailLogPath: "/safe/onprest-agent.validate.log"}}, wantCode: 1, wantStderr: "validate: capability \"update_customer\" EXPLAIN validation failed; see validation detail log: /safe/onprest-agent.validate.log\n"},
		{name: "json failure", args: []string{"--format=json"}, outcome: validationOutcome{err: &preparationError{stage: validationStageDatabasePing, publicMessage: "database is unreachable", detailLogPath: "/safe/onprest-agent.validate.log"}}, wantCode: 1, wantStdout: "{\"valid\":false,\"stage\":\"database_ping\",\"message\":\"database is unreachable\",\"detail_log\":\"/safe/onprest-agent.validate.log\"}\n"},
		{name: "busy", args: []string{"--format", "json"}, outcome: validationOutcome{err: &preparationError{stage: validationStageBusy, publicMessage: "another validation is already running"}}, wantCode: 1, wantStdout: "{\"valid\":false,\"stage\":\"busy\",\"message\":\"another validation is already running\"}\n"},
		{name: "cleanup path", args: []string{"--format", "json"}, outcome: validationOutcome{err: &preparationError{stage: validationStageDetailLog, publicMessage: validationDiagnosticFailureMessage, cleanupPath: "/safe/.onprest-agent.validate.0123456789abcdef0123456789abcdef.tmp"}}, wantCode: 1, wantStdout: "{\"valid\":false,\"stage\":\"detail_log\",\"message\":\"validation failed, but diagnostic log could not be recorded\",\"cleanup_path\":\"/safe/.onprest-agent.validate.0123456789abcdef0123456789abcdef.tmp\"}\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			releases := 0
			tc.outcome.release = func() error { releases++; return nil }
			code := handleValidateCLIWithValidator(context.Background(), tc.args, &stdout, &stderr, func(string) string { return "" }, func(context.Context, Config) validationOutcome { return tc.outcome })
			if code != tc.wantCode || stdout.String() != tc.wantStdout || stderr.String() != tc.wantStderr {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if releases != 1 {
				t.Fatalf("release calls=%d, want 1", releases)
			}
		})
	}
}

func TestValidateCLIUsageNeverCallsValidator(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"invalid format", []string{"--format", "yaml"}},
		{"unknown flag", []string{"--unknown"}},
		{"positional", []string{"secret-positional"}},
		{"missing value", []string{"--config"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			calls := 0
			code := handleValidateCLIWithValidator(context.Background(), tc.args, &stdout, &stderr, func(string) string { return "" }, func(context.Context, Config) validationOutcome {
				calls++
				return validationOutcome{}
			})
			if code != 2 || calls != 0 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "validate: invalid arguments") {
				t.Fatalf("code=%d calls=%d stdout=%q stderr=%q", code, calls, stdout.String(), stderr.String())
			}
			if strings.Contains(stderr.String(), "secret-positional") {
				t.Fatalf("usage error echoed positional input: %q", stderr.String())
			}
		})
	}
	for _, help := range [][]string{{"--help"}, {"-h"}} {
		var stdout, stderr bytes.Buffer
		if code := handleValidateCLIWithValidator(context.Background(), help, &stdout, &stderr, nil, nil); code != 0 || !strings.Contains(stdout.String(), "onprest-agent validate") || stderr.Len() != 0 {
			t.Fatalf("help %v code=%d stdout=%q stderr=%q", help, code, stdout.String(), stderr.String())
		}
	}
}

func TestValidateCLIConfigPrecedenceUsesSharedResolver(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		t.Fatal(err)
	}
	binaryAdjacentConfig := filepath.Join(filepath.Dir(executable), "capability.yaml")

	tests := []struct {
		name           string
		args           []string
		env            string
		want           string
		binaryAdjacent bool
	}{
		{name: "config", args: []string{"--config", "primary.yaml", "--capability-file", "alias.yaml"}, env: "env.yaml", want: "primary.yaml"},
		{name: "alias", args: []string{"--capability-file", "alias.yaml"}, env: "env.yaml", want: "alias.yaml"},
		{name: "environment", env: "env.yaml", want: "env.yaml"},
		{name: "binary adjacent fallback", want: binaryAdjacentConfig, binaryAdjacent: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			validatorCalled := false
			code := handleValidateCLIWithValidator(context.Background(), tc.args, ioDiscard{}, ioDiscard{}, func(key string) string {
				if key == "AGENT_CAPABILITY_FILE" {
					return tc.env
				}
				return ""
			}, func(_ context.Context, cfg Config) validationOutcome {
				validatorCalled = true
				if cfg.CapabilityFile != tc.want {
					t.Errorf("CapabilityFile = %q, want %q", cfg.CapabilityFile, tc.want)
				}
				if tc.binaryAdjacent {
					if !filepath.IsAbs(cfg.CapabilityFile) {
						t.Errorf("CapabilityFile = %q, want absolute path", cfg.CapabilityFile)
					}
					if filepath.Dir(cfg.CapabilityFile) != filepath.Dir(executable) {
						t.Errorf("CapabilityFile directory = %q, want binary directory %q", filepath.Dir(cfg.CapabilityFile), filepath.Dir(executable))
					}
				}
				return validationOutcome{report: ValidationReport{DatabaseDriver: "postgres", Capabilities: 1}}
			})
			if code != 0 || !validatorCalled {
				t.Fatalf("args=%v code=%d validatorCalled=%t", tc.args, code, validatorCalled)
			}
		})
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }

func TestTopLevelUnknownArgumentsAreUsageErrors(t *testing.T) {
	for _, args := range [][]string{{"unknown"}, {"--unknown"}, {"--config"}} {
		var stdout, stderr bytes.Buffer
		handled, code := HandleCLI(context.Background(), args, &stdout, &stderr)
		if !handled || code != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "invalid command or arguments") {
			t.Fatalf("args=%v handled=%t code=%d stdout=%q stderr=%q", args, handled, code, stdout.String(), stderr.String())
		}
	}
	for _, args := range [][]string{nil, {"--config", "file.yaml"}, {"--capability-file=file.yaml"}} {
		handled, code := HandleCLI(context.Background(), args, ioDiscard{}, ioDiscard{})
		if handled || code != 0 {
			t.Fatalf("startup args=%v handled=%t code=%d", args, handled, code)
		}
	}
}

func TestPreparationErrorDoesNotExposePrivateDetailThroughFormattingOrUnwrap(t *testing.T) {
	sentinel := "password=PRIVATE_SENTINEL select * from secret_table"
	err := &preparationError{stage: validationStageDatabasePing, publicMessage: "database is unreachable", detailErr: errors.New(sentinel)}
	for _, formatted := range []string{err.Error(), strings.TrimSpace(string(mustJSON(t, err))), fmt.Sprintf("%v", err), fmt.Sprintf("%+v", fmt.Errorf("wrapped: %w", err))} {
		if strings.Contains(formatted, sentinel) {
			t.Fatalf("private detail leaked: %q", formatted)
		}
	}
	if errors.Unwrap(err) != nil {
		t.Fatal("preparationError unexpectedly exposes private error through Unwrap")
	}
}

func TestValidateCLIOutputWriterFailureIsNotSuccess(t *testing.T) {
	writerErr := errors.New("output unavailable")
	writer := faultWriter{err: writerErr}
	var stderr bytes.Buffer
	code := handleValidateCLIWithValidator(context.Background(), nil, writer, &stderr, func(string) string { return "" }, func(context.Context, Config) validationOutcome {
		return validationOutcome{report: ValidationReport{DatabaseDriver: "postgres", Capabilities: 1}}
	})
	if code != 1 || !strings.Contains(stderr.String(), "validate: output failed") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
}

func TestValidateFailureOutputWriterFaultPreservesCommittedFixedLog(t *testing.T) {
	dir := t.TempDir()
	fixed := filepath.Join(dir, "onprest-agent.validate.log")
	if err := os.WriteFile(fixed, []byte("complete failure"), 0o600); err != nil {
		t.Fatal(err)
	}
	code := handleValidateCLIWithValidator(context.Background(), []string{"--format", "json"}, faultWriter{err: errors.New("output unavailable")}, ioDiscard{}, func(string) string { return "" }, func(context.Context, Config) validationOutcome {
		return validationOutcome{err: &preparationError{stage: validationStageDatabasePing, publicMessage: "database is unreachable", detailLogPath: fixed}}
	})
	if code != 1 {
		t.Fatalf("code=%d", code)
	}
	b, err := os.ReadFile(fixed)
	if err != nil || string(b) != "complete failure" {
		t.Fatalf("fixed=%q err=%v", b, err)
	}
}

func TestValidateCLIReleaseFaultDoesNotContradictCompletedOutput(t *testing.T) {
	releaseErr := errors.New("unlock sentinel")
	tests := []struct {
		name       string
		args       []string
		outcome    validationOutcome
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "text success", outcome: validationOutcome{report: ValidationReport{DatabaseDriver: "postgres", Capabilities: 1}}, wantCode: 0, wantStdout: "validation succeeded: driver=postgres capabilities=1\n"},
		{name: "json success", args: []string{"--format", "json"}, outcome: validationOutcome{report: ValidationReport{DatabaseDriver: "postgres", Capabilities: 1}}, wantCode: 0, wantStdout: "{\"valid\":true,\"database_driver\":\"postgres\",\"capabilities\":1}\n"},
		{name: "text failure", outcome: validationOutcome{err: &preparationError{stage: validationStageDatabasePing, publicMessage: "database is unreachable"}}, wantCode: 1, wantStderr: "validate: database is unreachable\n"},
		{name: "json failure", args: []string{"--format", "json"}, outcome: validationOutcome{err: &preparationError{stage: validationStageDatabasePing, publicMessage: "database is unreachable"}}, wantCode: 1, wantStdout: "{\"valid\":false,\"stage\":\"database_ping\",\"message\":\"database is unreachable\"}\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			releases := 0
			tc.outcome.release = func() error { releases++; return releaseErr }
			code := handleValidateCLIWithValidator(context.Background(), tc.args, &stdout, &stderr, func(string) string { return "" }, func(context.Context, Config) validationOutcome { return tc.outcome })
			if code != tc.wantCode || stdout.String() != tc.wantStdout || stderr.String() != tc.wantStderr || releases != 1 {
				t.Fatalf("code=%d stdout=%q stderr=%q releases=%d", code, stdout.String(), stderr.String(), releases)
			}
		})
	}
}

type observedValidationOutputWriter struct {
	faultFirst bool
	shortFirst bool
	writes     int
	content    bytes.Buffer
	onWrite    func([]byte)
}

func (w *observedValidationOutputWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.onWrite != nil {
		w.onWrite(p)
	}
	if w.writes == 1 && w.faultFirst {
		return 0, errors.New("output write sentinel")
	}
	if w.writes == 1 && w.shortFirst && len(p) > 0 {
		_, _ = w.content.Write(p[:len(p)-1])
		return len(p) - 1, nil
	}
	return w.content.Write(p)
}

func TestValidateCLIOutputFaultKeepsLockThroughFallbackAndReleasesExactlyOnce(t *testing.T) {
	oldExecutablePath := executablePath
	defer func() { executablePath = oldExecutablePath }()

	for _, format := range []string{"text", "json"} {
		for _, validationFails := range []bool{false, true} {
			for _, fault := range []string{"write", "short-write"} {
				name := fmt.Sprintf("%s/validation-failure=%t/%s", format, validationFails, fault)
				t.Run(name, func(t *testing.T) {
					dir := t.TempDir()
					executablePath = func() (string, error) { return filepath.Join(dir, "onprest-agent"), nil }
					releases := 0
					mainWrites, fallbackWrites := 0, 0
					observeWrite := func(p []byte) {
						if releases != 0 {
							t.Fatalf("release count inside output writer=%d, want 0", releases)
						}
						if string(p) == "validate: output failed\n" {
							fallbackWrites++
						} else {
							mainWrites++
						}
						if second, err := newValidationLogSession(); !errors.Is(err, errValidationBusy) {
							if second != nil {
								_ = second.Close()
							}
							t.Fatalf("second validation during output write error=%v, want busy", err)
						}
					}

					stdout := &observedValidationOutputWriter{onWrite: observeWrite}
					stderr := &observedValidationOutputWriter{onWrite: observeWrite}
					mainWriter := stdout
					if format == "text" && validationFails {
						mainWriter = stderr
					}
					if fault == "write" {
						mainWriter.faultFirst = true
					} else {
						mainWriter.shortFirst = true
					}

					args := []string(nil)
					if format == "json" {
						args = []string{"--format", "json"}
					}
					code := handleValidateCLIWithValidator(context.Background(), args, stdout, stderr, func(string) string { return "" }, func(context.Context, Config) validationOutcome {
						session, err := newValidationLogSession()
						if err != nil {
							t.Fatalf("create first validation session: %v", err)
						}
						outcome := validationOutcome{
							report: ValidationReport{DatabaseDriver: "postgres", Capabilities: 1},
							release: func() error {
								releases++
								return session.Close()
							},
						}
						if validationFails {
							outcome.err = &preparationError{stage: validationStageDatabasePing, publicMessage: "database is unreachable"}
						}
						return outcome
					})
					if code != 1 || mainWrites != 1 || fallbackWrites != 1 || releases != 1 {
						t.Fatalf("code=%d mainWrites=%d fallbackWrites=%d releases=%d stdout=%q stderr=%q", code, mainWrites, fallbackWrites, releases, stdout.content.String(), stderr.content.String())
					}
					if !strings.Contains(stderr.content.String(), "validate: output failed\n") {
						t.Fatalf("fallback output missing: %q", stderr.content.String())
					}
					after, err := newValidationLogSession()
					if err != nil {
						t.Fatalf("lock remained held after return: %v", err)
					}
					if err := after.Close(); err != nil {
						t.Fatal(err)
					}
				})
			}
		}
	}
}

func TestValidateCLIJSONEncoderFailureKeepsLockAndEmitsOnlyFixedFallback(t *testing.T) {
	oldExecutablePath := executablePath
	dir := t.TempDir()
	executablePath = func() (string, error) { return filepath.Join(dir, "onprest-agent"), nil }
	t.Cleanup(func() { executablePath = oldExecutablePath })

	for _, validationFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("validation-failure=%t", validationFails), func(t *testing.T) {
			releases := 0
			assertLocked := func(point string) {
				if releases != 0 {
					t.Fatalf("release count at %s=%d, want 0", point, releases)
				}
				second, err := newValidationLogSession()
				if second != nil {
					_ = second.Close()
				}
				if !errors.Is(err, errValidationBusy) {
					t.Fatalf("second validation at %s error=%v, want busy", point, err)
				}
			}
			var stdout bytes.Buffer
			stderr := &observedValidationOutputWriter{onWrite: func(p []byte) {
				if string(p) != "validate: output failed\n" {
					t.Fatalf("unexpected fallback bytes: %q", p)
				}
				assertLocked("fallback write")
			}}
			encodeErr := errors.New("JSON_ENCODER_SENTINEL")
			encoderCalls := 0
			code := handleValidateCLIWithJSONEncoder(context.Background(), []string{"--format", "json"}, &stdout, stderr, func(string) string { return "" }, func(context.Context, Config) validationOutcome {
				session, err := newValidationLogSession()
				if err != nil {
					t.Fatalf("create validation session: %v", err)
				}
				outcome := validationOutcome{
					report: ValidationReport{DatabaseDriver: "postgres", Capabilities: 1},
					release: func() error {
						releases++
						return session.Close()
					},
				}
				if validationFails {
					outcome.err = &preparationError{stage: validationStageDatabasePing, publicMessage: "database is unreachable"}
				}
				return outcome
			}, func(w io.Writer, _ validationJSONOutput) error {
				encoderCalls++
				assertLocked("JSON encode")
				if _, err := w.Write([]byte(`{"valid":`)); err != nil {
					t.Fatalf("write partial encoded bytes: %v", err)
				}
				return encodeErr
			})

			if code != 1 || encoderCalls != 1 || releases != 1 || stdout.Len() != 0 || stderr.content.String() != "validate: output failed\n" {
				t.Fatalf("code=%d encoderCalls=%d releases=%d stdout=%q stderr=%q", code, encoderCalls, releases, stdout.String(), stderr.content.String())
			}
			if strings.Contains(stdout.String(), "valid") || strings.Contains(stderr.content.String(), encodeErr.Error()) {
				t.Fatalf("partial JSON or private encoder error escaped: stdout=%q stderr=%q", stdout.String(), stderr.content.String())
			}
			after, err := newValidationLogSession()
			if err != nil {
				t.Fatalf("lock remained held after CLI return: %v", err)
			}
			if err := after.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
