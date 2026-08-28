package agent

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRotatingFileWriterRotatesBySize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "onprest-agent.log")
	w, err := newRotatingFileWriter(path, 20, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("1234567890\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("abcdefghij\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != "abcdefghij\n" {
		t.Fatalf("current log = %q", string(current))
	}
	rotated, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if string(rotated) != "1234567890\n" {
		t.Fatalf("rotated log = %q", string(rotated))
	}
}

func TestValidationLogLatestFailureSuccessCleanupAndBusy(t *testing.T) {
	old := executablePath
	defer func() { executablePath = old }()
	dir := t.TempDir()
	executablePath = func() (string, error) { return filepath.Join(dir, "onprest-agent"), nil }

	session, err := newValidationLogSession()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newValidationLogSession(); !errors.Is(err, errValidationBusy) {
		t.Fatalf("second session error=%v, want busy", err)
	}
	writeFailure := func(content string) {
		log, err := session.NewDetailLog(LoggingDef{})
		if err != nil {
			t.Fatal(err)
		}
		if log.TemporaryPath == "" {
			t.Fatal("validation detail log did not expose its owned temporary path")
		}
		if err := validateExistingPrivateFile(log.TemporaryPath); err != nil {
			t.Fatalf("temporary file permission contract: %v", err)
		}
		if _, err := log.Writer.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
		if err := log.Sync(); err != nil {
			t.Fatal(err)
		}
		if err := log.Close(); err != nil {
			t.Fatal(err)
		}
		if err := log.CommitFailure(); err != nil {
			t.Fatal(err)
		}
	}
	writeFailure("failure-a\n")
	fixed := filepath.Join(dir, "onprest-agent.validate.log")
	assertFileContent(t, fixed, "failure-a\n")
	writeFailure("failure-b\n")
	assertFileContent(t, fixed, "failure-b\n")

	log, err := session.NewDetailLog(LoggingDef{})
	if err != nil {
		t.Fatal(err)
	}
	if err := log.CleanupSuccess(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fixed); !os.IsNotExist(err) {
		t.Fatalf("fixed failure log remained after success: %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(dir, ".onprest-agent.validate.lock")); err != nil || info.Size() != 0 {
		t.Fatalf("lock backing file info=%v err=%v", info, err)
	}
	if runtime.GOOS != "windows" {
		for _, path := range []string{filepath.Join(dir, ".onprest-agent.validate.lock")} {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("%s mode=%v", path, info.Mode().Perm())
			}
		}
	}
}

func TestValidationLogBoundAndStrictOrphanRecovery(t *testing.T) {
	var target bytes.Buffer
	w := &boundedWriter{w: &target}
	if _, err := w.Write(make([]byte, validationDetailLogMaxBytes)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("x")); err == nil {
		t.Fatal("bounded writer accepted more than 1 MiB")
	}

	old := executablePath
	defer func() { executablePath = old }()
	dir := t.TempDir()
	executablePath = func() (string, error) { return filepath.Join(dir, "onprest-agent"), nil }
	orphan := filepath.Join(dir, ".onprest-agent.validate.0123456789abcdef0123456789abcdef.tmp")
	unknown := filepath.Join(dir, ".onprest-agent.validate.not-a-run-id.tmp")
	if err := os.WriteFile(orphan, []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unknown, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := newValidationLogSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan remained: %v", err)
	}
	assertFileContent(t, unknown, "keep")
}

func TestValidationLogFaultsPreservePreviousCompletedFailure(t *testing.T) {
	oldExe := executablePath
	oldRandom := validationRandomSource
	oldReplace := validationReplacePrivateFile
	oldRemove := validationRemoveFile
	defer func() {
		executablePath = oldExe
		validationRandomSource = oldRandom
		validationReplacePrivateFile = oldReplace
		validationRemoveFile = oldRemove
	}()
	dir := t.TempDir()
	executablePath = func() (string, error) { return filepath.Join(dir, "onprest-agent"), nil }
	fixed := filepath.Join(dir, "onprest-agent.validate.log")
	if err := os.WriteFile(fixed, []byte("previous-complete"), 0o600); err != nil {
		t.Fatal(err)
	}

	session, err := newValidationLogSession()
	if err != nil {
		t.Fatal(err)
	}
	validationRandomSource = errorReader{err: errors.New("random failed")}
	if _, err := session.NewDetailLog(LoggingDef{}); err == nil {
		t.Fatal("run-id source failure was accepted")
	}
	assertFileContent(t, fixed, "previous-complete")
	validationRandomSource = oldRandom

	log, err := session.NewDetailLog(LoggingDef{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Writer.Write([]byte("new-complete")); err != nil {
		t.Fatal(err)
	}
	if err := log.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	validationReplacePrivateFile = func(string, string) error { return errors.New("replace failed") }
	if err := log.CommitFailure(); err == nil {
		t.Fatal("replace failure was accepted")
	}
	if cleanupPath, err := log.AbortTemporary(); err != nil || cleanupPath != "" {
		t.Fatalf("abort cleanupPath=%q err=%v", cleanupPath, err)
	}
	assertFileContent(t, fixed, "previous-complete")
	validationReplacePrivateFile = oldReplace

	successLog, err := session.NewDetailLog(LoggingDef{})
	if err != nil {
		t.Fatal(err)
	}
	validationRemoveFile = func(path string) error {
		if path == fixed {
			return errors.New("fixed remove failed")
		}
		return os.Remove(path)
	}
	if err := successLog.CleanupSuccess(); err == nil {
		t.Fatal("success fixed-log cleanup failure was accepted")
	}
	assertFileContent(t, fixed, "previous-complete")
	validationRemoveFile = oldRemove
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestValidationPostCommitDurabilityFaultLeavesOnlyCompleteFixedLog(t *testing.T) {
	oldExe, oldHook := executablePath, validationPostCommitDurabilityHook
	defer func() { executablePath, validationPostCommitDurabilityHook = oldExe, oldHook }()
	dir := t.TempDir()
	executablePath = func() (string, error) { return filepath.Join(dir, "onprest-agent"), nil }
	if err := os.WriteFile(filepath.Join(dir, "onprest-agent.validate.log"), []byte("previous"), 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := newValidationLogSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	log, err := session.NewDetailLog(LoggingDef{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Writer.Write([]byte("new-complete")); err != nil {
		t.Fatal(err)
	}
	if err := log.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	validationPostCommitDurabilityHook = func() error { return errors.New("post-commit durability sentinel") }
	if err := log.CommitFailure(); err == nil {
		t.Fatal("post-commit durability fault was accepted")
	}
	assertFileContent(t, filepath.Join(dir, "onprest-agent.validate.log"), "new-complete")
	if cleanupPath, err := log.AbortTemporary(); err != nil || cleanupPath != "" {
		t.Fatalf("abort cleanupPath=%q err=%v", cleanupPath, err)
	}
}

func TestValidationLockAcquireAndReleaseFaultSeams(t *testing.T) {
	oldExe, oldAcquire, oldRelease := executablePath, validationAcquireNativeLock, validationReleaseNativeLock
	defer func() {
		executablePath, validationAcquireNativeLock, validationReleaseNativeLock = oldExe, oldAcquire, oldRelease
	}()
	executablePath = func() (string, error) { return filepath.Join(t.TempDir(), "onprest-agent"), nil }
	validationAcquireNativeLock = func(string) (*validationNativeLock, bool, error) {
		return nil, false, errors.New("open/acquire sentinel")
	}
	if _, err := newValidationLogSession(); err == nil || strings.Contains(err.Error(), "busy") {
		t.Fatalf("acquire fault=%v", err)
	}
	validationAcquireNativeLock = oldAcquire
	dir := t.TempDir()
	executablePath = func() (string, error) { return filepath.Join(dir, "onprest-agent"), nil }
	session, err := newValidationLogSession()
	if err != nil {
		t.Fatal(err)
	}
	releases := 0
	validationReleaseNativeLock = func(lock *validationNativeLock) error {
		releases++
		_ = oldRelease(lock)
		return errors.New("release sentinel")
	}
	if err := session.Close(); err == nil {
		t.Fatal("release fault was accepted")
	}
	if err := session.Close(); err == nil {
		t.Fatal("memoized release fault was lost")
	}
	if releases != 1 {
		t.Fatalf("native release calls=%d, want 1", releases)
	}
}

func TestValidationLogOrphanRemovalFailureStopsBeforeNewTemporary(t *testing.T) {
	oldExe := executablePath
	oldRemove := validationRemoveFile
	defer func() { executablePath, validationRemoveFile = oldExe, oldRemove }()
	dir := t.TempDir()
	executablePath = func() (string, error) { return filepath.Join(dir, "onprest-agent"), nil }
	orphan := filepath.Join(dir, ".onprest-agent.validate.0123456789abcdef0123456789abcdef.tmp")
	if err := os.WriteFile(orphan, []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	validationRemoveFile = func(path string) error {
		if path == orphan {
			return errors.New("orphan remove failed")
		}
		return os.Remove(path)
	}
	if _, err := newValidationLogSession(); err == nil {
		t.Fatal("orphan removal failure was accepted")
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, ".onprest-agent.validate.*.tmp")); len(matches) != 1 || matches[0] != orphan {
		t.Fatalf("unexpected temporary set: %v", matches)
	}
	validationRemoveFile = oldRemove
	session, err := newValidationLogSession()
	if err != nil {
		t.Fatalf("lock was not released after recovery failure: %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

func TestValidateLatestLogCrashRecoveryProcess(t *testing.T) {
	if target := os.Getenv("ONPREST_VALIDATE_CRASH_HELPER_DIR"); target != "" {
		executablePath = func() (string, error) { return filepath.Join(target, "onprest-agent"), nil }
		checkpoint := func() {
			if err := os.WriteFile(filepath.Join(target, "checkpoint"), []byte("ready"), 0o600); err != nil {
				os.Exit(74)
			}
			// Keep a runtime timer alive: a bare select{} can be diagnosed as a
			// process-wide deadlock, releasing the native lock before the parent
			// verifies the live-owner contract.
			for {
				time.Sleep(time.Hour)
			}
		}
		point := os.Getenv("ONPREST_VALIDATE_CRASH_POINT")
		if point == "lock-create" || point == "lock-open" {
			validationLockOpenedHook = func(string) { checkpoint() }
		}
		if point == "orphan-recovery" {
			validationLockAcquiredHook = func(string) { checkpoint() }
		}
		if point == "success-before-fixed-delete" {
			loadCapabilityForPreparation = func(string) (*CapabilityFile, error) { return validCapabilityFile(), nil }
			openDatabaseForPreparation = func(DatabaseDef) (*sql.DB, error) { return sql.Open(preflightTestDriverName, "") }
			validationBeforeFixedSuccessCleanupHook = checkpoint
			outcome := validateConfiguration(context.Background(), Config{})
			if outcome.release != nil {
				_ = outcome.release()
			}
			os.Exit(79)
		}
		session, err := newValidationLogSession()
		if err != nil {
			os.Exit(71)
		}
		if point == "lock-held" {
			checkpoint()
		}
		log, err := session.NewDetailLog(LoggingDef{})
		if err != nil {
			os.Exit(72)
		}
		content := []byte(os.Getenv("ONPREST_VALIDATE_CRASH_CONTENT"))
		if point == "temporary" {
			checkpoint()
		}
		half := len(content) / 2
		if _, err := log.Writer.Write(content[:half]); err != nil {
			os.Exit(76)
		}
		if point == "diagnostic-write" {
			checkpoint()
		}
		if _, err := log.Writer.Write(content[half:]); err != nil {
			os.Exit(77)
		}
		if log.Sync() != nil || log.Close() != nil {
			os.Exit(73)
		}
		if point == "before-replace" {
			checkpoint()
		}
		if log.CommitFailure() != nil {
			os.Exit(78)
		}
		if point == "after-replace" {
			checkpoint()
		}
		if point == "output-after-commit" {
			if err := os.WriteFile(filepath.Join(target, "output"), []byte("written"), 0o600); err != nil {
				os.Exit(75)
			}
			checkpoint()
		}
		checkpoint()
	}

	dir := t.TempDir()
	fixed := filepath.Join(dir, "onprest-agent.validate.log")
	if err := os.WriteFile(fixed, []byte("previous"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := executablePath
	executablePath = func() (string, error) { return filepath.Join(dir, "onprest-agent"), nil }
	defer func() { executablePath = old }()
	runKilledHelper := func(point, content string, whileAlive func()) {
		t.Helper()
		_ = os.Remove(filepath.Join(dir, "checkpoint"))
		cmd := exec.Command(os.Args[0], "-test.run=^TestValidateLatestLogCrashRecoveryProcess$")
		cmd.Env = append(os.Environ(), "ONPREST_VALIDATE_CRASH_HELPER_DIR="+dir, "ONPREST_VALIDATE_CRASH_CONTENT="+content, "ONPREST_VALIDATE_CRASH_POINT="+point)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, err := os.Stat(filepath.Join(dir, "checkpoint")); err == nil {
				break
			}
			if time.Now().After(deadline) {
				_ = cmd.Process.Kill()
				t.Fatal("helper checkpoint timeout")
			}
			time.Sleep(5 * time.Millisecond)
		}
		if whileAlive != nil {
			whileAlive()
		}
		if err := cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		_ = cmd.Wait()
	}

	_ = os.Remove(filepath.Join(dir, ".onprest-agent.validate.lock"))
	runKilledHelper("lock-create", "", nil)
	if info, err := os.Stat(filepath.Join(dir, ".onprest-agent.validate.lock")); err != nil || info.Size() != 0 {
		t.Fatalf("empty lock bootstrap info=%v err=%v", info, err)
	}
	runKilledHelper("lock-open", "", nil)

	runKilledHelper("lock-held", "", func() {
		if _, err := newValidationLogSession(); !errors.Is(err, errValidationBusy) {
			t.Fatalf("live owner did not cause busy: %v", err)
		}
	})
	orphan := filepath.Join(dir, ".onprest-agent.validate.0123456789abcdef0123456789abcdef.tmp")
	if err := os.WriteFile(orphan, []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	runKilledHelper("orphan-recovery", "", nil)
	assertFileContent(t, orphan, "orphan")

	runKilledHelper("temporary", "before-commit", func() {
		matches, _ := filepath.Glob(filepath.Join(dir, ".onprest-agent.validate.*.tmp"))
		if len(matches) != 1 {
			t.Fatalf("live temporary set=%v", matches)
		}
		if err := validateExistingPrivateFile(matches[0]); err != nil {
			t.Fatalf("live temporary permission contract: %v", err)
		}
	})
	assertFileContent(t, fixed, "previous")
	session, err := newValidationLogSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, ".onprest-agent.validate.*.tmp")); len(matches) != 0 {
		t.Fatalf("orphan temporary remained: %v", matches)
	}

	runKilledHelper("diagnostic-write", "partial-diagnostic", nil)
	assertFileContent(t, fixed, "previous")
	runKilledHelper("before-replace", "before-replace", nil)
	assertFileContent(t, fixed, "previous")
	runKilledHelper("after-replace", "after-commit", nil)
	assertFileContent(t, fixed, "after-commit")
	_ = os.Remove(filepath.Join(dir, "output"))
	runKilledHelper("output-after-commit", "after-output", nil)
	assertFileContent(t, fixed, "after-output")
	assertFileContent(t, filepath.Join(dir, "output"), "written")
	assertFileContent(t, fixed, "after-output")
	runKilledHelper("success-before-fixed-delete", "", func() {
		assertFileContent(t, fixed, "after-output")
		if matches, _ := filepath.Glob(filepath.Join(dir, ".onprest-agent.validate.*.tmp")); len(matches) != 0 {
			t.Fatalf("success cleanup checkpoint still had temporary files: %v", matches)
		}
		if _, err := newValidationLogSession(); !errors.Is(err, errValidationBusy) {
			t.Fatalf("success cleanup checkpoint did not retain lifecycle lock: %v", err)
		}
	})
	assertFileContent(t, fixed, "after-output")
	if matches, _ := filepath.Glob(filepath.Join(dir, ".onprest-agent.validate.*.tmp")); len(matches) != 0 {
		t.Fatalf("success cleanup checkpoint left temporary files: %v", matches)
	}
	preflightDriver.reset()
	oldLoad, oldOpen := loadCapabilityForPreparation, openDatabaseForPreparation
	loadCapabilityForPreparation = func(string) (*CapabilityFile, error) { return validCapabilityFile(), nil }
	openDatabaseForPreparation = func(DatabaseDef) (*sql.DB, error) { return sql.Open(preflightTestDriverName, "") }
	outcome := validateConfiguration(context.Background(), Config{})
	loadCapabilityForPreparation, openDatabaseForPreparation = oldLoad, oldOpen
	if outcome.release == nil {
		t.Fatal("next successful validation returned no lock release")
	}
	releaseErr := outcome.release()
	if outcome.err != nil {
		t.Fatalf("next successful validation failed: %v", outcome.err)
	}
	if releaseErr != nil {
		t.Fatalf("next successful validation release: %v", releaseErr)
	}
	if _, err := os.Stat(fixed); !os.IsNotExist(err) {
		t.Fatalf("next successful validation did not remove fixed log: %v", err)
	}
	for i := 0; i < 100; i++ {
		runKilledHelper("temporary", "crash-loop", nil)
		session, err := newValidationLogSession()
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		if err := session.Close(); err != nil {
			t.Fatalf("iteration %d close: %v", i, err)
		}
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, ".onprest-agent.validate.*.tmp")); len(matches) != 0 {
		t.Fatalf("temporary files accumulated: %v", matches)
	}
}

func TestValidationRunIDIsFixedLengthFilesystemSafe(t *testing.T) {
	for i := 0; i < 100; i++ {
		id, err := validationRunID()
		if err != nil {
			t.Fatal(err)
		}
		if len(id) != 32 || strings.Trim(id, "0123456789abcdef") != "" {
			t.Fatalf("run ID=%q", id)
		}
	}
}

func TestRotatingFileWriterHonorsMaxFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "onprest-agent.log")
	w, err := newRotatingFileWriter(path, 5, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, chunk := range []string{"one\n", "two\n", "three\n", "four\n"} {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, path, "four\n")
	assertFileContent(t, path+".1", "three\n")
	assertFileContent(t, path+".2", "two\n")
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatalf("unexpected third rotated file: %v", err)
	}
}

func TestAgentDetailLogPathUsesExecutableDirectory(t *testing.T) {
	old := executablePath
	defer func() { executablePath = old }()
	dir := t.TempDir()
	executablePath = func() (string, error) {
		return filepath.Join(dir, "onprest-agent"), nil
	}
	w, err := newAgentDetailLog(LoggingDef{MaxSize: "1KB", MaxFiles: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("detail\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "onprest-agent.log"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "detail\n" {
		t.Fatalf("detail log = %q", string(b))
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", path, string(got), want)
	}
}
