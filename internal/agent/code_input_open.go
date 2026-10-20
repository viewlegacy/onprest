package agent

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/viewlegacy/onprest/internal/agent/filesystem"
)

// codeOpenInput is the CLI-only read boundary. Native open refuses a final
// link/reparse point; type validation and subsequent reads use that same handle.
// It intentionally does not change sandbox-OFF SDK filesystem semantics.
func codeOpenInput(path string) (*os.File, error) {
	if e := filesystem.ValidateHostPath(path); e != nil {
		return nil, e
	}
	absolute, e := filepath.Abs(path)
	if e != nil {
		return nil, e
	}
	f, e := codeOpenInputNative(absolute)
	if e != nil {
		return nil, e
	}
	info, e := f.Stat()
	if e != nil || !info.Mode().IsRegular() {
		return nil, errors.Join(e, fs.ErrPermission, f.Close())
	}
	return f, nil
}
