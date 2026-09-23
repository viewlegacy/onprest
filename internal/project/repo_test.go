package project

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestMakeBuildProducesOnlyRunnableGatewayAndAgent(t *testing.T) {
	root := repoRoot(t)
	dist := t.TempDir()
	runRepoCommand(t, root, []string{"DIST_DIR=" + dist}, "make", "build")
	entries, err := os.ReadDir(dist)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("make build entries = %v, want exactly two binaries", entryNames(entries))
	}
	for _, name := range []string{"onprest-gateway", "onprest-agent"} {
		path := filepath.Join(dist, name)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
		if info.Mode()&0o111 == 0 {
			t.Fatalf("%s is not executable: %v", name, info.Mode())
		}
		runRepoCommand(t, root, nil, path, "--help")
		if got := strings.TrimSpace(runRepoCommand(t, root, nil, path, "--version")); got != "dev" {
			t.Fatalf("%s --version=%q, want dev for an uninjected build", name, got)
		}
	}
	packages := runRepoCommand(t, root, nil, "go", "list", "./...")
	if strings.Contains(strings.ToLower(packages), "dashboard") || strings.Contains(packages, "/manage") {
		t.Fatalf("OSS package list contains managed/dashboard code:\n%s", packages)
	}
}

func TestMakeBuildInjectsOneVersionIntoBothBinaries(t *testing.T) {
	root := repoRoot(t)
	dist := t.TempDir()
	runRepoCommand(t, root, []string{"DIST_DIR=" + dist, "VERSION=1.2.4"}, "make", "build")
	for _, name := range []string{"onprest-gateway", "onprest-agent"} {
		for _, arg := range []string{"version", "--version", "-v"} {
			got := strings.TrimSpace(runRepoCommand(t, root, nil, filepath.Join(dist, name), arg))
			if got != "1.2.4" {
				t.Fatalf("%s %s=%q, want 1.2.4", name, arg, got)
			}
		}
	}
}

func TestMakeBuildCrossProducesBothBinariesForEveryTarget(t *testing.T) {
	root := repoRoot(t)
	dist := t.TempDir()
	runRepoCommand(t, root, []string{"DIST_DIR=" + dist, "VERSION=1.2.4"}, "make", "build-cross")
	targets := []string{"linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64", "windows-amd64"}
	for _, target := range targets {
		ext := ""
		if strings.HasPrefix(target, "windows-") {
			ext = ".exe"
		}
		entries, err := os.ReadDir(filepath.Join(dist, target))
		if err != nil {
			t.Fatalf("target %s: %v", target, err)
		}
		if len(entries) != 2 {
			t.Fatalf("target %s entries = %v, want two", target, entryNames(entries))
		}
		for _, binary := range []string{"onprest-gateway" + ext, "onprest-agent" + ext} {
			info, err := os.Stat(filepath.Join(dist, target, binary))
			if err != nil || info.Size() == 0 {
				t.Fatalf("target %s binary %s missing/empty: info=%v err=%v", target, binary, info, err)
			}
		}
	}
	nativeTarget := runtime.GOOS + "-" + runtime.GOARCH
	if nativeTarget != "windows-amd64" {
		for _, binary := range []string{"onprest-gateway", "onprest-agent"} {
			if info, err := os.Stat(filepath.Join(dist, nativeTarget, binary)); err == nil && info.Mode()&0o111 == 0 {
				t.Fatalf("native cross-built %s is not executable", binary)
			}
			got := strings.TrimSpace(runRepoCommand(t, root, nil, filepath.Join(dist, nativeTarget, binary), "--version"))
			if got != "1.2.4" {
				t.Fatalf("native cross-built %s --version=%q, want 1.2.4", binary, got)
			}
		}
	}
}

