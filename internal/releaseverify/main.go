package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

const maxArchiveFileSize = 256 << 20

var (
	versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	shaPattern     = regexp.MustCompile(`^[0-9a-f]{40}$`)
	runURLPattern  = regexp.MustCompile(`^https://github\.com/viewlegacy/onprest/actions/runs/[0-9]+$`)
)

type config struct {
	dir, version, tag, sha, runURL string
	targets                        []string
}

type archiveEntry struct {
	directory bool
	mode      os.FileMode
	data      []byte
}

func main() {
	args := os.Args[1:]
	var err error
	if len(args) > 0 && args[0] == "extract" {
		err = runExtract(args[1:])
	} else {
		err = run(args)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "release artifact verification:", err)
		os.Exit(1)
	}
}

func runExtract(args []string) error {
	if len(args) != 8 {
		return errors.New("usage: releaseverify extract ARCHIVE OUTPUT VERSION TAG SHA RUN_URL TARGET CHECKSUMS")
	}
	archivePath, output := args[0], args[1]
	cfg := config{dir: filepath.Dir(archivePath), version: args[2], tag: args[3], sha: args[4], runURL: args[5], targets: []string{args[6]}}
	if !versionPattern.MatchString(cfg.version) || cfg.tag != "v"+cfg.version || !shaPattern.MatchString(cfg.sha) || !runURLPattern.MatchString(cfg.runURL) {
		return errors.New("invalid release metadata arguments")
	}
	if err := verifyChecksumSubject(args[7], archivePath); err != nil {
		return err
	}
	entries, root, binaryExt, err := loadAndVerifyArchive(cfg, args[6], archivePath)
	if err != nil {
		return err
	}
	return extractFixedFiles(entries, root, binaryExt, output)
}

func run(args []string) error {
	if len(args) < 6 {
		return errors.New("usage: releaseverify DIR VERSION TAG SHA RUN_URL TARGET...")
	}
	cfg := config{dir: args[0], version: args[1], tag: args[2], sha: args[3], runURL: args[4], targets: args[5:]}
	if !versionPattern.MatchString(cfg.version) || cfg.tag != "v"+cfg.version || !shaPattern.MatchString(cfg.sha) || !runURLPattern.MatchString(cfg.runURL) {
		return errors.New("invalid release metadata arguments")
	}
	if err := verifyTopLevel(cfg); err != nil {
		return err
	}
	if err := verifyTextEvidence(cfg); err != nil {
		return err
	}
	for _, target := range cfg.targets {
		if err := verifyArchive(cfg, target); err != nil {
			return fmt.Errorf("%s: %w", target, err)
		}
	}
	return nil
}

func verifyTopLevel(cfg config) error {
	expected := map[string]bool{
		"DEPENDENCIES.txt":           true,
		"RELEASE-EVIDENCE.txt":       true,
		"VULNERABILITY-EVIDENCE.txt": true,
		"SHA256SUMS":                 true,
	}
	for _, target := range cfg.targets {
		name := strings.ReplaceAll(target, "/", "-")
		ext := ".tar.gz"
		if strings.HasPrefix(name, "windows-") {
			ext = ".zip"
		}
		expected["onprest-"+cfg.version+"-"+name+ext] = true
	}
	entries, err := os.ReadDir(cfg.dir)
	if err != nil {
		return err
	}
	if len(entries) != len(expected) {
		return fmt.Errorf("top-level asset count=%d, want %d", len(entries), len(expected))
	}
	for _, entry := range entries {
		if entry.IsDir() || !expected[entry.Name()] {
			return fmt.Errorf("unexpected top-level entry %q", entry.Name())
		}
	}
	return verifyChecksums(cfg.dir, expected)
}

