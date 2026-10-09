package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/viewlegacy/onprest/internal/agent/artifacts"
	"github.com/viewlegacy/onprest/internal/contract"
)

func codeBindings(flags []string) (map[string]string, error) {
	m := map[string]string{}
	for _, v := range flags {
		p, s, ok := strings.Cut(v, "=")
		if !ok || p == "" || s == "" {
			return nil, errors.New("invalid file binding")
		}
		if _, ok := m[p]; ok {
			return nil, errors.New("duplicate file binding")
		}
		m[p] = s
	}
	return m, nil
}
func codeMIME(path string) string {
	table := map[string]string{".txt": "text/plain", ".csv": "text/csv", ".json": "application/json", ".pdf": "application/pdf", ".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".mp4": "video/mp4", ".zip": "application/zip"}
	if v := table[strings.ToLower(filepath.Ext(path))]; v != "" {
		return v
	}
	return "application/octet-stream"
}
func codeReadParams(o codeOptions, c *contract.Capability) (map[string]any, error) {
	b := []byte("{}")
	if o.set["params"] {
		b = []byte(o.params)
	}
	if o.set["params-file"] {
		f, e := codeOpenInput(o.paramsFile)
		if e != nil {
			return nil, e
		}
		var e2 error
		b, e2 = io.ReadAll(io.LimitReader(f, c.Limits().MaxBytes+1))
		e = errors.Join(e2, f.Close())
		if e != nil {
			return nil, e
		}
	}
	// CLI inline bytes have the same finite encoded-JSON budget as REST JSON.
	l := c.Limits()
	v, e := contract.DecodeWireJSON(b, l.MaxBytes, l)
	if e != nil {
		return nil, e
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("params must be a JSON object")
	}
	return m, nil
}
func codeReceiveInput(ctx context.Context, o codeOptions, c *contract.Capability, request *artifacts.Request, owner artifacts.Owner, params map[string]any) (map[string]any, error) {
	paths, e := codeBindings(o.inputFiles)
	if e != nil {
		return nil, e
	}
	media, e := codeBindings(o.inputMIMEs)
	if e != nil {
		return nil, e
	}
	used := map[string]bool{}
	var walk func(*contract.Schema, any, string) (any, error)
	walk = func(schema *contract.Schema, value any, pointer string) (any, error) {
		if schema == nil {
			return value, nil
		}
		if schema.File != nil {
			var reader io.Reader
			var closer io.Closer
			var name, mimetype string
			if p, ok := paths[pointer]; ok {
				if value != nil {
					return nil, errors.New("input flag requires null placeholder")
				}
				used[pointer] = true
				f, e := codeOpenInput(p)
				if e != nil {
					return nil, e
				}
				reader = f
				closer = f
				name = filepath.Base(p)
				mimetype = codeMIME(p)
				if v, ok := media[pointer]; ok {
					mt, parameters, e := mime.ParseMediaType(v)
					if e != nil || len(parameters) != 0 || strings.Contains(mt, "*") || !strings.Contains(mt, "/") {
						f.Close()
						return nil, errors.New("invalid input MIME")
					}
					mimetype = strings.ToLower(mt)
				}
			} else {
				if value == nil {
					return nil, errors.New("file null requires input flag")
				}
				m, r, e := contract.DecodeInlineFile(value, schema.File.MaxBytes)
				if e != nil {
					return nil, e
				}
				reader = r
				name = m.Name
				mimetype = m.MIMEType
				if _, ok := media[pointer]; ok {
					return nil, errors.New("MIME override requires local input file")
				}
			}
			if closer != nil {
				defer func() {
					if closer != nil {
						closer.Close()
					}
				}()
			}
			if request == nil {
				return nil, errors.New("unexpected file input")
			}
			stage, e := request.NewFile(owner, pointer, name, mimetype)
			if e != nil {
				return nil, e
			}
			hash := sha256.New()
			var bytes int64
			var b [64 << 10]byte
			for {
				if e = ctx.Err(); e != nil {
					return nil, e
				}
				n, readErr := reader.Read(b[:])
				if n > 0 {
					hash.Write(b[:n])
					bytes += int64(n)
					if e = checkedWrite(stage, b[:n]); e != nil {
						return nil, e
					}
				}
				if readErr == io.EOF {
					break
				}
				if readErr != nil {
					return nil, readErr
				}
				if n == 0 {
					return nil, io.ErrNoProgress
				}
			}
			if closer != nil {
				if e = closer.Close(); e != nil {
					return nil, e
				}
				closer = nil
			}
			if e = stage.Close(); e != nil {
				return nil, e
			}
			return request.Complete(owner, stage, bytes, hex.EncodeToString(hash.Sum(nil)))
		}
		switch x := value.(type) {
		case map[string]any:
			for _, k := range schema.PropertyOrder {
				if v, ok := x[k]; ok {
					n, e := walk(schema.Properties[k], v, pointer+"/"+strings.ReplaceAll(strings.ReplaceAll(k, "~", "~0"), "/", "~1"))
					if e != nil {
						return nil, e
					}
					x[k] = n
				}
			}
		case []any:
			for i, v := range x {
				n, e := walk(schema.Items, v, pointer+"/"+strconv.Itoa(i))
				if e != nil {
					return nil, e
				}
				x[i] = n
			}
		}
		return value, nil
	}
	v, e := walk(c.Input, params, "")
	if e != nil {
		return nil, e
	}
	for p := range paths {
		if !used[p] {
			return nil, errors.New("unused input file binding")
		}
	}
	for p := range media {
		if !used[p] {
			return nil, errors.New("unused input MIME binding")
		}
	}
	if request != nil {
		return request.Commit(owner, v.(map[string]any))
	}
	normalized, _, e := c.ValidateInput(v.(map[string]any))
	return normalized, e
}
