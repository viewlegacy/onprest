package filesystem

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"mime"
	"path"
	"reflect"
	"strings"
	"sync"

	"github.com/viewlegacy/onprest/internal/contract"
)

// SnapshotLimits must be positive and finite. They bound original bytes, not
// wire encoding, and do not reserve or replace the stager's shared disk quota.
type SnapshotLimits struct{ MaxFileBytes, MaxTotalBytes, MaxFiles int64 }

// Snapshot owns completed readers and private stages. It is returned only after
// every source, copy, source Close, stage flush, and Finish has succeeded. Close
// releases all stages, including completed ones, after consumers finish.
type Snapshot struct {
	Files    []contract.OutputArtifact
	stages   []contract.ArtifactStage
	readers  []io.ReadCloser
	once     sync.Once
	closeErr error
}

func (s *Snapshot) Close() error {
	s.once.Do(func() {
		var errs []error
		for _, reader := range s.readers {
			errs = append(errs, reader.Close())
		}
		for _, stage := range s.stages {
			errs = append(errs, stage.Abort())
		}
		s.closeErr = joinFailures(errs...)
	})
	return s.closeErr
}

// SnapshotFiles copies only descriptors bound to a completed selection. The
// supplied stager must satisfy contract.ArtifactStager's private immutable
// completion contract. P06 connects the production spool; this API provides no
// path-based fallback storage or ownership/TTL store.
func SnapshotFiles(ctx context.Context, selected []Selection, limits SnapshotLimits, mimeType string, stager contract.ArtifactStager) (_ *Snapshot, err error) {
	if limits.MaxFileBytes < 1 || limits.MaxTotalBytes < 1 || limits.MaxFiles < 1 || limits.MaxFileBytes > limits.MaxTotalBytes || nilLike(stager) {
		return nil, failure("VALIDATION_FAILED", fs.ErrInvalid)
	}
	media, parameters, e := mime.ParseMediaType(mimeType)
	if e != nil || media != mimeType || len(parameters) > 0 || mimeType != strings.ToLower(mimeType) || !strings.Contains(mimeType, "/") || strings.Contains(mimeType, "*") {
		return nil, failure("VALIDATION_FAILED", fs.ErrInvalid)
	}
	if int64(len(selected)) > limits.MaxFiles {
		return nil, failure("LIMIT_EXCEEDED", errors.New("snapshot file count exceeds limit"))
	}
	snapshot := &Snapshot{Files: []contract.OutputArtifact{}}
	defer func() {
		if err != nil {
			err = joinFailures(err, snapshot.Close())
		}
	}()
	var total int64
	buffer := make([]byte, 64*1024)
	for _, selection := range selected {
		if e := ctx.Err(); e != nil {
			return nil, e
		}
		if selection.root == nil {
			return nil, failure("VALIDATION_FAILED", fs.ErrInvalid)
		}
		size := selection.original.info.Size()
		if size > limits.MaxFileBytes || size > limits.MaxTotalBytes-total {
			return nil, failure("LIMIT_EXCEEDED", errors.New("snapshot bytes exceed limit"))
		}
		source, e := selection.Open(ctx)
		if e != nil {
			return nil, e
		}
		name := path.Base(selection.relative)
		stage, e := stager.Begin(ctx, name, mimeType)
		if e != nil {
			return nil, joinFailures(osFailure(e), source.Close())
		}
		if nilLike(stage) {
			return nil, joinFailures(failure("EXECUTION_FAILED", errors.New("stager returned nil stage")), source.Close())
		}
		snapshot.stages = append(snapshot.stages, stage)
		digest := sha256.New()
		var copied int64
		copyErr := func() error {
			for {
				if e := ctx.Err(); e != nil {
					return e
				}
				remaining := min(limits.MaxFileBytes-copied, limits.MaxTotalBytes-total)
				// One extra byte detects growth without allocating another whole file.
				chunk := buffer
				if remaining < int64(len(chunk)) {
					chunk = chunk[:remaining+1]
				}
				n, readErr := source.Read(chunk)
				if int64(n) > remaining {
					return failure("LIMIT_EXCEEDED", errors.New("snapshot bytes exceed limit"))
				}
				if n > 0 {
					written, e := stage.Write(chunk[:n])
					if e != nil {
						return osFailure(e)
					}
					if written != n {
						return osFailure(io.ErrShortWrite)
					}
					_, _ = digest.Write(chunk[:n])
					copied += int64(n)
					total += int64(n)
				}
				if readErr == io.EOF {
					break
				}
				if readErr != nil {
					var typed *Error
					if errors.As(readErr, &typed) {
						return readErr
					}
					return osFailure(readErr)
				}
				if n == 0 {
					return osFailure(io.ErrNoProgress)
				}
			}
			if copied != size {
				return changed(errors.New("source length changed"))
			}
			return source.Verify()
		}()
		closeErr := source.Close()
		if copyErr != nil || closeErr != nil {
			return nil, joinFailures(copyErr, closeErr)
		}
		if e := ctx.Err(); e != nil {
			return nil, e
		}
		if e := stage.Close(); e != nil {
			return nil, osFailure(e)
		}
		metadata := contract.ArtifactMetadata{Name: name, MIMEType: mimeType, SizeBytes: copied, SHA256: hex.EncodeToString(digest.Sum(nil))}
		reader, e := stage.Finish(metadata)
		// Even a faulty stager's reader+error pair transfers reader ownership.
		if !nilLike(reader) {
			snapshot.readers = append(snapshot.readers, reader)
			snapshot.Files = append(snapshot.Files, contract.OutputArtifact{ArtifactMetadata: metadata, Reader: reader})
		}
		if e != nil {
			return nil, osFailure(e)
		}
		if nilLike(reader) {
			return nil, osFailure(errors.New("stager returned nil reader"))
		}
	}
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	return snapshot, nil
}

func nilLike(v any) bool {
	if v == nil {
		return true
	}
	value := reflect.ValueOf(v)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	}
	return false
}