func verifyChecksums(dir string, expected map[string]bool) error {
	want := map[string]string{}
	f, err := os.Open(filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 || len(fields[0]) != 64 {
			return fmt.Errorf("invalid SHA256SUMS line %q", scanner.Text())
		}
		if _, err := hex.DecodeString(fields[0]); err != nil {
			return fmt.Errorf("invalid checksum for %q", fields[1])
		}
		if fields[1] == "SHA256SUMS" || !expected[fields[1]] || !safeArchiveName(fields[1], false) || want[fields[1]] != "" {
			return fmt.Errorf("unexpected or duplicate checksum subject %q", fields[1])
		}
		want[fields[1]] = fields[0]
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if len(want) != len(expected)-1 {
		return fmt.Errorf("checksum subject count=%d, want %d", len(want), len(expected)-1)
	}
	for name, digest := range want {
		file, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		hasher := sha256.New()
		_, copyErr := io.Copy(hasher, file)
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil {
			return fmt.Errorf("hash %s: copy=%v close=%v", name, copyErr, closeErr)
		}
		if hex.EncodeToString(hasher.Sum(nil)) != digest {
			return fmt.Errorf("checksum mismatch for %s", name)
		}
	}
	return nil
}

func verifyChecksumSubject(checksumPath, archivePath string) error {
	archiveName := filepath.Base(archivePath)
	f, err := os.Open(checksumPath)
	if err != nil {
		return err
	}
	defer f.Close()
	want := ""
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 || fields[1] != archiveName {
			continue
		}
		if want != "" || len(fields[0]) != 64 {
			return fmt.Errorf("duplicate or invalid checksum subject %q", archiveName)
		}
		if _, err := hex.DecodeString(fields[0]); err != nil {
			return fmt.Errorf("invalid checksum for %q", archiveName)
		}
		want = fields[0]
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if want == "" {
		return fmt.Errorf("checksum subject %q is missing", archiveName)
	}
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	hasher := sha256.New()
	_, copyErr := io.Copy(hasher, file)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		return fmt.Errorf("hash %s: copy=%v close=%v", archiveName, copyErr, closeErr)
	}
	if hex.EncodeToString(hasher.Sum(nil)) != want {
		return fmt.Errorf("checksum mismatch for %s", archiveName)
	}
	return nil
}

func verifyTextEvidence(cfg config) error {
	release, err := parseKeyValues(filepath.Join(cfg.dir, "RELEASE-EVIDENCE.txt"), 6)
	if err != nil {
		return err
	}
	wantRelease := map[string]string{
		"release_tag": cfg.tag, "version": cfg.version, "commit_sha": cfg.sha,
		"release_ready_url": cfg.runURL, "release_workflow": "viewlegacy/onprest/.github/workflows/release.yml",
		"release_ref": "refs/tags/" + cfg.tag,
	}
	if err := compareFields("release evidence", release, wantRelease); err != nil {
		return err
	}

	dependencies, err := os.ReadFile(filepath.Join(cfg.dir, "DEPENDENCIES.txt"))
	if err != nil {
		return err
	}
	if !bytes.Contains(dependencies, []byte("release_tag="+cfg.tag+"\n")) || !bytes.Contains(dependencies, []byte("commit_sha="+cfg.sha+"\n")) {
		return errors.New("dependency evidence metadata mismatch")
	}
	wantBinaryHeadings := binaryHeadings(cfg.targets)
	if err := verifyHeadings("dependency evidence", dependencies, wantBinaryHeadings); err != nil {
		return err
	}

	vulnerability, err := os.ReadFile(filepath.Join(cfg.dir, "VULNERABILITY-EVIDENCE.txt"))
	if err != nil {
		return err
	}
	if !bytes.HasPrefix(vulnerability, []byte("Onprest release vulnerability scans\n")) ||
		!bytes.Contains(vulnerability, []byte("version="+cfg.version+"\n")) ||
		!bytes.Contains(vulnerability, []byte("commit_sha="+cfg.sha+"\n")) {
		return errors.New("vulnerability evidence metadata mismatch")
	}
	wantVulnerabilityHeadings := append([]string{"source ./..."}, wantBinaryHeadings...)
	if err := verifyHeadings("vulnerability evidence", vulnerability, wantVulnerabilityHeadings); err != nil {
		return err
	}
	if got := bytes.Count(vulnerability, []byte("No vulnerabilities found.")); got != len(wantVulnerabilityHeadings) {
		return fmt.Errorf("vulnerability evidence successful scan count=%d, want %d", got, len(wantVulnerabilityHeadings))
	}
	return nil
}

