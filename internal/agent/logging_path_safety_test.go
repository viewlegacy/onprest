package agent

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type validationPathInvariant struct {
	path   string
	before os.FileInfo
	verify func(*testing.T)
}

func captureValidationPathInvariant(t *testing.T, path string, verify func(*testing.T)) validationPathInvariant {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return validationPathInvariant{path: path, before: info, verify: verify}
}

func (i validationPathInvariant) assertUnchanged(t *testing.T) {
	t.Helper()
	after, err := os.Lstat(i.path)
	if err != nil {
		t.Fatalf("protected path %s changed: %v", i.path, err)
	}
	if !os.SameFile(i.before, after) || i.before.Mode() != after.Mode() {
		t.Fatalf("protected path %s identity changed: before=%v after=%v", i.path, i.before.Mode(), after.Mode())
	}
	if i.verify != nil {
		i.verify(t)
	}
}

type validationPathFactory struct {
	name   string
	create func(*testing.T, string) validationPathInvariant
}

func regularValidationPath(t *testing.T, path, content string) validationPathInvariant {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return captureValidationPathInvariant(t, path, func(t *testing.T) {
		t.Helper()
		got, err := os.ReadFile(path)
		if err != nil || string(got) != content {
			t.Fatalf("protected file %s content=%q err=%v", path, got, err)
		}
	})
}

func directoryValidationPath(t *testing.T, path string) validationPathInvariant {
	t.Helper()
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(path, "identity-marker")
	if err := os.WriteFile(marker, []byte("directory-marker"), 0o600); err != nil {
		t.Fatal(err)
	}
	return captureValidationPathInvariant(t, path, func(t *testing.T) {
		t.Helper()
		got, err := os.ReadFile(marker)
		if err != nil || string(got) != "directory-marker" {
			t.Fatalf("protected directory marker content=%q err=%v", got, err)
		}
	})
}

func runValidationPathSafetyCLI(t *testing.T, dir string, configErr error) (int, string, string, int, int, int) {
	t.Helper()
	oldExe, oldLoad, oldOpen, oldCreate := executablePath, loadCapabilityForPreparation, openDatabaseForPreparation, validationCreatePrivateFile
	defer func() {
		executablePath, loadCapabilityForPreparation, openDatabaseForPreparation, validationCreatePrivateFile = oldExe, oldLoad, oldOpen, oldCreate
	}()
	executablePath = func() (string, error) { return filepath.Join(dir, "onprest-agent"), nil }
	loads, opens, creates := 0, 0, 0
	validationCreatePrivateFile = func(path string) (*os.File, error) {
		creates++
		return oldCreate(path)
	}
	loadCapabilityForPreparation = func(string) (*CapabilityFile, error) {
		loads++
		return nil, configErr
	}
	openDatabaseForPreparation = func(DatabaseDef) (*sql.DB, error) {
		opens++
		return nil, errors.New("database path safety sentinel")
	}
	var stdout, stderr bytes.Buffer
	code := handleValidateCLIWithValidator(context.Background(), []string{"--format", "json"}, &stdout, &stderr, func(string) string { return "" }, validateConfiguration)
	return code, stdout.String(), stderr.String(), loads, opens, creates
}

func TestValidationRecoveryPreservesUnknownAndSpecialTemporaryPaths(t *testing.T) {
	dir := t.TempDir()
	unknown := regularValidationPath(t, filepath.Join(dir, ".onprest-agent.validate.not-a-run-id.tmp"), "unknown-temporary-marker")
	subdirectory := directoryValidationPath(t, filepath.Join(dir, ".onprest-agent.validate.11111111111111111111111111111111.tmp"))
	reparse := createValidationLinkLikePath(t, filepath.Join(dir, ".onprest-agent.validate.22222222222222222222222222222222.tmp"))

	code, stdout, stderr, loads, opens, creates := runValidationPathSafetyCLI(t, dir, errors.New("capability configuration is invalid"))
	want := "{\"valid\":false,\"stage\":\"config\",\"message\":\"capability configuration is invalid\"}\n"
	if code != 1 || stdout != want || stderr != "" || loads != 1 || opens != 0 || creates != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q loads=%d opens=%d creates=%d", code, stdout, stderr, loads, opens, creates)
	}
	if strings.Contains(stdout+stderr, "unknown-temporary-marker") || strings.Contains(stdout+stderr, "directory-marker") {
		t.Fatalf("protected path content reached public output: stdout=%q stderr=%q", stdout, stderr)
	}
	unknown.assertUnchanged(t)
	subdirectory.assertUnchanged(t)
	reparse.assertUnchanged(t)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".onprest-agent.validate.") && strings.HasSuffix(entry.Name(), ".tmp") &&
			entry.Name() != filepath.Base(unknown.path) && entry.Name() != filepath.Base(subdirectory.path) && entry.Name() != filepath.Base(reparse.path) {
			t.Fatalf("validation created an unexpected temporary path: %s", entry.Name())
		}
	}
}

func TestValidationRejectsInvalidFixedPathsWithoutMutation(t *testing.T) {
	factories := []validationPathFactory{{name: "directory", create: directoryValidationPath}}
	factories = append(factories, invalidFixedValidationPathFactories()...)
	for _, factory := range factories {
		t.Run(factory.name, func(t *testing.T) {
			dir := t.TempDir()
			fixed := filepath.Join(dir, "onprest-agent.validate.log")
			invariant := factory.create(t, fixed)
			code, stdout, stderr, loads, opens, creates := runValidationPathSafetyCLI(t, dir, errors.New("config loader must not run"))
			want := "{\"valid\":false,\"stage\":\"detail_log\",\"message\":\"validation failed, but diagnostic log could not be recorded\"}\n"
			if code != 1 || stdout != want || stderr != "" || loads != 0 || opens != 0 || creates != 0 {
				t.Fatalf("code=%d stdout=%q stderr=%q loads=%d opens=%d creates=%d", code, stdout, stderr, loads, opens, creates)
			}
			invariant.assertUnchanged(t)
			if matches, _ := filepath.Glob(filepath.Join(dir, ".onprest-agent.validate.*.tmp")); len(matches) != 0 {
				t.Fatalf("invalid fixed path validation created temporary files: %v", matches)
			}
		})
	}
}
