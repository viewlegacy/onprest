package providers

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// identityKey publishes only a fully written/synced private key. Linking the
// temporary file is atomic and does not replace another preparer's winning key.
// There is no fallback when an existing key is malformed or unprotected.
func identityKey(directory string) ([]byte, error) {
	if err := privateDirectory(directory); err != nil {
		return nil, err
	}
	path := filepath.Join(directory, "config-hmac.key")
	read := func() ([]byte, error) {
		f, err := openRegular(path, true)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		b, err := io.ReadAll(io.LimitReader(f, 33))
		if err != nil || len(b) != 32 {
			clear(b)
			return nil, errors.New("invalid configuration identity key")
		}
		return b, nil
	}
	if b, err := read(); err == nil {
		return b, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	defer clear(key)
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	tmp := filepath.Join(directory, ".config-hmac-"+hex.EncodeToString(id[:])+".tmp")
	f, err := createPrivate(tmp)
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp)
	n, writeErr := f.Write(key)
	if writeErr == nil && n != len(key) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil {
		return nil, writeErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if err := os.Link(tmp, path); err != nil && !os.IsExist(err) {
		return nil, err
	}
	if err := os.Remove(tmp); err != nil {
		return nil, err
	}
	if err := syncDirectory(directory); err != nil {
		return nil, err
	}
	return read()
}