func binaryHeadings(targets []string) []string {
	var headings []string
	for _, target := range targets {
		name := strings.ReplaceAll(target, "/", "-")
		ext := ""
		if strings.HasPrefix(name, "windows-") {
			ext = ".exe"
		}
		for _, binary := range []string{"gateway", "agent"} {
			headings = append(headings, name+"/onprest-"+binary+ext)
		}
	}
	return headings
}

func verifyHeadings(label string, body []byte, want []string) error {
	var got []string
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "## ") {
			got = append(got, strings.TrimPrefix(line, "## "))
		}
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		return fmt.Errorf("%s headings=%q, want %q", label, got, want)
	}
	return nil
}

func verifyArchive(cfg config, target string) error {
	targetName := strings.ReplaceAll(target, "/", "-")
	root := "onprest-" + cfg.version + "-" + targetName
	windows := strings.HasPrefix(targetName, "windows-")
	archivePath := filepath.Join(cfg.dir, root+map[bool]string{false: ".tar.gz", true: ".zip"}[windows])
	_, _, _, err := loadAndVerifyArchive(cfg, target, archivePath)
	return err
}

func loadAndVerifyArchive(cfg config, target, archivePath string) (map[string]archiveEntry, string, string, error) {
	targetName := strings.ReplaceAll(target, "/", "-")
	root := "onprest-" + cfg.version + "-" + targetName
	windows := strings.HasPrefix(targetName, "windows-")
	archiveExt := map[bool]string{false: ".tar.gz", true: ".zip"}[windows]
	if filepath.Base(archivePath) != root+archiveExt {
		return nil, "", "", fmt.Errorf("archive name=%q, want %q", filepath.Base(archivePath), root+archiveExt)
	}
	var (
		entries map[string]archiveEntry
		err     error
	)
	if windows {
		entries, err = readZip(archivePath)
	} else {
		entries, err = readTarGz(archivePath)
	}
	if err != nil {
		return nil, "", "", err
	}
	binaryExt := ""
	if windows {
		binaryExt = ".exe"
	}
	want := map[string]bool{
		root + "/":                                 true,
		root + "/dependencies/":                    true,
		root + "/onprest-gateway" + binaryExt:      false,
		root + "/onprest-agent" + binaryExt:        false,
		root + "/LICENSE":                          false,
		root + "/gateway.env.example":              false,
		root + "/capability.yaml.example":          false,
		root + "/INSTALL.md":                       false,
		root + "/RELEASE-MANIFEST.txt":             false,
		root + "/dependencies/onprest-gateway.txt": false,
		root + "/dependencies/onprest-agent.txt":   false,
	}
	if len(entries) != len(want) {
		names := make([]string, 0, len(entries))
		for name := range entries {
			names = append(names, name)
		}
		sort.Strings(names)
		return nil, "", "", fmt.Errorf("archive entry count=%d, want %d: %s", len(entries), len(want), strings.Join(names, ", "))
	}
	for name, entry := range entries {
		directory, ok := want[name]
		if !ok || directory != entry.directory {
			return nil, "", "", fmt.Errorf("unexpected archive entry %q", name)
		}
	}

	manifest, err := parseKeyValueBytes(entries[root+"/RELEASE-MANIFEST.txt"].data, 7)
	if err != nil {
		return nil, "", "", fmt.Errorf("manifest: %w", err)
	}
	wantManifest := map[string]string{
		"version": cfg.version, "release_tag": cfg.tag, "commit_sha": cfg.sha,
		"target": targetName, "gateway": "onprest-gateway" + binaryExt,
		"agent": "onprest-agent" + binaryExt, "release_ready_url": cfg.runURL,
	}
	if err := compareFields("manifest", manifest, wantManifest); err != nil {
		return nil, "", "", err
	}
	for _, binary := range []string{"gateway", "agent"} {
		binaryName := "onprest-" + binary + binaryExt
		entry := entries[root+"/"+binaryName]
		if !bytes.Contains(entry.data, []byte("onprest-release-version:"+cfg.version)) {
			return nil, "", "", fmt.Errorf("%s version marker mismatch", binaryName)
		}
		if !windows && entry.mode.Perm()&0o111 == 0 {
			return nil, "", "", fmt.Errorf("%s is not executable", binaryName)
		}
		report := entries[root+"/dependencies/onprest-"+binary+".txt"].data
		if len(report) == 0 || !bytes.Contains(report, []byte("\tpath\tgithub.com/viewlegacy/onprest/cmd/"+binary+"\n")) {
			return nil, "", "", fmt.Errorf("%s dependency report mismatch", binaryName)
		}
	}
	for _, name := range []string{"LICENSE", "gateway.env.example", "capability.yaml.example", "INSTALL.md"} {
		if len(entries[root+"/"+name].data) == 0 {
			return nil, "", "", fmt.Errorf("%s is empty", name)
		}
	}
	if targetName == runtime.GOOS+"-"+runtime.GOARCH {
		if err := verifyNativeCLI(entries, root, binaryExt, cfg.version); err != nil {
			return nil, "", "", err
		}
	}
	return entries, root, binaryExt, nil
}

