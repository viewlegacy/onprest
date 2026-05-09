package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

var executablePath = os.Executable

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
