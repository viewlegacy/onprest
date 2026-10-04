package contract

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"math"
	"reflect"
	"strings"
	"unicode/utf8"
)

// ProviderResult contains business data before publication. Row providers must
// supply column metadata even when Data is an empty row array. Files transfer
// ownership of their readers to ProcessOutput, including on failure.
type ProviderResult struct {
	ReturnedRows, AffectedRows *int64
	Data                       any
	Columns                    []string
	Files                      []ProviderFile
}
type ProviderFile struct {
	Name, MIMEType string
	Reader         io.Reader
	// Optional integrity assertions are checked against the entire stream.
	SizeBytes *int64
	SHA256    string
}
type ArtifactMetadata struct {
	Name      string `json:"name"`
	MIMEType  string `json:"mime_type"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
}
type OutputArtifact struct {
	ArtifactMetadata
	Reader io.ReadCloser
}

// ArtifactStager supplies private, bounded storage, not public delivery. The
// Agent implementations supply an explicit private output ownership scope.
// Begin must not publish bytes. Close flushes a stage; Finish opens a reader of
// the completed immutable bytes. Abort releases the stage, including after
// Finish. It must be safe after a failed Close or Finish.
type ArtifactStager interface {
	Begin(context.Context, string, string) (ArtifactStage, error)
}
type ArtifactStage interface {
	io.WriteCloser
	Finish(ArtifactMetadata) (io.ReadCloser, error)
	Abort() error
}

// PreparedOutput is returned only after all data and artifact streams have
// passed validation. Close releases all readers and private stages. Callers
// must keep it alive until their consumer finishes; Close does not undo any
// business effects performed by the provider.
type PreparedOutput struct {
	ReturnedRows, AffectedRows *int64
	Data                       any
	Files                      []OutputArtifact
	stages                     []ArtifactStage
	closed                     bool
}

func (o *PreparedOutput) Close() error {
	if o.closed {
		return nil
	}
	o.closed = true
	var errs []error
	for _, f := range o.Files {
		errs = append(errs, f.Reader.Close())
	}
	for _, s := range o.stages {
		errs = append(errs, s.Abort())
	}
	return errors.Join(errs...)
}

// OutputError carries a safe category; the cause is for local diagnostics only.
// Its Error text never includes provider values, paths, or reader errors.
type OutputError struct {
	Code  string
	cause error
}

func (e *OutputError) Error() string {
	if e.Code == "LIMIT_EXCEEDED" {
		return "output exceeds the configured limit"
	}
	return "output does not satisfy the configured contract"
}
func (e *OutputError) Unwrap() error { return e.cause }
func invalidOutput(e error) error {
	if errors.Is(e, context.Canceled) || errors.Is(e, context.DeadlineExceeded) {
		return e
	}
	var existing *OutputError
	if errors.As(e, &existing) {
		return existing
	}
	return &OutputError{"OUTPUT_INVALID", e}
}
func outputLimit() error                        { return &OutputError{"LIMIT_EXCEEDED", nil} }
func policyInt(c *Capability, key string) int64 { n, _ := c.Policy[key].(int64); return n }
func outputLimits(c *Capability) Limits {
	return Limits{MaxBytes: policyInt(c, "max_output_bytes"), MaxDepth: policyInt(c, "max_json_depth"), MaxItems: policyInt(c, "max_json_items")}
}
func rowProvider(c *Capability) bool {
	return c.Executor.Type == "csv" || c.Executor.Type == "db" && c.Executor.Operation == "select"
}
func dmlProvider(c *Capability) bool {
	return c.Executor.Type == "db" && c.Executor.Operation != "select"
}

// ProjectOutput applies selection before value constraints. It never changes
// the definition or the provider's data. JSON validity and structural bounds
// are checked before recursive copying, including values later excluded.
func (c *Capability) ProjectOutput(data any, columns []string) (any, error) {
	l := outputLimits(c)
	raw := l
	raw.MaxBytes = math.MaxInt64
	if e := checkOutputJSON(data, raw); e != nil {
		return nil, e
	}
	if rowProvider(c) {
		if int64(len(columns)) > l.MaxItems {
			return nil, outputLimit()
		}
		rows, ok := data.([]any)
		if !ok {
			return nil, invalidOutput(errors.New("row provider must return a nonnull array"))
		}
		if e := validateColumns(columns); e != nil {
			return nil, invalidOutput(e)
		}
		if c.Output.Mode != "passthrough" && c.Output.Schema.Items != nil {
			for _, k := range c.Output.Schema.Items.PropertyOrder {
				if !contains(columns, k) {
					return nil, invalidOutput(errors.New("declared column is missing from provider metadata"))
				}
			}
		}
		for _, v := range rows {
			row, ok := v.(map[string]any)
			if !ok || row == nil {
				return nil, invalidOutput(errors.New("row provider must return nonnull objects"))
			}
			if c.Output.Mode != "passthrough" && c.Output.Schema.Items != nil {
				for _, k := range c.Output.Schema.Items.PropertyOrder {
					if _, ok := row[k]; !ok {
						return nil, invalidOutput(errors.New("declared column is missing from a row"))
					}
				}
			}
		}
	} else if dmlProvider(c) {
		obj, ok := data.(map[string]any)
		if !ok || len(obj) != 1 {
			return nil, invalidOutput(errors.New("DML must return only count"))
		}
		count, ok := exactInteger(obj["count"])
		if !ok || count < 0 {
			return nil, invalidOutput(errors.New("DML count must be a nonnegative int64"))
		}
		data = map[string]any{"count": count}
	}
	value := projectValue(c.Output.Schema, data, c.Output.Mode == "passthrough")
	value, e := c.Output.Schema.validate(value, "", false, nil)
	if e != nil {
		return nil, invalidOutput(e)
	}
	if e := checkOutputJSON(value, l); e != nil {
		return nil, e
	}
	return value, nil
}
func checkOutputJSON(v any, l Limits) error {
	// FileValue is an internal input type and must never appear in output data.
	if e := checkJSONLimits(v, l); e != nil {
		if strings.Contains(e.Error(), "limit exceeded") {
			return outputLimit()
		}
		return invalidOutput(e)
	}
	var visit func(any) bool
	visit = func(v any) bool {
		switch x := v.(type) {
		case FileValue:
			return false
		case map[string]any:
			for _, v := range x {
				if !visit(v) {
					return false
				}
			}
		case []any:
			for _, v := range x {
				if !visit(v) {
					return false
				}
			}
		}
		return true
	}
	if !visit(v) {
		return invalidOutput(errors.New("input file metadata is not output JSON"))
	}
	return nil
}
func projectValue(s *Schema, v any, passthrough bool) any {
	if passthrough {
		return v
	}
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, v := range x {
			if child, ok := s.Properties[k]; ok {
				out[k] = projectValue(child, v, false)
			} else if !contains(s.Types, "object") || s.AdditionalAllowed {
				if s.Additional != nil {
					out[k] = projectValue(s.Additional, v, false)
				} else {
					out[k] = v
				}
			}
		}
		return out
	case []any:
		if s.Items == nil {
			return x
		}
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = projectValue(s.Items, v, false)
		}
		return out
	default:
		return v
	}
}
func validateColumns(columns []string) error {
	seen := map[string]bool{}
	for _, c := range columns {
		if c == "" || !utf8.ValidString(c) || seen[c] {
			return errors.New("invalid or duplicate provider column metadata")
		}
		seen[c] = true
	}
	return nil
}
func nilInterface(v any) bool {
	if v == nil {
		return true
	}
	r := reflect.ValueOf(v)
	switch r.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Chan, reflect.Func, reflect.Interface:
		return r.IsNil()
	}
	return false
}

// ProcessOutput consumes original streams or generates CSV into private stages.
// No result is returned after a late read/write/Close/validation failure. A
// stager is required only when files are actually published; omitted files are
// closed without reading. There is no URI, expiry, persistent spool, or transport
// implementation in this package.
func ProcessOutput(ctx context.Context, c *Capability, source ProviderResult, stager ArtifactStager) (out *PreparedOutput, err error) {
	ready := &PreparedOutput{Files: []OutputArtifact{}, ReturnedRows: source.ReturnedRows, AffectedRows: source.AffectedRows}
	sourceClosed := make([]bool, len(source.Files))
	closeSource := func(i int) error {
		if sourceClosed[i] {
			return nil
		}
		sourceClosed[i] = true
		if cl, ok := source.Files[i].Reader.(io.Closer); ok && !nilInterface(cl) {
			return cl.Close()
		}
		return nil
	}
	defer func() {
		for i := range source.Files {
			if e := closeSource(i); e != nil && err == nil {
				err = invalidOutput(e)
			}
		}
		if err == nil {
			err = ctx.Err()
		}
		if err != nil {
			_ = ready.Close()
			out = nil
		}
	}()
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	generateCSV := c.Output.Files != nil && c.Output.Files["format"] == "csv"
	if generateCSV && len(source.Files) > 0 {
		return nil, invalidOutput(errors.New("generated CSV and original files cannot be mixed"))
	}
	var projected any
	if c.Output.Data || generateCSV {
		projected, err = c.ProjectOutput(source.Data, source.Columns)
		if err != nil {
			return nil, err
		}
	}
	if c.Output.Data {
		ready.Data = projected
		if rowProvider(c) {
			ready.Data = map[string]any{"rows": projected, "count": int64(len(projected.([]any)))}
		}
		if e := checkOutputJSON(ready.Data, outputLimits(c)); e != nil {
			return nil, e
		}
	}
	if c.Output.Files == nil {
		return ready, nil
	}
	count := len(source.Files)
	if generateCSV {
		count = 1
	}
	if int64(count) > policyInt(c, "max_output_files") {
		return nil, outputLimit()
	}
	if c.Output.Files["delivery"].(map[string]any)["rest"] == "direct" && count != 1 {
		return nil, invalidOutput(errors.New("direct output requires exactly one file"))
	}
	total := int64(0)
	makeFile := func(name, mime string, write func(io.Writer) error) error {
		if e := ValidateBasename(name); e != nil {
			return invalidOutput(e)
		}
		if !validMIME(mime) {
			return invalidOutput(errors.New("invalid output MIME type"))
		}
		if nilInterface(stager) {
			return invalidOutput(errors.New("private artifact stager is required"))
		}
		stage, e := stager.Begin(ctx, name, mime)
		if e != nil {
			return invalidOutput(e)
		}
		if nilInterface(stage) {
			return invalidOutput(errors.New("private artifact stage is missing"))
		}
		ready.stages = append(ready.stages, stage)
		writer := &artifactWriter{ctx: ctx, dest: stage, hash: sha256.New(), remaining: min(policyInt(c, "max_output_file_bytes"), policyInt(c, "max_total_output_file_bytes")-total)}
		if e = write(writer); e != nil {
			_ = stage.Close()
			return e
		}
		if e = stage.Close(); e != nil {
			return invalidOutput(e)
		}
		if e = ctx.Err(); e != nil {
			return e
		}
		metadata := ArtifactMetadata{name, mime, writer.size, hex.EncodeToString(writer.hash.Sum(nil))}
		reader, e := stage.Finish(metadata)
		if e != nil {
			if !nilInterface(reader) {
				_ = reader.Close()
			}
			return invalidOutput(e)
		}
		if nilInterface(reader) {
			return invalidOutput(errors.New("completed artifact reader is missing"))
		}
		ready.Files = append(ready.Files, OutputArtifact{metadata, reader})
		total += writer.size
		return nil
	}
	if generateCSV {
		name := c.Output.Files["filename"].(string)
		mime := c.Output.Files["mime_type"].(string)
		if e := makeFile(name, mime, func(w io.Writer) error { return c.WriteCSV(w, projected, source.Columns) }); e != nil {
			return nil, e
		}
	} else {
		for i, f := range source.Files {
			mime := f.MIMEType
			if fixed, ok := c.Output.Files["mime_type"].(string); ok {
				mime = fixed
			}
			if nilInterface(f.Reader) {
				return nil, invalidOutput(errors.New("output reader is missing"))
			}
			if f.SizeBytes != nil && *f.SizeBytes < 0 || f.SHA256 != "" && !digestRE.MatchString(f.SHA256) {
				return nil, invalidOutput(errors.New("invalid expected artifact integrity"))
			}
			e := makeFile(f.Name, mime, func(w io.Writer) error {
				hash := sha256.New()
				size, e := copyArtifact(ctx, io.MultiWriter(w, hash), f.Reader)
				if e != nil {
					return e
				}
				if f.SizeBytes != nil && size != *f.SizeBytes || f.SHA256 != "" && hex.EncodeToString(hash.Sum(nil)) != f.SHA256 {
					return invalidOutput(errors.New("artifact integrity mismatch"))
				}
				return nil
			})
			closeErr := closeSource(i)
			if e != nil {
				return nil, e
			}
			if closeErr != nil {
				return nil, invalidOutput(closeErr)
			}
		}
	}
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	return ready, nil
}

type artifactWriter struct {
	ctx             context.Context
	dest            io.Writer
	hash            hash.Hash
	remaining, size int64
}

func (w *artifactWriter) Write(p []byte) (int, error) {
	if e := w.ctx.Err(); e != nil {
		return 0, e
	}
	if int64(len(p)) > w.remaining {
		return 0, outputLimit()
	}
	total := 0
	for len(p) > 0 {
		if e := w.ctx.Err(); e != nil {
			return total, e
		}
		chunk := p[:min(len(p), 64*1024)]
		n, e := w.dest.Write(chunk)
		if n < 0 || n > len(chunk) {
			return total, invalidOutput(errors.New("invalid writer count"))
		}
		if n > 0 {
			_, _ = w.hash.Write(chunk[:n])
			w.size += int64(n)
			w.remaining -= int64(n)
			total += n
		}
		if e != nil {
			return total, invalidOutput(e)
		}
		if n != len(chunk) {
			return total, invalidOutput(io.ErrShortWrite)
		}
		p = p[len(chunk):]
	}
	return total, nil
}
func copyArtifact(ctx context.Context, w io.Writer, r io.Reader) (int64, error) {
	buf := make([]byte, 64*1024)
	total := int64(0)
	empty := 0
	for {
		if e := ctx.Err(); e != nil {
			return total, e
		}
		n, e := r.Read(buf)
		if n < 0 || n > len(buf) {
			return total, invalidOutput(errors.New("invalid reader count"))
		}
		if n > 0 {
			empty = 0
			written, we := w.Write(buf[:n])
			total += int64(written)
			if we != nil {
				return total, we
			}
		} else {
			empty++
			if empty > 100 {
				return total, invalidOutput(io.ErrNoProgress)
			}
		}
		if e == io.EOF {
			return total, nil
		}
		if e != nil {
			return total, invalidOutput(e)
		}
	}
}