func extractFixedFiles(entries map[string]archiveEntry, root, binaryExt, output string) error {
	if info, err := os.Lstat(output); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("extract output must not be a symlink: %s", output)
		}
		if !info.IsDir() {
			return fmt.Errorf("extract output is not a directory: %s", output)
		}
		entries, err := os.ReadDir(output)
		if err != nil {
			return err
		}
		if len(entries) != 0 {
			return fmt.Errorf("extract output must be new or empty: %s", output)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Join(output, root, "dependencies"), 0o755); err != nil {
		return err
	}
	files := []string{
		"onprest-gateway" + binaryExt,
		"onprest-agent" + binaryExt,
		"LICENSE",
		"gateway.env.example",
		"capability.yaml.example",
		"INSTALL.md",
		"RELEASE-MANIFEST.txt",
		"dependencies/onprest-gateway.txt",
		"dependencies/onprest-agent.txt",
	}
	for _, relative := range files {
		entry := entries[root+"/"+relative]
		mode := os.FileMode(0o644)
		if strings.HasPrefix(relative, "onprest-") && !strings.Contains(relative, "/") {
			mode = 0o755
		}
		if err := os.WriteFile(filepath.Join(output, root, filepath.FromSlash(relative)), entry.data, mode); err != nil {
			return err
		}
	}
	return nil
}

func readTarGz(name string) (map[string]archiveEntry, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	entries := map[string]archiveEntry{}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if !safeArchiveName(h.Name, h.Typeflag == tar.TypeDir) {
			return nil, fmt.Errorf("unsafe tar path %q", h.Name)
		}
		if _, exists := entries[h.Name]; exists {
			return nil, fmt.Errorf("duplicate tar entry %q", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			entries[h.Name] = archiveEntry{directory: true, mode: os.FileMode(h.Mode)}
		case tar.TypeReg, tar.TypeRegA:
			body, err := readLimited(tr, h.Size)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", h.Name, err)
			}
			entries[h.Name] = archiveEntry{mode: os.FileMode(h.Mode), data: body}
		default:
			return nil, fmt.Errorf("unsupported tar entry type for %q", h.Name)
		}
	}
	return entries, nil
}

