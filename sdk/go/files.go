package onprest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

type fileOperations struct{}

// Files provides stateless operations on the Agent's filesystem.
var Files fileOperations

func pathValid(path string) bool { return utf8.ValidString(path) && !strings.ContainsRune(path, 0) }
func fileCall(ctx context.Context, op, path string, args map[string]any) (*execution, map[string]any, error) {
	e, err := resolve(ctx)
	if err == nil && !pathValid(path) {
		err = fs.ErrInvalid
	}
	if err != nil {
		return nil, nil, &fs.PathError{Op: op, Path: path, Err: err}
	}
	v, err := e.businessCall(ctx, "files."+op, args)
	if err != nil {
		return e, nil, &fs.PathError{Op: op, Path: path, Err: err}
	}
	return e, v, nil
}
func (fileOperations) Open(ctx context.Context, path string) (io.ReadCloser, error) {
	e, v, err := fileCall(ctx, "open", path, map[string]any{"path": path})
	if err != nil {
		return nil, err
	}
	return e.own(ctx, integer(v["body_handle"]), "read", path, false), nil
}
func (fileOperations) Create(ctx context.Context, path string) (io.WriteCloser, error) {
	e, v, err := fileCall(ctx, "create", path, map[string]any{"path": path})
	if err != nil {
		return nil, err
	}
	return e.own(ctx, integer(v["body_handle"]), "write", path, true), nil
}
func (fileOperations) ReadFile(ctx context.Context, path string) ([]byte, error) {
	e, v, err := fileCall(ctx, "read_file", path, map[string]any{"path": path})
	if err != nil {
		return nil, err
	}
	r := e.own(ctx, integer(v["body_handle"]), "read", path, false)
	b, err := io.ReadAll(r)
	return b, errors.Join(err, r.Close())
}
func (fileOperations) WriteFile(ctx context.Context, path string, data []byte, perm fs.FileMode) error {
	if perm&^0777 != 0 {
		return &fs.PathError{Op: "write_file", Path: path, Err: fs.ErrInvalid}
	}
	e, v, err := fileCall(ctx, "write_file", path, map[string]any{"path": path, "size": len(data), "perm": uint32(perm)})
	if err != nil {
		return err
	}
	w := e.own(ctx, integer(v["body_handle"]), "write", path, true)
	_, err = w.Write(data)
	return errors.Join(err, w.Close())
}

type fileInfo struct {
	name     string
	size     int64
	mode     fs.FileMode
	modified time.Time
	dir      bool
}

func (i fileInfo) Name() string       { return i.name }
func (i fileInfo) Size() int64        { return i.size }
func (i fileInfo) Mode() fs.FileMode  { return i.mode }
func (i fileInfo) ModTime() time.Time { return i.modified }
func (i fileInfo) IsDir() bool        { return i.dir }
func (i fileInfo) Sys() any           { return nil }
func metadataInfo(v map[string]any) (fileInfo, error) {
	name, ok := v["name"].(string)
	date, ok2 := v["mod_time"].(string)
	modified, err := time.Parse(time.RFC3339Nano, date)
	dir, ok3 := v["is_dir"].(bool)
	if !ok || !ok2 || !ok3 || !pathValid(name) || err != nil {
		return fileInfo{}, fs.ErrInvalid
	}
	return fileInfo{name, integer(v["size"]), fs.FileMode(integer(v["mode"])), modified, dir}, nil
}
func (fileOperations) Stat(ctx context.Context, path string) (fs.FileInfo, error) {
	_, v, err := fileCall(ctx, "stat", path, map[string]any{"path": path})
	if err != nil {
		return nil, err
	}
	i, err := metadataInfo(v)
	if err != nil {
		return nil, &fs.PathError{Op: "stat", Path: path, Err: err}
	}
	return i, nil
}

type directoryEntry struct {
	info fileInfo
	ctx  context.Context
	path string
}

func (e directoryEntry) Name() string               { return e.info.Name() }
func (e directoryEntry) IsDir() bool                { return e.info.IsDir() }
func (e directoryEntry) Type() fs.FileMode          { return e.info.Mode().Type() }
func (e directoryEntry) Info() (fs.FileInfo, error) { return Files.Stat(e.ctx, e.path) }
func (fileOperations) ReadDir(ctx context.Context, path string) ([]fs.DirEntry, error) {
	e, v, err := fileCall(ctx, "read_dir", path, map[string]any{"path": path})
	if err != nil {
		return nil, err
	}
	r := e.own(ctx, integer(v["body_handle"]), "read_dir", path, false)
	var entries []fs.DirEntry
	d := json.NewDecoder(r)
	d.UseNumber()
	for {
		var m map[string]any
		err = d.Decode(&m)
		if err == io.EOF {
			err = nil
			break
		}
		if err != nil {
			break
		}
		i, parse := metadataInfo(m)
		if parse != nil {
			err = parse
			break
		}
		separator := "/"
		if strings.HasSuffix(path, "/") || strings.HasSuffix(path, "\\") {
			separator = ""
		}
		entries = append(entries, directoryEntry{i, ctx, path + separator + i.Name()})
	}
	if err == nil {
		err = directoryControlError(v["error"], ctx)
	}
	closeErr := r.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return entries, &fs.PathError{Op: "read_dir", Path: path, Err: err}
	}
	return entries, nil
}
func (fileOperations) MkdirAll(ctx context.Context, path string, perm fs.FileMode) error {
	if perm&^0777 != 0 {
		return &fs.PathError{Op: "mkdir_all", Path: path, Err: fs.ErrInvalid}
	}
	_, _, err := fileCall(ctx, "mkdir_all", path, map[string]any{"path": path, "perm": uint32(perm)})
	return err
}
func (fileOperations) Remove(ctx context.Context, path string) error {
	_, _, err := fileCall(ctx, "remove", path, map[string]any{"path": path})
	return err
}
func (fileOperations) Rename(ctx context.Context, oldPath, newPath string) error {
	e, err := resolve(ctx)
	if err == nil && (!pathValid(oldPath) || !pathValid(newPath)) {
		err = fs.ErrInvalid
	}
	if err == nil {
		_, err = e.businessCall(ctx, "files.rename", map[string]any{"old_path": oldPath, "new_path": newPath})
	}
	if err != nil {
		return &os.LinkError{Op: "rename", Old: oldPath, New: newPath, Err: err}
	}
	return nil
}

// ReadDir embeds positive fixed host codes alongside its partial body.
func directoryControlError(v any, ctx context.Context) error {
	value, ok := v.(json.Number)
	code, parse := value.Int64()
	if !ok || parse != nil || code < 0 || code > 8 {
		return hostError(-6, ctx)
	}
	return hostErrorIf(-int32(code), ctx)
}
