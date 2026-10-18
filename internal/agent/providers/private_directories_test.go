package providers

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Folder roots and Wasm filesystem grants may not be, contain, or lie inside
// the private spool or identity directory, however the path is spelled.
func TestFolderAndCodeGrantMayNotOverlapPrivateDirectories(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.yaml")
	for _, d := range []string{"reports", "spool/objects", "outside"} {
		if e := os.MkdirAll(filepath.Join(dir, d), 0o700); e != nil {
			t.Fatal(e)
		}
	}
	if e := os.Symlink(filepath.Join(dir, "spool"), filepath.Join(dir, "alias")); e != nil {
		t.Fatal(e)
	}
	folder := func(root, runtimeLine string) string {
		return runtimeLine + "sources:\n  folders:\n    reports: {root: '" + root + "', access: read}\ncapabilities:\n  read:\n    executor: {type: file, read: {source: reports, selector: {name: report.pdf}}}\n    output: {files: {}}\n"
	}
	cases := map[string]string{
		"config directory holds the default spool": folder(".", ""),
		"root is the spool":                        folder("./spool", ""),
		"root inside the spool":                    folder("./spool/objects", ""),
		"root through a symlink to the spool":      folder("./alias", ""),
		"spool configured inside the root":         folder("./reports", "runtime: {spool_dir: ./reports/spool}\n"),
		"identity inside the root":                 folder("./reports", "runtime: {identity_dir: ./reports/identity}\n"),
		"absolute spool equal to the root":         folder(filepath.Join(dir, "outside"), "runtime: {spool_dir: '"+filepath.Join(dir, "outside")+"'}\n"),
	}
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		cases["case variant of the spool"] = folder("./SPOOL", "")
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, e := Resolve(context.Background(), config(t, body), path, "read", ResolveOptions{})
			if !errors.Is(e, errPrivateDirectoryOverlap) {
				t.Fatalf("overlap accepted: %v", e)
			}
		})
	}
	opts := ResolveOptions{ModuleDigests: map[string]string{"run": strings.Repeat("a", 64)}}
	grant := func(root string) string {
		return "capabilities:\n  run:\n    executor:\n      type: wasm\n      module: module.wasm\n      sandbox: {enabled: true, permissions: {filesystem: [{root: '" + root + "', access: read}]}}\n    output: {data: {}}\n"
	}
	for _, root := range []string{".", "./spool", "./identity/keys", "./alias"} {
		if _, e := Resolve(context.Background(), config(t, grant(root)), path, "run", opts); !errors.Is(e, errPrivateDirectoryOverlap) {
			t.Fatalf("code grant %s accepted: %v", root, e)
		}
	}
	if _, e := Resolve(context.Background(), config(t, grant("./reports")), path, "run", opts); e != nil {
		t.Fatal(e)
	}
	resolve(t, config(t, folder("./reports", "")), path, "read")
	resolve(t, config(t, folder("./outside", "runtime: {spool_dir: ./spool-elsewhere}\n")), path, "read")
	if directoriesOverlap(filepath.Join(dir, "reports"), filepath.Join(dir, "reports-spool")) {
		t.Fatal("sibling name prefix treated as containment")
	}
	if !strings.Contains(errPrivateDirectoryOverlap.Error(), "spool_dir") {
		t.Fatal("refusal does not name the setting")
	}
}