func readZip(name string) (map[string]archiveEntry, error) {
	r, err := zip.OpenReader(name)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	entries := map[string]archiveEntry{}
	for _, f := range r.File {
		directory := f.FileInfo().IsDir()
		if !safeArchiveName(f.Name, directory) {
			return nil, fmt.Errorf("unsafe zip path %q", f.Name)
		}
		if _, exists := entries[f.Name]; exists {
			return nil, fmt.Errorf("duplicate zip entry %q", f.Name)
		}
		mode := f.Mode()
		if mode&os.ModeType != 0 && !directory {
			return nil, fmt.Errorf("unsupported zip entry type for %q", f.Name)
		}
		if directory {
			entries[f.Name] = archiveEntry{directory: true, mode: mode}
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		body, readErr := readLimited(rc, int64(f.UncompressedSize64))
		closeErr := rc.Close()
		if readErr != nil {
			return nil, fmt.Errorf("%s: %w", f.Name, readErr)
		}
		if closeErr != nil {
			return nil, closeErr
		}
		entries[f.Name] = archiveEntry{mode: mode, data: body}
	}
	return entries, nil
}

func safeArchiveName(name string, directory bool) bool {
	if name == "" || strings.Contains(name, "\\") || strings.IndexByte(name, 0) >= 0 || path.IsAbs(name) {
		return false
	}
	cleanInput := name
	if directory {
		if !strings.HasSuffix(name, "/") {
			return false
		}
		cleanInput = strings.TrimSuffix(name, "/")
	} else if strings.HasSuffix(name, "/") {
		return false
	}
	return cleanInput != "" && path.Clean(cleanInput) == cleanInput && cleanInput != ".." && !strings.HasPrefix(cleanInput, "../")
}

func readLimited(r io.Reader, size int64) ([]byte, error) {
	if size < 0 || size > maxArchiveFileSize {
		return nil, fmt.Errorf("archive file size %d exceeds limit", size)
	}
	body, err := io.ReadAll(io.LimitReader(r, maxArchiveFileSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) != size {
		return nil, fmt.Errorf("archive size=%d, want %d", len(body), size)
	}
	return body, nil
}

func verifyNativeCLI(entries map[string]archiveEntry, root, ext, version string) error {
	tmp, err := os.MkdirTemp("", "onprest-release-verify-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	for _, binary := range []string{"gateway", "agent"} {
		name := "onprest-" + binary + ext
		binaryPath := filepath.Join(tmp, name)
		if err := os.WriteFile(binaryPath, entries[root+"/"+name].data, 0o700); err != nil {
			return err
		}
		cmd := exec.Command(binaryPath, "--version")
		cmd.Dir = string(filepath.Separator)
		output, err := cmd.Output()
		if err != nil {
			return fmt.Errorf("execute %s: %w", name, err)
		}
		if strings.TrimSpace(string(output)) != version {
			return fmt.Errorf("%s --version=%q, want %q", name, strings.TrimSpace(string(output)), version)
		}
	}
	return nil
}

func parseKeyValues(name string, count int) (map[string]string, error) {
	body, err := os.ReadFile(name)
	if err != nil {
		return nil, err
	}
	return parseKeyValueBytes(body, count)
}

func parseKeyValueBytes(body []byte, count int) (map[string]string, error) {
	fields := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		key, value, ok := strings.Cut(line, "=")
		_, exists := fields[key]
		if !ok || key == "" || exists {
			return nil, fmt.Errorf("invalid or duplicate field %q", line)
		}
		fields[key] = value
	}
	if len(fields) != count {
		keys := make([]string, 0, len(fields))
		for key := range fields {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		return nil, fmt.Errorf("field count=%d, want %d (%s)", len(fields), count, strings.Join(keys, ","))
	}
	return fields, nil
}

func compareFields(label string, got, want map[string]string) error {
	if len(got) != len(want) {
		return fmt.Errorf("%s field count=%d, want %d", label, len(got), len(want))
	}
	for key, value := range want {
		if got[key] != value {
			return fmt.Errorf("%s %s=%q, want %q", label, key, got[key], value)
		}
	}
	return nil
}