func TestReleasePackageContainsCanonicalTargetsAndVerifiableMetadata(t *testing.T) {
	root := repoRoot(t)
	releaseDir := t.TempDir()
	sha := strings.TrimSpace(runRepoCommand(t, root, nil, "git", "rev-parse", "HEAD"))
	env := []string{
		"VERSION=1.2.12",
		"RELEASE_TAG=v1.2.12",
		"RELEASE_SHA=" + sha,
		"RELEASE_READY_URL=https://github.com/viewlegacy/onprest/actions/runs/123",
		"RELEASE_DIR=" + releaseDir,
	}
	runRepoCommand(t, root, env, "make", "package-release")

	targets := []string{"linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64", "windows-amd64"}
	for _, target := range targets {
		ext := ".tar.gz"
		if strings.HasPrefix(target, "windows-") {
			ext = ".zip"
		}
		archivePath := filepath.Join(releaseDir, "onprest-1.2.12-"+target+ext)
		entries := releaseArchiveEntries(t, archivePath)
		rootName := "onprest-1.2.12-" + target + "/"
		binaryExt := ""
		if strings.HasPrefix(target, "windows-") {
			binaryExt = ".exe"
		}
		for _, want := range []string{
			rootName + "onprest-gateway" + binaryExt,
			rootName + "onprest-agent" + binaryExt,
			rootName + "LICENSE",
			rootName + "gateway.env.example",
			rootName + "capability.yaml.example",
			rootName + "INSTALL.md",
			rootName + "RELEASE-MANIFEST.txt",
			rootName + "dependencies/onprest-gateway.txt",
			rootName + "dependencies/onprest-agent.txt",
		} {
			if !entries[want] {
				t.Fatalf("%s missing %s", filepath.Base(archivePath), want)
			}
		}
		for _, forbidden := range []string{"quickstart/gateway.env", "quickstart/capability.postgres.yaml", "quickstart/postgres.compose.yml", "quickstart/postgres-init.sql"} {
			if entries[rootName+forbidden] {
				t.Fatalf("production archive contains development-only %s", forbidden)
			}
		}
		for _, template := range []string{"gateway.env.example", "capability.yaml.example"} {
			body := releaseArchiveFile(t, archivePath, rootName+template)
			for _, secret := range []string{"TrMm87V3aET3MmGUzHf3_XKZRPEHe1bDM-POH1mrjr8", "onprest-example-password", "orjrqqPeX8FXhsECOnrnOr6oa70pOYjyeUWmxTbaZrM"} {
				if bytes.Contains(body, []byte(secret)) {
					t.Fatalf("production archive %s includes public Quick Start credential", template)
				}
			}
		}
	}
	quickstartArchive := filepath.Join(releaseDir, "onprest-1.2.12-quickstart.tar.gz")
	quickstartRoot := "onprest-1.2.12-quickstart/"
	quickstartEntries := releaseArchiveEntries(t, quickstartArchive)
	for archiveName, sourceName := range map[string]string{
		"gateway.env":              "examples/gateway.env",
		"capability.postgres.yaml": "examples/capability.postgres.yaml",
		"postgres.compose.yml":     "examples/postgres.compose.yml",
		"postgres-init.sql":        "examples/postgres-init.sql",
		"README.md":                "release/QUICKSTART.md",
	} {
		if !quickstartEntries[quickstartRoot+archiveName] {
			t.Fatalf("quickstart archive missing %s", archiveName)
		}
		got := releaseArchiveFile(t, quickstartArchive, quickstartRoot+archiveName)
		want, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(sourceName)))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("quickstart %s differs from %s", archiveName, sourceName)
		}
	}

	dependencies := readText(t, filepath.Join(releaseDir, "DEPENDENCIES.txt"))
	if strings.Count(dependencies, "## ") != 10 {
		t.Fatalf("dependency report has %d binary sections, want 10", strings.Count(dependencies, "## "))
	}
	evidence := readText(t, filepath.Join(releaseDir, "RELEASE-EVIDENCE.txt"))
	for _, want := range []string{"release_tag=v1.2.12", "version=1.2.12", "commit_sha=" + sha, "actions/runs/123", "refs/tags/v1.2.12"} {
		if !strings.Contains(evidence, want) {
			t.Fatalf("release evidence missing %q", want)
		}
	}

	if runtime.GOOS != "windows" {
		native := runtime.GOOS + "-" + runtime.GOARCH
		archivePath := filepath.Join(releaseDir, "onprest-1.2.12-"+native+".tar.gz")
		extractDir := t.TempDir()
		runRepoCommand(t, root, nil, "tar", "-xzf", archivePath, "-C", extractDir)
		for _, binary := range []string{"onprest-gateway", "onprest-agent"} {
			got := strings.TrimSpace(runRepoCommand(t, t.TempDir(), nil, filepath.Join(extractDir, "onprest-1.2.12-"+native, binary), "--version"))
			if got != "1.2.12" {
				t.Fatalf("packaged %s --version=%q", binary, got)
			}
		}
	}

	if err := os.WriteFile(filepath.Join(releaseDir, "VULNERABILITY-EVIDENCE.txt"), []byte(fakeVulnerabilityEvidence("1.2.12", sha)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(releaseDir, ".archive-digests")); err != nil {
		t.Fatal(err)
	}
	runRepoCommand(t, root, env, "make", "finalize-release-artifacts")
	runRepoCommand(t, root, env, "make", "verify-release-artifacts")
	nativeTarget := runtime.GOOS + "/" + runtime.GOARCH
	nativeName := strings.ReplaceAll(nativeTarget, "/", "-")
	nativeExt := ".tar.gz"
	if runtime.GOOS == "windows" {
		nativeExt = ".zip"
	}
	extracted := t.TempDir()
	runRepoCommand(t, root, nil, "go", "run", "./internal/releaseverify", "extract",
		filepath.Join(releaseDir, "onprest-1.2.12-"+nativeName+nativeExt), extracted,
		"1.2.12", "v1.2.12", sha, "https://github.com/viewlegacy/onprest/actions/runs/123",
		nativeTarget, filepath.Join(releaseDir, "SHA256SUMS"))
	for _, binary := range []string{"onprest-gateway", "onprest-agent"} {
		if runtime.GOOS == "windows" {
			binary += ".exe"
		}
		if _, err := os.Stat(filepath.Join(extracted, "onprest-1.2.12-"+nativeName, binary)); err != nil {
			t.Fatalf("safe extraction missing %s: %v", binary, err)
		}
	}
	quickstartExtract := t.TempDir()
	runRepoCommand(t, root, nil, "go", "run", "./internal/releaseverify", "extract",
		quickstartArchive, quickstartExtract, "1.2.12", "v1.2.12", sha,
		"https://github.com/viewlegacy/onprest/actions/runs/123", "quickstart", filepath.Join(releaseDir, "SHA256SUMS"))
	for _, name := range []string{"README.md", "gateway.env", "capability.postgres.yaml", "postgres.compose.yml", "postgres-init.sql"} {
		if _, err := os.Stat(filepath.Join(quickstartExtract, "onprest-1.2.12-quickstart", name)); err != nil {
			t.Fatalf("safe quickstart extraction missing %s: %v", name, err)
		}
	}
	checksum := readText(t, filepath.Join(releaseDir, "SHA256SUMS"))
	if strings.Count(strings.TrimSpace(checksum), "\n")+1 != 9 {
		t.Fatalf("SHA256SUMS entries=%d, want 9", strings.Count(strings.TrimSpace(checksum), "\n")+1)
	}

	for _, mutation := range []struct {
		name     string
		archive  string
		kind     string
		injected string
	}{
		{name: "manifest mismatch after checksum regeneration", archive: "onprest-1.2.12-linux-amd64.tar.gz", kind: "manifest"},
		{name: "Unix binary loses executable mode", archive: "onprest-1.2.12-linux-amd64.tar.gz", kind: "mode"},
		{name: "required entry missing", archive: "onprest-1.2.12-linux-amd64.tar.gz", kind: "missing"},
		{name: "symlink entry", archive: "onprest-1.2.12-linux-amd64.tar.gz", kind: "symlink"},
		{name: "hardlink entry", archive: "onprest-1.2.12-linux-amd64.tar.gz", kind: "hardlink"},
		{name: "relative traversal entry", archive: "onprest-1.2.12-linux-amd64.tar.gz", kind: "relative", injected: "../onprest-verifier-escape"},
		{name: "absolute tar entry", archive: "onprest-1.2.12-linux-amd64.tar.gz", kind: "absolute", injected: filepath.Join(t.TempDir(), "onprest-verifier-escape")},
		{name: "duplicate tar entry", archive: "onprest-1.2.12-linux-amd64.tar.gz", kind: "duplicate"},
		{name: "extra tar entry", archive: "onprest-1.2.12-linux-amd64.tar.gz", kind: "extra"},
		{name: "quickstart required entry missing", archive: "onprest-1.2.12-quickstart.tar.gz", kind: "quickstart-missing"},
		{name: "quickstart extra entry", archive: "onprest-1.2.12-quickstart.tar.gz", kind: "quickstart-extra"},
		{name: "quickstart traversal entry", archive: "onprest-1.2.12-quickstart.tar.gz", kind: "relative", injected: "../onprest-quickstart-escape"},
		{name: "relative traversal zip entry", archive: "onprest-1.2.12-windows-amd64.zip", kind: "relative", injected: "../onprest-verifier-escape"},
		{name: "absolute zip entry", archive: "onprest-1.2.12-windows-amd64.zip", kind: "absolute", injected: filepath.Join(t.TempDir(), "onprest-verifier-escape")},
		{name: "zip symlink entry", archive: "onprest-1.2.12-windows-amd64.zip", kind: "symlink"},
		{name: "zip special mode entry", archive: "onprest-1.2.12-windows-amd64.zip", kind: "mode"},
		{name: "duplicate zip entry", archive: "onprest-1.2.12-windows-amd64.zip", kind: "duplicate"},
		{name: "extra zip entry", archive: "onprest-1.2.12-windows-amd64.zip", kind: "extra"},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			mutatedDir := cloneReleaseAssets(t, releaseDir)
			archive := filepath.Join(mutatedDir, mutation.archive)
			if strings.HasSuffix(archive, ".zip") {
				mutateZipArchive(t, archive, mutation.kind, mutation.injected)
			} else {
				mutateTarArchive(t, archive, mutation.kind, mutation.injected)
			}
			mutatedEnv := replaceEnv(env, "RELEASE_DIR", mutatedDir)
			runRepoCommand(t, root, mutatedEnv, "make", "finalize-release-artifacts")
			runRepoCommandMustFail(t, root, mutatedEnv, "make", "verify-release-artifacts")
			target := "linux/amd64"
			if strings.HasSuffix(archive, ".zip") {
				target = "windows/amd64"
			} else if strings.Contains(archive, "-quickstart.tar.gz") {
				target = "quickstart"
			}
			extractOutput := t.TempDir()
			runRepoCommandMustFail(t, root, nil, "go", "run", "./internal/releaseverify", "extract",
				archive, extractOutput, "1.2.12", "v1.2.12", sha,
				"https://github.com/viewlegacy/onprest/actions/runs/123", target, filepath.Join(mutatedDir, "SHA256SUMS"))
			if entries, err := os.ReadDir(extractOutput); err != nil || len(entries) != 0 {
				t.Fatalf("failed safe extraction wrote output: entries=%v err=%v", entryNames(entries), err)
			}
			if mutation.injected != "" && filepath.IsAbs(mutation.injected) {
				if _, err := os.Stat(mutation.injected); !os.IsNotExist(err) {
					t.Fatalf("unsafe archive path was created: %s", mutation.injected)
				}
			}
		})
	}
	for _, mutation := range []struct {
		name string
		old  string
		new  string
	}{
		{name: "source vulnerability scan missing", old: "## source ./...\n", new: ""},
		{name: "vulnerability evidence has another SHA", old: "commit_sha=" + sha, new: "commit_sha=0000000000000000000000000000000000000000"},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			mutatedDir := cloneReleaseAssets(t, releaseDir)
			path := filepath.Join(mutatedDir, "VULNERABILITY-EVIDENCE.txt")
			body := readText(t, path)
			if err := os.WriteFile(path, []byte(strings.Replace(body, mutation.old, mutation.new, 1)), 0o644); err != nil {
				t.Fatal(err)
			}
			mutatedEnv := replaceEnv(env, "RELEASE_DIR", mutatedDir)
			runRepoCommand(t, root, mutatedEnv, "make", "finalize-release-artifacts")
			runRepoCommandMustFail(t, root, mutatedEnv, "make", "verify-release-artifacts")
		})
	}
	archiveToCorrupt := filepath.Join(releaseDir, "onprest-1.2.12-linux-amd64.tar.gz")
	f, err := os.OpenFile(archiveToCorrupt, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("corrupt"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	runRepoCommandMustFail(t, root, env, "make", "verify-release-artifacts")
}

func TestReleasePackageRejectsMismatchedTagAndCommitBeforeBuilding(t *testing.T) {
	root := repoRoot(t)
	sha := strings.TrimSpace(runRepoCommand(t, root, nil, "git", "rev-parse", "HEAD"))
	base := []string{
		"VERSION=1.2.12",
		"RELEASE_SHA=" + sha,
		"RELEASE_READY_URL=https://github.com/viewlegacy/onprest/actions/runs/123",
		"RELEASE_DIR=" + t.TempDir(),
	}
	runRepoCommandMustFail(t, root, append(base, "RELEASE_TAG=v1.2.11"), "make", "package-release")
	runRepoCommandMustFail(t, root, []string{
		"VERSION=1.2.12",
		"RELEASE_TAG=v1.2.12",
		"RELEASE_SHA=0000000000000000000000000000000000000000",
		"RELEASE_READY_URL=https://github.com/viewlegacy/onprest/actions/runs/123",
		"RELEASE_DIR=" + t.TempDir(),
	}, "make", "package-release")
}

func TestNormalizeReleaseVersionRejectsShellMetacharacters(t *testing.T) {
	root := repoRoot(t)
	if got := strings.TrimSpace(runRepoCommand(t, root, nil, "bash", "scripts/normalize_release_version.sh", "v1.2.12")); got != "1.2.12" {
		t.Fatalf("normalized version=%q", got)
	}
	marker := filepath.Join(t.TempDir(), "must-not-exist")
	for _, invalid := range []string{"1.2", "v1.2.3.4", "1.2.3' ; touch " + marker + " ; #", "$(touch " + marker + ")"} {
		runRepoCommandMustFail(t, root, nil, "bash", "scripts/normalize_release_version.sh", invalid)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("invalid version executed shell content: %s", marker)
	}
}

func releaseArchiveEntries(t *testing.T, path string) map[string]bool {
	t.Helper()
	entries := map[string]bool{}
	if strings.HasSuffix(path, ".zip") {
		r, err := zip.OpenReader(path)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		for _, file := range r.File {
			entries[file.Name] = true
		}
		return entries
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		entries[header.Name] = true
	}
	return entries
}

func releaseArchiveFile(t *testing.T, archivePath, entryName string) []byte {
	t.Helper()
	if strings.HasSuffix(archivePath, ".zip") {
		r, err := zip.OpenReader(archivePath)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		for _, file := range r.File {
			if file.Name != entryName {
				continue
			}
			reader, err := file.Open()
			if err != nil {
				t.Fatal(err)
			}
			body, readErr := io.ReadAll(reader)
			closeErr := reader.Close()
			if readErr != nil || closeErr != nil {
				t.Fatalf("read %s: read=%v close=%v", entryName, readErr, closeErr)
			}
			return body
		}
		t.Fatalf("archive entry missing: %s", entryName)
	}
	f, err := os.Open(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == entryName {
			body, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			return body
		}
	}
	t.Fatalf("archive entry missing: %s", entryName)
	return nil
}

func fakeVulnerabilityEvidence(version, sha string) string {
	var body strings.Builder
	fmt.Fprintf(&body, "Onprest release vulnerability scans\nversion=%s\ncommit_sha=%s\n\n## source ./...\nNo vulnerabilities found.\n", version, sha)
	for _, target := range []string{"linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64", "windows-amd64"} {
		ext := ""
		if strings.HasPrefix(target, "windows-") {
			ext = ".exe"
		}
		for _, binary := range []string{"gateway", "agent"} {
			fmt.Fprintf(&body, "\n## %s/onprest-%s%s\nNo vulnerabilities found.\n", target, binary, ext)
		}
	}
	return body.String()
}

func cloneReleaseAssets(t *testing.T, source string) string {
	t.Helper()
	destination := t.TempDir()
	entries, err := os.ReadDir(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		from := filepath.Join(source, entry.Name())
		to := filepath.Join(destination, entry.Name())
		if err := os.Link(from, to); err == nil {
			continue
		}
		input, err := os.Open(from)
		if err != nil {
			t.Fatal(err)
		}
		output, err := os.Create(to)
		if err != nil {
			_ = input.Close()
			t.Fatal(err)
		}
		_, copyErr := io.Copy(output, input)
		closeInputErr := input.Close()
		closeOutputErr := output.Close()
		if copyErr != nil || closeInputErr != nil || closeOutputErr != nil {
			t.Fatalf("copy %s: copy=%v input-close=%v output-close=%v", entry.Name(), copyErr, closeInputErr, closeOutputErr)
		}
	}
	return destination
}

func replaceEnv(env []string, key, value string) []string {
	prefix := key + "="
	result := append([]string(nil), env...)
	for i, item := range result {
		if strings.HasPrefix(item, prefix) {
			result[i] = prefix + value
			return result
		}
	}
	return append(result, prefix+value)
}

func mutateTarArchive(t *testing.T, archivePath, kind, injected string) {
	t.Helper()
	input, err := os.Open(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(input)
	if err != nil {
		_ = input.Close()
		t.Fatal(err)
	}
	temporary := archivePath + ".mutated"
	output, err := os.Create(temporary)
	if err != nil {
		_ = gz.Close()
		_ = input.Close()
		t.Fatal(err)
	}
	zw := gzip.NewWriter(output)
	tw := tar.NewWriter(zw)
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if kind == "missing" && strings.HasSuffix(header.Name, "/INSTALL.md") {
			continue
		}
		if kind == "quickstart-missing" && strings.HasSuffix(header.Name, "/postgres-init.sql") {
			continue
		}
		if kind == "symlink" && strings.HasSuffix(header.Name, "/INSTALL.md") {
			header.Typeflag = tar.TypeSymlink
			header.Linkname = "../outside"
			header.Size = 0
			body = nil
		}
		if kind == "hardlink" && strings.HasSuffix(header.Name, "/INSTALL.md") {
			header.Typeflag = tar.TypeLink
			header.Linkname = "onprest-1.2.12-linux-amd64/LICENSE"
			header.Size = 0
			body = nil
		}
		if kind == "manifest" && strings.HasSuffix(header.Name, "/RELEASE-MANIFEST.txt") {
			body = bytes.Replace(body, []byte("version=1.2.12"), []byte("version=9.9.9"), 1)
			header.Size = int64(len(body))
		}
		if kind == "mode" && strings.HasSuffix(header.Name, "/onprest-gateway") {
			header.Mode = 0o644
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if len(body) > 0 {
			if _, err := tw.Write(body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if kind == "relative" || kind == "absolute" || kind == "duplicate" || kind == "extra" || kind == "quickstart-extra" {
		body := []byte("must not be extracted")
		name := injected
		switch kind {
		case "duplicate":
			name = "onprest-1.2.12-linux-amd64/LICENSE"
		case "extra":
			name = "onprest-1.2.12-linux-amd64/EXTRA"
		case "quickstart-extra":
			name = "onprest-1.2.12-quickstart/EXTRA"
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	for _, closeFn := range []func() error{tw.Close, zw.Close, output.Close, gz.Close, input.Close} {
		if err := closeFn(); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Rename(temporary, archivePath); err != nil {
		t.Fatal(err)
	}
}

func mutateZipArchive(t *testing.T, archivePath, kind, injected string) {
	t.Helper()
	input, err := zip.OpenReader(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	temporary := archivePath + ".mutated"
	output, err := os.Create(temporary)
	if err != nil {
		_ = input.Close()
		t.Fatal(err)
	}
	zw := zip.NewWriter(output)
	for _, file := range input.File {
		header := file.FileHeader
		if kind == "symlink" && strings.HasSuffix(header.Name, "/INSTALL.md") {
			header.SetMode(os.ModeSymlink | 0o777)
		}
		if kind == "mode" && strings.HasSuffix(header.Name, "/INSTALL.md") {
			header.SetMode(os.ModeDevice | 0o644)
		}
		writer, err := zw.CreateHeader(&header)
		if err != nil {
			t.Fatal(err)
		}
		if file.FileInfo().IsDir() {
			continue
		}
		reader, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(writer, reader); err != nil {
			_ = reader.Close()
			t.Fatal(err)
		}
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
	}
	name := injected
	switch kind {
	case "duplicate":
		name = "onprest-1.2.12-windows-amd64/LICENSE"
	case "extra":
		name = "onprest-1.2.12-windows-amd64/EXTRA"
	case "symlink":
		name = ""
	}
	if name != "" {
		header := &zip.FileHeader{Name: name, Method: zip.Store}
		header.SetMode(0o644)
		writer, err := zw.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte("must not be extracted")); err != nil {
			t.Fatal(err)
		}
	}
	for _, closeFn := range []func() error{zw.Close, output.Close, input.Close} {
		if err := closeFn(); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Rename(temporary, archivePath); err != nil {
		t.Fatal(err)
	}
}

func runRepoCommand(t *testing.T, dir string, extraEnv []string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), extraEnv...)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, output.String())
	}
	return output.String()
}

func runRepoCommandMustFail(t *testing.T, dir string, extraEnv []string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), extraEnv...)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err == nil {
		t.Fatalf("%s %s unexpectedly succeeded\n%s", name, strings.Join(args, " "), output.String())
	}
	return output.String()
}

func entryNames(entries []os.DirEntry) []string {
	names := make([]string, len(entries))
	for i, entry := range entries {
		names[i] = fmt.Sprintf("%s (%s)", entry.Name(), entry.Type())
	}
	return names
}

func TestDockerfileBuildsSelectableSingleBinaryTargets(t *testing.T) {
	root := repoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	dockerfile := string(b)
	for _, want := range []string{
		"ARG TARGET=gateway",
		"ARG VERSION=dev",
		"go build -buildvcs=false -trimpath -ldflags=\"-X github.com/viewlegacy/onprest/internal/buildinfo.Version=${VERSION} -X github.com/viewlegacy/onprest/internal/buildinfo.ReleaseMarker=onprest-release-version:${VERSION}\" -o /out/onprest ./cmd/${TARGET}",
		"ENTRYPOINT [\"/app/onprest\"]",
	} {
		if !strings.Contains(dockerfile, want) {
			t.Fatalf("Dockerfile missing %q", want)
		}
	}
}

func TestExamplesIncludeCurrentPublicConfigurationFields(t *testing.T) {
	root := repoRoot(t)
	gatewayEnv := readText(t, filepath.Join(root, "examples", "gateway.env"))
	for _, want := range []string{
		"GATEWAY_ADDR=:8080",
		"GATEWAY_AGENT_PUBLIC_KEY=",
		"GATEWAY_API_KEYS_JSON=",
		"GATEWAY_RATE_LIMIT_REQUESTS_PER_SECOND=10",
		"GATEWAY_RATE_LIMIT_BURST=20",
	} {
		if !strings.Contains(gatewayEnv, want) {
			t.Fatalf("examples/gateway.env missing %q", want)
		}
	}
	if !strings.Contains(gatewayEnv, `GATEWAY_API_KEYS_JSON='[`) {
		t.Fatalf("examples/gateway.env must single-quote GATEWAY_API_KEYS_JSON to preserve bcrypt dollar signs")
	}
	if !strings.Contains(gatewayEnv, `"capabilities":["get_customer","update_customer"]`) || strings.Contains(gatewayEnv, `"capabilities":["*"]`) {
		t.Fatal("examples/gateway.env must use the explicit Quick Start capability allow-list")
	}

	capabilityYAML := readText(t, filepath.Join(root, "examples", "capability.postgres.yaml"))
	for _, want := range []string{
		"driver: postgres",
		"name: legacy",
		"user: capability_user",
		"password: onprest-example-password",
		"sql: select id, name, email from customers where id = :customer_id",
		"sql: update customers set name = :name where id = :customer_id",
		"max_size: 10MB",
		"max_files: 3",
		"readonly: true",
		"expose_in_openapi: true",
	} {
		if !strings.Contains(capabilityYAML, want) {
			t.Fatalf("examples/capability.postgres.yaml missing %q", want)
		}
	}

	composeYAML := readText(t, filepath.Join(root, "examples", "postgres.compose.yml"))
	for _, want := range []string{
		"POSTGRES_DB: legacy",
		"POSTGRES_USER: onprest_admin",
		"POSTGRES_PASSWORD: onprest-example-password",
		`"127.0.0.1:5432:5432"`,
		"./postgres-init.sql:/docker-entrypoint-initdb.d/001-onprest-example.sql:ro",
	} {
		if !strings.Contains(composeYAML, want) {
			t.Fatalf("examples/postgres.compose.yml missing %q", want)
		}
	}

	initSQL := readText(t, filepath.Join(root, "examples", "postgres-init.sql"))
	for _, want := range []string{
		"CREATE ROLE capability_user LOGIN PASSWORD 'onprest-example-password'",
		"CREATE TABLE customers",
		"(1, 'Ada Lovelace', 'ada@example.com')",
		"GRANT SELECT ON customers TO capability_user",
		"GRANT UPDATE (name) ON customers TO capability_user",
	} {
		if !strings.Contains(initSQL, want) {
			t.Fatalf("examples/postgres-init.sql missing %q", want)
		}
	}
}

func TestRepositoryDoesNotAddCaddyImplementationDependency(t *testing.T) {
	root := repoRoot(t)
	allowed := map[string]bool{
		filepath.Join(root, "architecture.md"):               true,
		filepath.Join(root, "architecture-test-plan.md"):     true,
		filepath.Join(root, "README.md"):                     true,
		filepath.Join(root, "AGENTS.md"):                     true,
		filepath.Join(root, "internal/project/repo_test.go"): true,
	}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "dist", "docs", "it", "local":
				return filepath.SkipDir
			}
			return nil
		}
		if allowed[path] {
			return nil
		}
		name := strings.ToLower(d.Name())
		if strings.Contains(name, "caddy") {
			t.Fatalf("Caddy-specific implementation artifact found: %s", path)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(strings.ToLower(string(b)), "caddy") {
			t.Fatalf("Caddy-specific implementation reference found in %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestGitHubActionsSeparateFastAndMainReleaseChecks(t *testing.T) {
	root := repoRoot(t)
	workflowDir := filepath.Join(root, ".github", "workflows")

	ci := readWorkflow(t, filepath.Join(workflowDir, "ci.yml"))
	ciEvents := workflowSection(t, ci, "on")
	for _, event := range []string{"push", "pull_request"} {
		if _, ok := ciEvents[event]; !ok {
			t.Fatalf("ci workflow missing %s trigger", event)
		}
	}
	ciText := readText(t, filepath.Join(workflowDir, "ci.yml"))
	for _, command := range []string{"go test ./...", "go vet ./..."} {
		if !strings.Contains(ciText, command) {
			t.Fatalf("ci workflow missing %q", command)
		}
	}

	release := readWorkflow(t, filepath.Join(workflowDir, "release-gate.yml"))
	assertMainWorkflowTriggers(t, release)
	releaseJobs := workflowSection(t, release, "jobs")
	for _, job := range []string{"package-candidate", "integration-linux", "service-lifecycle", "release-ready"} {
		if _, ok := releaseJobs[job]; !ok {
			t.Fatalf("release gate workflow missing %s job", job)
		}
	}
	if text := readText(t, filepath.Join(workflowDir, "release-gate.yml")); !strings.Contains(text, "make test-it-release-gate") ||
		!strings.Contains(text, "make release-artifacts") ||
		!strings.Contains(text, "release-candidate-${{ github.sha }}") ||
		!strings.Contains(text, "uses: ./.github/workflows/service-lifecycle.yml") ||
		!strings.Contains(text, "PACKAGE_RESULT") || !strings.Contains(text, "SERVICE_RESULT") || !strings.Contains(text, "concurrency:") || !strings.Contains(text, "cancel-in-progress: true") {
		t.Fatal("release gate workflow does not aggregate Linux integration and reusable service lifecycle")
	}
	releaseScript := readText(t, filepath.Join(root, "scripts", "it_release_gate.sh"))
	for _, marker := range []string{"ONPREST_IT_GATEWAY_BINARY", "ONPREST_IT_AGENT_BINARY", "ONPREST_IT_DISTRIBUTION_VERSION", "must be extracted outside the source tree", `cd / && "$absolute" --version`, "scripts/quickstart_smoke.sh"} {
		if !strings.Contains(releaseScript, marker) {
			t.Fatalf("release gate script does not validate prebuilt archive binaries: missing %q", marker)
		}
	}

	service := readWorkflow(t, filepath.Join(workflowDir, "service-lifecycle.yml"))
	serviceEvents := workflowSection(t, service, "on")
	if _, ok := serviceEvents["workflow_call"]; !ok {
		t.Fatal("service lifecycle workflow missing workflow_call trigger")
	}
	for _, event := range []string{"push", "pull_request", "workflow_dispatch"} {
		if _, ok := serviceEvents[event]; ok {
			t.Fatalf("service lifecycle workflow must be triggered through release-gate, found direct %s", event)
		}
	}
	serviceJobs := workflowSection(t, service, "jobs")
	for _, job := range []string{"linux-systemd", "macos-launchd", "windows-service"} {
		definition, ok := serviceJobs[job].(map[string]any)
		if !ok {
			t.Fatalf("service lifecycle workflow missing %s job", job)
		}
		steps, ok := definition["steps"].([]any)
		if !ok {
			t.Fatalf("service lifecycle %s steps missing", job)
		}
		validateAt, downloadAt := -1, -1
		for i, raw := range steps {
			step, _ := raw.(map[string]any)
			if step["name"] == "Validate release version input" {
				validateAt = i
			}
			if step["uses"] == "actions/download-artifact@v5" {
				downloadAt = i
			}
		}
		if validateAt < 0 || downloadAt < 0 || validateAt >= downloadAt {
			t.Fatalf("service lifecycle %s must reject invalid version before artifact download", job)
		}
	}
	serviceText := readText(t, filepath.Join(workflowDir, "service-lifecycle.yml"))
	if strings.Count(serviceText, "${{ inputs.version }}") != 3 || strings.Contains(serviceText, "version=${{ inputs.version }}") {
		t.Fatal("service lifecycle version input must enter scripts only through quoted environment values")
	}
	for _, marker := range []string{"normalize_release_version.sh \"$INPUT_VERSION\"", "-notmatch '^[0-9]+\\.[0-9]+\\.[0-9]+$'", "docker exec --env \"PACKAGE_VERSION=$PACKAGE_VERSION\""} {
		if !strings.Contains(serviceText, marker) {
			t.Fatalf("service lifecycle version validation missing %q", marker)
		}
	}
	for _, workflow := range []string{"release-gate.yml", "release.yml", "service-lifecycle.yml"} {
		text := readText(t, filepath.Join(workflowDir, workflow))
		for _, forbidden := range []string{"tar -x", "unzip ", "Expand-Archive"} {
			if strings.Contains(text, forbidden) {
				t.Fatalf("%s contains raw archive extraction %q", workflow, forbidden)
			}
		}
		if !strings.Contains(text, "go run ./internal/releaseverify extract") {
			t.Fatalf("%s does not use the common safe archive extractor", workflow)
		}
	}
	scanScript := readText(t, filepath.Join(root, "scripts", "scan_release_binaries.sh"))
	for _, forbidden := range []string{"tar -x", "unzip "} {
		if strings.Contains(scanScript, forbidden) {
			t.Fatalf("binary scan contains raw archive extraction %q", forbidden)
		}
	}
	if !strings.Contains(scanScript, "go run ./internal/releaseverify extract") || !strings.Contains(scanScript, ".archive-digests") {
		t.Fatal("binary scan does not verify the package-time archive digest through the safe extractor")
	}
	if strings.Contains(serviceText, "concurrency:") {
		t.Fatal("called service lifecycle workflow must not share caller concurrency and cancel its own release gate")
	}
	if strings.Contains(serviceText, "\n    paths:") {
		t.Fatal("service lifecycle must run unconditionally for main PRs and pushes")
	}
	if !strings.Contains(serviceText, "scripts/service-test-systemd.Dockerfile") {
		t.Fatal("linux service lifecycle does not build the systemd test image")
	}
	if strings.Contains(serviceText, "choco install postgresql") {
		t.Fatal("Windows service lifecycle reinstalls PostgreSQL instead of using the runner-provided service")
	}
	for _, marker := range []string{"Start preinstalled PostgreSQL", "$env:PGBIN", "Get-Service -Name \"postgresql-x64-$postgresVersion\"", "Set-Service -Name $postgresService.Name -StartupType Manual", "Start-Service -Name $postgresService.Name", "ALTER USER postgres PASSWORD 'onprest'"} {
		if !strings.Contains(serviceText, marker) {
			t.Fatalf("Windows service lifecycle does not configure the runner-provided PostgreSQL service: missing %q", marker)
		}
	}
	if strings.Count(serviceText, "TestValidateLatestLogCrashRecoveryProcess") != 3 {
		t.Fatal("service lifecycle must run validate crash recovery on Linux, macOS, and Windows")
	}
	for _, testName := range []string{
		"TestValidationRecoveryPreservesUnknownAndSpecialTemporaryPaths",
		"TestValidationRejectsInvalidFixedPathsWithoutMutation",
	} {
		if strings.Count(serviceText, testName) != 3 {
			t.Fatalf("service lifecycle must run %s on Linux, macOS, and Windows", testName)
		}
	}
	for path, required := range map[string][]string{
		"scripts/test_service_lifecycle_unix.sh":     {"gateway_bin", "kill -0 \"$gateway_pid\"", "runtime_marker_a", "runtime_marker_b", "rollout_marker_new", "assert_old_public_contract", "assert_new_capability_absent", "MCP-Protocol-Version: 2025-11-25", "Accept: application/json, text/event-stream", "runtime_writer_pid", "capability.validate-blocking.yaml", "cleanup\ntrap - EXIT"},
		"scripts/test_service_lifecycle_windows.ps1": {"GatewayBin", "gatewayProcess.WaitForExit()", "SetEnvironmentVariable('GATEWAY_API_KEYS_JSON'", "runtime_marker_a", "runtime_marker_b", "rollout_marker_new", "Assert-OldPublicContract", "Assert-NewCapabilityAbsent", "MCP-Protocol-Version'='2025-11-25", "Accept='application/json, text/event-stream", "runtimeWriter", "[Guid]::NewGuid().ToString('N').Substring(0, 10)", "$readerCreated", "$primaryFailure", "$cleanupFailure", "temporaryLog"},
	} {
		text := readText(t, filepath.Join(root, filepath.FromSlash(path)))
		for _, marker := range required {
			if !strings.Contains(text, marker) {
				t.Fatalf("%s does not exercise production runtime rotation/validation isolation marker %q", path, marker)
			}
		}
	}
	unixLifecycle := readText(t, filepath.Join(root, "scripts", "test_service_lifecycle_unix.sh"))
	windowsLifecycle := readText(t, filepath.Join(root, "scripts", "test_service_lifecycle_windows.ps1"))
	if strings.Contains(windowsLifecycle, "OnprestValidateReader") {
		t.Fatal("Windows lifecycle uses a local account name longer than the Windows 20-character limit")
	}
	if strings.Contains(windowsLifecycle, "OnprestVal$PID") ||
		!strings.Contains(windowsLifecycle, "if ($null -ne $cleanupFailure)") ||
		!strings.Contains(windowsLifecycle, "throw $cleanupFailure") {
		t.Fatal("Windows lifecycle does not use a collision-resistant account name and fail a successful run on final account cleanup failure")
	}
	readerPasswordMatch := regexp.MustCompile(`\$readerPasswordPlain = '([^']+)'`).FindStringSubmatch(windowsLifecycle)
	if len(readerPasswordMatch) != 2 || len(readerPasswordMatch[1]) > 14 {
		t.Fatal("Windows lifecycle reader password must not trigger net.exe's interactive legacy-compatibility prompt")
	}
	readerPassword := readerPasswordMatch[1]
	for class, pattern := range map[string]string{
		"uppercase": `[A-Z]`,
		"lowercase": `[a-z]`,
		"digit":     `[0-9]`,
		"symbol":    `[^A-Za-z0-9]`,
	} {
		if !regexp.MustCompile(pattern).MatchString(readerPassword) {
			t.Fatalf("Windows lifecycle reader password does not satisfy the %s complexity class", class)
		}
	}
	for path, text := range map[string]string{
		"Unix lifecycle":    unixLifecycle,
		"Windows lifecycle": windowsLifecycle,
	} {
		if strings.Contains(text, "onprest_validate_missing") {
			t.Fatalf("%s uses a redacted password substring in its missing-database marker", path)
		}
		for _, marker := range []string{"validate_missing_database_a", "validate_missing_database_b"} {
			if !strings.Contains(text, marker) {
				t.Fatalf("%s does not assert unredacted missing-database marker %q", path, marker)
			}
		}
	}
	for path, lifecycle := range map[string]struct {
		text     string
		settings []string
	}{
		"Unix lifecycle": {
			text: unixLifecycle,
			settings: []string{
				"GATEWAY_RATE_LIMIT_REQUESTS_PER_SECOND=1000",
				"GATEWAY_RATE_LIMIT_BURST=1000",
			},
		},
		"Windows lifecycle": {
			text: windowsLifecycle,
			settings: []string{
				"$env:GATEWAY_RATE_LIMIT_REQUESTS_PER_SECOND = '1000'",
				"$env:GATEWAY_RATE_LIMIT_BURST = '1000'",
			},
		},
	} {
		for _, setting := range lifecycle.settings {
			if !strings.Contains(lifecycle.text, setting) {
				t.Fatalf("%s does not set high test-only rate limit %q", path, setting)
			}
		}
	}
	if strings.Contains(unixLifecycle, "stat -f '%Lp' \"$fixed_log\" 2>/dev/null || stat -c") ||
		!strings.Contains(unixLifecycle, "Darwin) stat -f") || !strings.Contains(unixLifecycle, "Linux) stat -c") {
		t.Fatal("Unix lifecycle does not select permission/size stat syntax by operating system")
	}
	if strings.Contains(windowsLifecycle, "Get-FileHash") {
		t.Fatal("Windows lifecycle hashes a live Agent log without write/delete sharing")
	}
	for _, marker := range []string{
		"function Get-SharedFileHash",
		"[System.IO.FileShare]::ReadWrite -bor [System.IO.FileShare]::Delete",
		"$hasher.ComputeHash($stream)",
		"$runtimeBefore = @((Get-SharedFileHash $runtimeLog), (Get-SharedFileHash $rotatedLog))",
		"$runtimeAfter = @((Get-SharedFileHash $runtimeLog), (Get-SharedFileHash $rotatedLog))",
	} {
		if !strings.Contains(windowsLifecycle, marker) {
			t.Fatalf("Windows lifecycle does not hash live Agent logs with sharing-compatible helper %q", marker)
		}
	}
	trapAt := strings.Index(unixLifecycle, "trap cleanup EXIT")
	installAt := strings.Index(unixLifecycle, `"${elevate[@]}" "$agent_bin" service install`)
	if trapAt < 0 || installAt <= trapAt || strings.Contains(unixLifecycle[trapAt:installAt], "\ncleanup\n") {
		t.Fatal("Unix lifecycle invokes final cleanup after starting the Gateway but before installing the service")
	}
	for _, invocation := range []string{
		"Extract Linux release archive outside the repository",
		"onprest-$PACKAGE_VERSION-linux-amd64.tar.gz",
		"onprest-$PACKAGE_VERSION-$target.tar.gz",
		"onprest-$($env:PACKAGE_VERSION)-windows-amd64.zip",
		"run_service_lifecycle_sanitized_unix.sh",
		"development tool remains on service test PATH",
		"LINUX_PACKAGE_ROOT",
		"$env:RUNNER_TEMP",
	} {
		if !strings.Contains(serviceText, invocation) {
			t.Fatalf("service lifecycle workflow does not use the source-free archive path: %q", invocation)
		}
	}
	systemdImage := readText(t, filepath.Join(root, "scripts", "service-test-systemd.Dockerfile"))
	for _, want := range []string{"systemd-sysv", "postgresql", `CMD ["/sbin/init"]`} {
		if !strings.Contains(systemdImage, want) {
			t.Fatalf("systemd service test image missing %q", want)
		}
	}
}

func TestTagReleaseWorkflowRequiresExactSuccessfulGateAndLeastPrivilege(t *testing.T) {
	root := repoRoot(t)
	path := filepath.Join(root, ".github", "workflows", "release.yml")
	workflow := readWorkflow(t, path)
	basePermissions := workflowSection(t, workflow, "permissions")
	if basePermissions["contents"] != "read" || basePermissions["actions"] != "read" || len(basePermissions) != 2 {
		t.Fatalf("release base permissions are not read-only: %#v", basePermissions)
	}
	events := workflowSection(t, workflow, "on")
	push, ok := events["push"].(map[string]any)
	if !ok {
		t.Fatalf("release workflow push trigger missing: %#v", events["push"])
	}
	tags, ok := push["tags"].([]any)
	if !ok || len(tags) != 1 || tags[0] != "v*.*.*" {
		t.Fatalf("release tags=%#v", push["tags"])
	}
	for _, forbidden := range []string{"pull_request", "workflow_dispatch"} {
		if _, exists := events[forbidden]; exists {
			t.Fatalf("release workflow unexpectedly has %s trigger", forbidden)
		}
	}
	jobs := workflowSection(t, workflow, "jobs")
	for _, job := range []string{"verify_release_ready", "build", "release_archive_integration", "release_archive_service_lifecycle", "attest", "publish"} {
		if _, ok := jobs[job]; !ok {
			t.Fatalf("release workflow missing %s", job)
		}
	}
	attest := jobs["attest"].(map[string]any)
	attestPermissions := attest["permissions"].(map[string]any)
	for key, want := range map[string]any{"contents": "read", "actions": "read", "id-token": "write", "attestations": "write", "artifact-metadata": "write"} {
		if attestPermissions[key] != want {
			t.Fatalf("attest permission %s=%#v, want %q", key, attestPermissions[key], want)
		}
	}
	if len(attestPermissions) != 5 {
		t.Fatalf("attest permissions include unexpected grants: %#v", attestPermissions)
	}
	publish := jobs["publish"].(map[string]any)
	publishPermissions := publish["permissions"].(map[string]any)
	if publishPermissions["contents"] != "write" || publishPermissions["actions"] != "read" || len(publishPermissions) != 2 {
		t.Fatalf("publish permissions are not limited to release content plus artifact read: %#v", publishPermissions)
	}
	text := readText(t, path)
	for _, required := range []string{
		"head_sha=$GITHUB_SHA",
		"scripts/select_release_ready_run.rb",
		"test \"$(git rev-parse \"$GITHUB_REF_NAME^{commit}\")\" = \"$GITHUB_SHA\"",
		"make release-artifacts",
		`test "$(cd / && "$install_dir/onprest-gateway" --version)" = "$VERSION"`,
		`test "$(cd / && "$install_dir/onprest-agent" --version)" = "$VERSION"`,
		`bash scripts/quickstart_smoke.sh "$install_dir"`,
		"TestDistributionBinariesDirectAndGenericProxyHTTPAndWebSocket",
		"TestOperationalAgentSecretRotationWithRealBinaries",
		"uses: ./.github/workflows/service-lifecycle.yml",
		"artifact-name: release-assets-${{ github.sha }}",
		"actions/attest@v4",
		"subject-path: release-dist/*",
		"artifact-metadata: write",
		"id-token: write",
		"attestations: write",
		"contents: write",
		"refusing to overwrite it",
		"sha256sum --check SHA256SUMS",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("release workflow missing %q", required)
		}
	}
	integrationSource := readText(t, filepath.Join(root, "it", "gateway_process_operational_test.go"))
	prebuiltSource := readText(t, filepath.Join(root, "it", "gateway_agent_postgres_test.go"))
	for _, marker := range []string{"PATH=\" + runtimePath", "startReverseProxy", "renderCapability", "assertRESTCapability", "assertMCPTools"} {
		if !strings.Contains(integrationSource, marker) {
			t.Fatalf("distribution direct/proxy E2E missing %q", marker)
		}
	}
	for _, marker := range []string{"ONPREST_IT_GATEWAY_BINARY", "ONPREST_IT_AGENT_BINARY", "copy prebuilt"} {
		if !strings.Contains(prebuiltSource, marker) {
			t.Fatalf("distribution E2E cannot consume packaged binary: missing %q", marker)
		}
	}
	for _, forbidden := range []string{"pull_request:", "actions/attest-build-provenance", "cancel-in-progress: true"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("release workflow contains forbidden %q", forbidden)
		}
	}
}

func TestReleaseReadySelectionRejectsWrongSHAAndUnsuccessfulRuns(t *testing.T) {
	root := repoRoot(t)
	tmp := t.TempDir()
	jobs := filepath.Join(tmp, "jobs")
	if err := os.Mkdir(jobs, 0o755); err != nil {
		t.Fatal(err)
	}
	exactSHA := "1111111111111111111111111111111111111111"
	otherSHA := "2222222222222222222222222222222222222222"
	runs := `{"workflow_runs":[` +
		`{"id":1,"html_url":"https://github.com/viewlegacy/onprest/actions/runs/1","head_sha":"` + otherSHA + `","status":"completed","conclusion":"success"},` +
		`{"id":2,"html_url":"https://github.com/viewlegacy/onprest/actions/runs/2","head_sha":"` + exactSHA + `","status":"completed","conclusion":"failure"},` +
		`{"id":3,"html_url":"https://github.com/viewlegacy/onprest/actions/runs/3","head_sha":"` + exactSHA + `","status":"completed","conclusion":"cancelled"},` +
		`{"id":4,"html_url":"https://github.com/viewlegacy/onprest/actions/runs/4","head_sha":"` + exactSHA + `","status":"completed","conclusion":"success"}` +
		`]}`
	runsPath := filepath.Join(tmp, "runs.json")
	if err := os.WriteFile(runsPath, []byte(runs), 0o644); err != nil {
		t.Fatal(err)
	}
	for id, conclusion := range map[string]string{"1": "success", "2": "success", "3": "success", "4": "failure"} {
		body := `{"jobs":[{"name":"release-ready","conclusion":"` + conclusion + `"}]}`
		if err := os.WriteFile(filepath.Join(jobs, id+".json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runRepoCommandMustFail(t, root, nil, "ruby", "scripts/select_release_ready_run.rb", exactSHA, runsPath, jobs)

	if err := os.WriteFile(filepath.Join(jobs, "4.json"), []byte(`{"jobs":[{"name":"release-ready","conclusion":"success"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(runRepoCommand(t, root, nil, "ruby", "scripts/select_release_ready_run.rb", exactSHA, runsPath, jobs))
	if got != "4\thttps://github.com/viewlegacy/onprest/actions/runs/4" {
		t.Fatalf("selected release-ready run = %q", got)
	}
}

func TestDatabaseGateDocumentationMatchesExecutableSelection(t *testing.T) {
	root := repoRoot(t)
	makefile := readText(t, filepath.Join(root, "Makefile"))
	releaseScript := readText(t, filepath.Join(root, "scripts", "it_release_gate.sh"))
	integrationReadme := readText(t, filepath.Join(root, "it", "README.md"))
	testCommands := readText(t, filepath.Join(root, "docs", "app", "reference", "test-commands", "page.mdx"))
	releaseDocs := readText(t, filepath.Join(root, "docs", "app", "operations", "release-gate", "page.mdx"))

	const allDBSelector = "-run '^TestContainerDBDriver'"
	for source, text := range map[string]string{
		"Makefile": makefile, "release script": releaseScript, "integration README": integrationReadme, "test commands": testCommands,
	} {
		if !strings.Contains(text, allDBSelector) {
			t.Fatalf("%s does not use common all-DB selection %q", source, allDBSelector)
		}
	}
	for path, name := range map[string]string{
		"it/transaction_start_conformance_test.go": "TestContainerDBDriverOracleTransactionStartIsImmediateAndRollbackable",
		"it/openapi_nullable_postgres_test.go":     "TestContainerDBDriverPostgresNullableResultMatchesGeneratedOpenAPI",
		"it/timestamp_postgres_test.go":            "TestContainerDBDriverPostgresTimestampResultPreservesJSONContract",
	} {
		if text := readText(t, filepath.Join(root, filepath.FromSlash(path))); !strings.Contains(text, "func "+name+"(") {
			t.Fatalf("%s does not use common all-DB test prefix: %s", path, name)
		}
	}
	for _, tlsContract := range []string{
		"TestPostgresTLSModesPrivateCAClientCertificateAndHostnameVerification",
		"TestMySQLTLSModesClientCertificateRotationAndReconnectAgainstRealDatabase",
		"TestSQLServerTLSRequireAndVerifyFullAgainstRealDatabase",
		"TestOracleTLSModesVerificationRotationAndReconnectAgainstRealDatabase",
	} {
		for source, text := range map[string]string{
			"Makefile": makefile, "release script": releaseScript, "integration README": integrationReadme,
		} {
			if !strings.Contains(text, tlsContract) {
				t.Fatalf("%s does not include database TLS contract %q", source, tlsContract)
			}
		}
	}
	for _, database := range []string{"PostgreSQL", "MySQL", "SQL Server", "Oracle"} {
		if !strings.Contains(testCommands, database) {
			t.Fatalf("test commands do not document the %s TLS contract", database)
		}
	}
	for source, text := range map[string]string{"test commands": testCommands, "release gate": releaseDocs} {
		for _, coverage := range []string{"private", "hostname", "client"} {
			if !strings.Contains(text, coverage) {
				t.Fatalf("%s does not document PostgreSQL TLS %s coverage", source, coverage)
			}
		}
	}
	if strings.Contains(testCommands, "exact filter `^TestContainerDBDriver`") || strings.Contains(releaseDocs, "all-DB smoke path") {
		t.Fatal("DB gate documentation retained the superseded selection")
	}
}

func TestMySQLTLSAdapterAvoidsProcessGlobalRegistry(t *testing.T) {
	root := repoRoot(t)
	agentDir := filepath.Join(root, "internal", "agent")
	err := filepath.WalkDir(agentDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		content := readText(t, path)
		for _, forbidden := range []string{"RegisterTLSConfig", "DeregisterTLSConfig"} {
			if strings.Contains(content, forbidden) {
				t.Fatalf("production source %s uses process-global MySQL TLS registry API %s", path, forbidden)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	databaseAdapter := readText(t, filepath.Join(agentDir, "database.go"))
	for _, required := range []string{"config.TLS = tlsConfig", "mysql.NewConnector(config)", "sql.OpenDB(connector)"} {
		if !strings.Contains(databaseAdapter, required) {
			t.Fatalf("MySQL adapter does not enforce connector-local TLS construction %q", required)
		}
	}
	if strings.Contains(databaseAdapter, "config.TLSConfig =") {
		t.Fatal("MySQL runtime adapter uses a named DSN TLS configuration instead of connector-local tls.Config")
	}
}

func TestPublicOSSBoundaryDocsDoNotDefineManagedOperatingPolicy(t *testing.T) {
	root := repoRoot(t)
	paths := []string{
		filepath.Join(root, "README.md"),
		filepath.Join(root, "docs", "app", "operations", "deployment", "page.mdx"),
		filepath.Join(root, "docs", "app", "architecture", "page.mdx"),
		filepath.Join(root, "docs", "app", "security", "page.mdx"),
	}
	for _, path := range paths {
		content := strings.ToLower(readText(t, path))
		for _, forbidden := range []string{
			"monitor agent connectivity",
			"handle patching",
			"retain operational logs",
			"backend admin ui",
			"managed dashboard is outside the oss core and is read-only",
			"dashboard does not issue api keys",
		} {
			if strings.Contains(content, forbidden) {
				t.Fatalf("%s defines managed-only policy %q", path, forbidden)
			}
		}
	}
}

func readWorkflow(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var workflow map[string]any
	if err := yaml.Unmarshal(b, &workflow); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return workflow
}

func workflowSection(t *testing.T, workflow map[string]any, name string) map[string]any {
	t.Helper()
	section, ok := workflow[name].(map[string]any)
	if !ok {
		t.Fatalf("workflow section %q missing or invalid: %#v", name, workflow[name])
	}
	return section
}

func assertMainWorkflowTriggers(t *testing.T, workflow map[string]any) {
	t.Helper()
	events := workflowSection(t, workflow, "on")
	for _, event := range []string{"pull_request", "push"} {
		config, ok := events[event].(map[string]any)
		if !ok {
			t.Fatalf("workflow %s trigger missing configuration", event)
		}
		branches, ok := config["branches"].([]any)
		if !ok || len(branches) != 1 || branches[0] != "main" {
			t.Fatalf("workflow %s branches = %#v, want [main]", event, config["branches"])
		}
	}
	if _, ok := events["workflow_dispatch"]; !ok {
		t.Fatal("workflow missing workflow_dispatch trigger")
	}
}

func readText(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repo root not found")
		}
		dir = parent
	}
}
