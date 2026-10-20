package filesystem

import (
	"io/fs"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

func ValidateHostPath(p string) error {
	if !utf8.ValidString(p) || p == "" || strings.ContainsRune(p, 0) {
		return fs.ErrInvalid
	}
	rest := strings.TrimPrefix(p, filepath.VolumeName(p))
	for _, part := range strings.Split(filepath.ToSlash(rest), "/") {
		if e := platformName(part); e != nil {
			return e
		}
	}
	return nil
}
