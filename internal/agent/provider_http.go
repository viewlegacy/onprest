package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/viewlegacy/onprest/internal/agent/artifacts"
	"github.com/viewlegacy/onprest/internal/agent/hostservices"
	"github.com/viewlegacy/onprest/internal/agent/providers"
	"github.com/viewlegacy/onprest/internal/contract"
)

type httpProvider struct {
	snapshot *providers.Snapshot
	cap      *contract.Capability
	origin   string
	// Only package tests set a deterministic multipart boundary.
	boundary string
}

func prepareHTTPProvider(ctx context.Context, snapshot *providers.Snapshot) (providers.Provider, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cap := snapshot.Definition()
	source, ok := snapshot.Source(providers.SourceKey{Group: "http", Name: cap.Executor.Settings["source"].(string)})
	if !ok {
		return nil, errors.New("HTTP source is unavailable")
	}
	return &httpProvider{snapshot: snapshot, cap: cap, origin: source["base_url"].(string)}, nil
}
func (p *httpProvider) Close() error { return nil }

// HTTP bindings distinguish absent positions from explicit null, including a
// null ancestor. The compiler has already validated every Pointer and position.
func httpPointer(value any, pointer string) (any, bool) {
	if pointer == "" {
		return value, true
	}
	for _, raw := range strings.Split(pointer[1:], "/") {
		token := strings.ReplaceAll(strings.ReplaceAll(raw, "~1", "/"), "~0", "~")
		if value == nil {
			return nil, false
		}
		switch current := value.(type) {
		case map[string]any:
			var exists bool
			value, exists = current[token]
			if !exists {
				return nil, false
			}
		case []any:
			index, err := strconv.Atoi(token)
			if err != nil || index < 0 || index >= len(current) {
				return nil, false
			}
			value = current[index]
		default:
			return nil, true
		}
	}
	return value, true
}
func httpBinding(binding map[string]any, input map[string]any) (any, bool) {
	if pointer, ok := binding["input"].(string); ok {
		return httpPointer(input, pointer)
	}
	return binding["value"], true
}
func httpText(value any) (string, error) {
	switch value := value.(type) {
	case string:
		return value, nil
	case bool, int64, float64:
		raw, err := contract.CanonicalJSON(value)
		return string(raw), err
	}
	return "", &providers.Failure{Code: "VALIDATION_FAILED", Cause: errors.New("HTTP text binding must be a nonnull scalar")}
}
func httpKeys(fields map[string]any) []string {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
func httpFields(fields map[string]any, input map[string]any) (url.Values, error) {
	values := make(url.Values)
	for _, key := range httpKeys(fields) {
		value, exists := httpBinding(fields[key].(map[string]any), input)
		if !exists {
			continue
		}
		array, ok := value.([]any)
		if !ok {
			array = []any{value}
		}
		for _, item := range array {
			text, err := httpText(item)
			if err != nil {
				return nil, err
			}
			values.Add(key, text)
		}
	}
	return values, nil
}
func httpJSONBody(body map[string]any, input map[string]any) (any, error) {
	if fields, ok := body["fields"].(map[string]any); ok {
		object := make(map[string]any)
		for _, key := range httpKeys(fields) {
			value, exists := httpBinding(fields[key].(map[string]any), input)
			if exists {
				object[key] = value
			}
		}
		return object, nil
	}
	value, exists := httpBinding(body, input)
	if !exists {
		return nil, &providers.Failure{Code: "VALIDATION_FAILED"}
	}
	return value, nil
}

type httpPart struct {
	name, kind, media string
	value             any
}
type httpBody struct {
	kind, media string
	raw         []byte
	parts       []httpPart
	file        contract.FileValue
	boundary    string
}

func (p *httpProvider) bind(input map[string]any) (string, http.Header, httpBody, error) {
	settings := p.cap.Executor.Settings
	query, err := httpFields(settings["query"].(map[string]any), input)
	if err != nil {
		return "", nil, httpBody{}, err
	}
	target, err := url.Parse(p.origin)
	if err != nil {
		return "", nil, httpBody{}, err
	}
	fixed, _ := url.Parse(settings["path"].(string))
	target.Path, target.RawPath = fixed.Path, fixed.RawPath
	target.RawQuery = query.Encode()
	headers := make(http.Header)
	fields := settings["headers"].(map[string]any)
	for _, name := range httpKeys(fields) {
		value, exists := httpBinding(fields[name].(map[string]any), input)
		if !exists {
			continue
		}
		text, err := httpText(value)
		if err != nil {
			return "", nil, httpBody{}, err
		}
		headers.Set(name, text)
	}
	result := httpBody{}
	body, hasBody := settings["body"].(map[string]any)
	if !hasBody {
		return target.String(), headers, result, nil
	}
	result.kind = body["kind"].(string)
	switch result.kind {
	case "json":
		value, e := httpJSONBody(body, input)
		if e != nil {
			return "", nil, result, e
		}
		result.raw, err = contract.CanonicalJSON(value)
		result.media = "application/json"
	case "form":
		values, e := httpFields(body["fields"].(map[string]any), input)
		if e != nil {
			return "", nil, result, e
		}
		result.raw = []byte(values.Encode())
		result.media = "application/x-www-form-urlencoded"
	case "file":
		value, exists := httpPointer(input, body["input"].(string))
		file, ok := value.(contract.FileValue)
		if !exists || !ok {
			return "", nil, result, &providers.Failure{Code: "VALIDATION_FAILED"}
		}
		result.file = file
		result.media = file.MIMEType
		if media, ok := body["content_type"].(string); ok {
			result.media = media
		}
	case "multipart":
		writer := multipart.NewWriter(io.Discard)
		if p.boundary != "" {
			if e := writer.SetBoundary(p.boundary); e != nil {
				return "", nil, result, e
			}
		}
		result.boundary, result.media = writer.Boundary(), writer.FormDataContentType()
		for _, raw := range body["parts"].([]any) {
			part := raw.(map[string]any)
			value, exists := httpBinding(part, input)
			if !exists {
				continue
			}
			item := httpPart{name: part["name"].(string), kind: part["kind"].(string), value: value}
			if media, ok := part["content_type"].(string); ok {
				item.media = media
			}
			switch item.kind {
			case "text":
				text, e := httpText(value)
				if e != nil {
					return "", nil, result, e
				}
				item.value = []byte(text)
			case "json":
				bytes, e := contract.CanonicalJSON(value)
				if e != nil {
					return "", nil, result, e
				}
				item.value = bytes
			case "file":
				files := []contract.FileValue{}
				switch value := value.(type) {
				case contract.FileValue:
					files = append(files, value)
				case []any:
					for _, raw := range value {
						file, ok := raw.(contract.FileValue)
						if !ok {
							return "", nil, result, &providers.Failure{Code: "VALIDATION_FAILED"}
						}
						files = append(files, file)
					}
				default:
					return "", nil, result, &providers.Failure{Code: "VALIDATION_FAILED"}
				}
				item.value = files
			}
			result.parts = append(result.parts, item)
		}
	}
	if err != nil {
		return "", nil, result, err
	}
	headers.Set("Content-Type", result.media)
	return target.String(), headers, result, nil
}

// Library serializers may write large slices. Split them at the shared spool
// chunk boundary without creating a body-sized second buffer.
type httpChunkWriter struct{ io.Writer }

func (w httpChunkWriter) Write(bytes []byte) (int, error) {
	total := 0
	for len(bytes) > 0 {
		count := min(len(bytes), 64<<10)
		n, err := w.Writer.Write(bytes[:count])
		total += n
		if err != nil {
			return total, err
		}
		if n != count {
			return total, io.ErrShortWrite
		}
		bytes = bytes[count:]
	}
	return total, nil
}
func httpCopyInput(ctx context.Context, target io.Writer, file contract.FileValue) error {
	reader, err := OpenInput(ctx, file)
	if err != nil {
		return err
	}
	_, err = io.CopyBuffer(target, reader, make([]byte, 64<<10))
	return errors.Join(err, reader.Close())
}
func (body httpBody) write(ctx context.Context, request *hostservices.Request) error {
	writer := httpChunkWriter{request}
	switch body.kind {
	case "json", "form":
		if _, err := writer.Write(body.raw); err != nil {
			return err
		}
	case "file":
		if err := httpCopyInput(ctx, writer, body.file); err != nil {
			return err
		}
	case "multipart":
		multipartWriter := multipart.NewWriter(writer)
		if err := multipartWriter.SetBoundary(body.boundary); err != nil {
			return err
		}
		for _, part := range body.parts {
			if part.kind == "file" {
				for _, file := range part.value.([]contract.FileValue) {
					media := part.media
					if media == "" {
						media = file.MIMEType
					}
					headers := textproto.MIMEHeader{"Content-Disposition": {mime.FormatMediaType("form-data", map[string]string{"name": part.name, "filename": file.Name})}, "Content-Type": {media}}
					target, err := multipartWriter.CreatePart(headers)
					if err != nil {
						return err
					}
					if err := httpCopyInput(ctx, target, file); err != nil {
						return err
					}
				}
			} else {
				media := "application/json"
				if part.kind == "text" {
					media = "text/plain; charset=utf-8"
				}
				target, err := multipartWriter.CreatePart(textproto.MIMEHeader{"Content-Disposition": {mime.FormatMediaType("form-data", map[string]string{"name": part.name})}, "Content-Type": {media}})
				if err != nil {
					return err
				}
				if _, err = target.Write(part.value.([]byte)); err != nil {
					return err
				}
			}
		}
		if err := multipartWriter.Close(); err != nil {
			return err
		}
	}
	return request.Ready()
}

func (p *httpProvider) run(ctx context.Context, input map[string]any, effects *providers.Effects) (result contract.ProviderResult, cleanup func() error, err error) {
	cleanup = func() error { return nil }
	effects.NotStarted()
	target, headers, body, err := p.bind(input)
	if err != nil {
		return result, cleanup, err
	}
	store, _ := ctx.Value(artifactStoreKey{}).(*artifacts.Store)
	if store == nil {
		return result, cleanup, errors.New("private HTTP storage is unavailable")
	}
	limits := p.cap.Executor.Settings["limits"].(map[string]any)
	service, err := hostservices.FixedSource(p.snapshot, hostservices.Options{Context: ctx, Store: store, MaxRequestBytes: limits["max_http_request_bytes"].(int64), MaxResponseBytes: limits["max_http_response_bytes"].(int64), MaxRequests: 1})
	if err != nil {
		return result, cleanup, err
	}
	cleanup = service.Close
	responseConfig := p.cap.Executor.Settings["response"].(map[string]any)
	redirect := responseConfig["redirect"].(map[string]any)
	for hop := 0; ; hop++ {
		length := int64(len(body.raw))
		if body.kind == "file" {
			length = body.file.SizeBytes
		}
		if body.kind == "multipart" {
			length = -1
		}
		request, e := service.Open(ctx, p.cap.Executor.Settings["method"].(string), target, headers, length, effects)
		if e != nil {
			return result, cleanup, e
		}
		if e = body.write(ctx, request); e != nil {
			return result, cleanup, e
		}
		response, e := request.Do()
		if e != nil {
			return result, cleanup, e
		}
		if response.Status == 301 || response.Status == 302 || response.Status == 303 || response.Status == 307 || response.Status == 308 {
			if redirect["enabled"] == true {
				location := response.Headers.Get("Location")
				if location == "" {
					return result, cleanup, &providers.Failure{Code: "OUTPUT_INVALID", Cause: errors.New("redirect location is missing")}
				}
				target, e = service.RedirectTarget(ctx, p.cap.Executor.Settings["method"].(string), target, location, hop+1, true, int(redirect["max_hops"].(int64)))
				if e != nil {
					return result, cleanup, e
				}
				if e = request.Close(); e != nil {
					return result, cleanup, e
				}
				continue
			}
		}
		success := false
		for _, status := range responseConfig["success_statuses"].([]any) {
			if int64(response.Status) == status.(int64) {
				success = true
			}
		}
		if !success {
			return result, cleanup, &providers.Failure{Code: "EXECUTION_FAILED", Reason: "UPSTREAM_HTTP_STATUS", Cause: fmt.Errorf("upstream returned HTTP %d", response.Status)}
		}
		if responseConfig["kind"] == "file" {
			result.Files = []contract.ProviderFile{{Name: responseConfig["filename"].(string), MIMEType: responseConfig["mime_type"].(string), Reader: response.Body}}
			if p.cap.Output.Files == nil {
				_, err = io.CopyBuffer(io.Discard, response.Body, make([]byte, 64<<10))
			}
			return result, cleanup, err
		}
		raw, e := io.ReadAll(response.Body)
		if e != nil {
			return result, cleanup, e
		}
		result.Data, e = contract.DecodeJSON(raw, contract.Limits{MaxBytes: limits["max_http_response_bytes"].(int64), MaxDepth: p.cap.Policy["max_json_depth"].(int64), MaxItems: p.cap.Policy["max_json_items"].(int64)})
		if e != nil {
			code := "OUTPUT_INVALID"
			if strings.Contains(e.Error(), "limit exceeded") {
				code = "LIMIT_EXCEEDED"
			}
			return result, cleanup, &providers.Failure{Code: code, Cause: e}
		}
		return result, cleanup, nil
	}
}
func (p *httpProvider) ExecutePublication(ctx context.Context, input map[string]any, effects *providers.Effects, publish func(contract.ProviderResult) (*contract.PreparedOutput, error)) (out *contract.PreparedOutput, err error) {
	result, cleanup, err := p.run(ctx, input, effects)
	defer func() {
		err = errors.Join(err, cleanup())
		if err != nil && out != nil {
			_ = out.Close()
			out = nil
		}
	}()
	if err != nil {
		return nil, err
	}
	return publish(result)
}

type httpOwnedReader struct {
	io.Reader
	cleanup func() error
	once    sync.Once
	err     error
}

func (r *httpOwnedReader) Close() error { r.once.Do(func() { r.err = r.cleanup() }); return r.err }
func (p *httpProvider) Execute(ctx context.Context, input map[string]any, effects *providers.Effects) (contract.ProviderResult, error) {
	result, cleanup, err := p.run(ctx, input, effects)
	if err == nil && len(result.Files) == 1 {
		result.Files[0].Reader = &httpOwnedReader{Reader: result.Files[0].Reader, cleanup: cleanup}
		return result, nil
	}
	err = errors.Join(err, cleanup())
	return result, err
}
