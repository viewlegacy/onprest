package agent

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

var executablePath = os.Executable

var (
	validationRandomSource                  io.Reader = rand.Reader
	validationAcquireNativeLock                       = acquireValidationLock
	validationCreatePrivateFile                       = createPrivateValidationFile
	validationCheckPrivateFile                        = validateExistingPrivateFile
	validationReplacePrivateFile                      = atomicReplaceValidationFile
	validationReadDirectory                           = os.ReadDir
	validationRemoveFile                              = os.Remove
	validationSyncDirectory                           = syncValidationDirectory
	validationLockAcquiredHook                        = func(string) {}
	validationBeforeFixedSuccessCleanupHook           = func() {}
	validationReleaseNativeLock                       = func(lock *validationNativeLock) error { return lock.Close() }
	validationPostCommitDurabilityHook                = func() error { return nil }
)

const validationDetailLogMaxBytes = 1 << 20

var errValidationBusy = errors.New("validation busy")

type detailLogFactory func(LoggingDef) (*preparationDetailLog, error)

type validationLogSession struct {
	NewDetailLog detailLogFactory
	Close        func() error
}

type boundedWriter struct {
	w io.Writer
	n int64
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > validationDetailLogMaxBytes-w.n {
		return 0, fmt.Errorf("validation detail log exceeds size limit")
	}
	n, err := w.w.Write(p)
	w.n += int64(n)
	if err == nil && n != len(p) {
		return n, io.ErrShortWrite
	}
	return n, err
}

type rotatingFileWriter struct {
	mu       sync.Mutex
	path     string
	maxSize  int64
	maxFiles int
	file     *os.File
}

func newAgentDetailLog(logging LoggingDef) (*rotatingFileWriter, error) {
	exe, err := executablePath()
	if err != nil {
		return nil, err
	}
	size, err := parseByteSize(logging.MaxSize)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(filepath.Dir(exe), filepath.Base(exe)+".log")
	return newRotatingFileWriter(path, size, logging.MaxFiles)
}

func newRotatingFileWriter(path string, maxSize int64, maxFiles int) (*rotatingFileWriter, error) {
	if maxSize <= 0 {
		return nil, fmt.Errorf("maxSize must be > 0")
	}
	if maxFiles <= 0 {
		maxFiles = 3
	}
	w := &rotatingFileWriter{path: path, maxSize: maxSize, maxFiles: maxFiles}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *rotatingFileWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.rotateIfNeeded(len(p)); err != nil {
		return 0, err
	}
	return w.file.Write(p)
}

func (w *rotatingFileWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

func (w *rotatingFileWriter) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return os.ErrClosed
	}
	return w.file.Sync()
}

func (w *rotatingFileWriter) open() error {
	if err := os.MkdirAll(filepath.Dir(w.path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	w.file = f
	return nil
}

func newValidationLogSession() (*validationLogSession, error) {
	exe, err := executablePath()
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(exe)
	base := strings.TrimSuffix(filepath.Base(exe), filepath.Ext(exe))
	fixedPath := filepath.Join(dir, base+".validate.log")
	lockPath := filepath.Join(dir, "."+base+".validate.lock")
	lock, busy, err := validationAcquireNativeLock(lockPath)
	if err != nil {
		return nil, err
	}
	if busy {
		return nil, errValidationBusy
	}
	validationLockAcquiredHook(lockPath)
	closeLock := sync.OnceValue(func() error { return validationReleaseNativeLock(lock) })
	fail := func(err error) (*validationLogSession, error) {
		_ = closeLock()
		return nil, err
	}
	if err := recoverValidationTemporaryFiles(dir, base); err != nil {
		return fail(err)
	}
	if err := validationCheckPrivateFile(fixedPath); err != nil {
		return fail(err)
	}

	return &validationLogSession{
		Close: closeLock,
		NewDetailLog: func(LoggingDef) (*preparationDetailLog, error) {
			runID, err := validationRunID()
			if err != nil {
				return nil, err
			}
			temporaryPath := filepath.Join(dir, "."+base+".validate."+runID+".tmp")
			file, err := validationCreatePrivateFile(temporaryPath)
			if err != nil {
				return nil, err
			}
			closed := false
			closeFile := func() error {
				if closed {
					return nil
				}
				closed = true
				return file.Close()
			}
			abort := func() (string, error) {
				_ = closeFile()
				if err := validationRemoveFile(temporaryPath); err != nil && !os.IsNotExist(err) {
					return temporaryPath, err
				}
				return "", nil
			}
			return &preparationDetailLog{
				Writer:         &boundedWriter{w: file},
				Sync:           file.Sync,
				Close:          closeFile,
				TemporaryPath:  temporaryPath,
				PublicPath:     fixedPath,
				AbortTemporary: abort,
				CommitFailure: func() error {
					if err := validationReplacePrivateFile(temporaryPath, fixedPath); err != nil {
						return err
					}
					return validationCheckPrivateFile(fixedPath)
				},
				CleanupSuccess: func() error {
					if err := closeFile(); err != nil {
						return err
					}
					if err := validationRemoveFile(temporaryPath); err != nil && !os.IsNotExist(err) {
						return err
					}
					validationBeforeFixedSuccessCleanupHook()
					if err := validationRemoveFile(fixedPath); err != nil && !os.IsNotExist(err) {
						return err
					}
					return validationSyncDirectory(dir)
				},
			}, nil
		},
	}, nil
}

func validationRunID() (string, error) {
	b := make([]byte, 16)
	if _, err := io.ReadFull(validationRandomSource, b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func recoverValidationTemporaryFiles(dir, base string) error {
	entries, err := validationReadDirectory(dir)
	if err != nil {
		return err
	}
	re := regexp.MustCompile(`^\.` + regexp.QuoteMeta(base) + `\.validate\.[0-9a-f]{32}\.tmp$`)
	for _, entry := range entries {
		if !re.MatchString(entry.Name()) || entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if err := validationCheckPrivateFile(path); err != nil {
			return err
		}
		if err := validationRemoveFile(path); err != nil {
			return err
		}
	}
	return nil
}

func (w *rotatingFileWriter) rotateIfNeeded(incoming int) error {
	if w.file == nil {
		if err := w.open(); err != nil {
			return err
		}
	}
	info, err := w.file.Stat()
	if err != nil {
		return err
	}
	if info.Size()+int64(incoming) <= w.maxSize {
		return nil
	}
	if err := w.file.Close(); err != nil {
		return err
	}
	w.file = nil
	for i := w.maxFiles - 1; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", w.path, i)
		dst := fmt.Sprintf("%s.%d", w.path, i+1)
		_ = os.Remove(dst)
		if _, err := os.Stat(src); err == nil {
			if err := os.Rename(src, dst); err != nil {
				return err
			}
		}
	}
	if w.maxFiles > 0 {
		_ = os.Remove(w.path + ".1")
		if _, err := os.Stat(w.path); err == nil {
			if err := os.Rename(w.path, w.path+".1"); err != nil {
				return err
			}
		}
	} else {
		_ = os.Remove(w.path)
	}
	return w.open()
}
