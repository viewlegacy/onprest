package wasmruntime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A listing that stops part way (here at the file-operation budget) must reach
// a real raw-WASI module as an error, never as a silently shorter listing. The
// runtime's dirent cache marks end-of-directory when fewer entries than asked
// are returned and drops entries returned with a nonzero errno, so raw WASI
// cannot receive both; the error wins. The SDK's Files.ReadDir, which uses the
// host ABI, returns the valid leading entries together with the error.
func TestWasmRawReadDirReportsPartialErrorsInsteadOfTruncating(t *testing.T) {
	r, m, c, base := bridgeSetup(t, true, "read")
	dir := filepath.Join(base, "d")
	os.Mkdir(dir, 0o700)
	for _, name := range []string{"a", "b", "c", "e", "f"} {
		os.WriteFile(filepath.Join(dir, name), []byte(name), 0o600)
	}
	failed := false
	for budget := int64(1); budget <= 12; budget++ {
		l := DefaultLimits()
		l.FileOperations = budget
		out := bridgeRun(t, r, m, c, map[string]any{"scenario": "raw_readdir", "path": "d"}, l).(map[string]any)
		var got []string
		for _, n := range out["names"].([]any) {
			got = append(got, n.(string))
		}
		if failure := out["error"].(string); failure != "" {
			failed = true
		} else if strings.Join(got, ",") != "a,b,c,e,f" {
			t.Fatalf("budget %d silently truncated the listing to %v", budget, got)
		}
	}
	if !failed {
		t.Fatal("no budget interrupted the listing")
	}
}
