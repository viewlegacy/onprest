package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	onprest "github.com/viewlegacy/onprest/sdk/go"
	"golang.org/x/oauth2"
)

type custom int32

func (v *custom) UnmarshalJSON(b []byte) error {
	var n int32
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*v = custom(n + 1)
	return nil
}

type embedded struct {
	Embedded string `json:"embedded"`
}
type Input struct {
	embedded
	Scenario  string              `json:"scenario"`
	Path      string              `json:"path"`
	URL       string              `json:"url"`
	Code      string              `json:"code"`
	Message   string              `json:"message"`
	I32       int32               `json:"i32"`
	I64       int64               `json:"i64"`
	UInt      uint64              `json:"uint"`
	Int       int                 `json:"int"`
	Float     float32             `json:"float"`
	Custom    custom              `json:"custom"`
	Pointer   *int64              `json:"pointer"`
	Raw       json.RawMessage     `json:"raw"`
	Any       any                 `json:"any"`
	Document  onprest.InputFile   `json:"document"`
	Documents []onprest.InputFile `json:"documents"`
	Nested    map[string]struct {
		Document *onprest.InputFile `json:"document"`
	} `json:"nested"`
}
type externalReader struct {
	reader     io.Reader
	closeError bool
	readError  bool
	closed     int
}

func (r *externalReader) Read(b []byte) (int, error) {
	if r.readError {
		return 0, errors.New("private reader detail")
	}
	return r.reader.Read(b)
}
func (r *externalReader) Close() error {
	r.closed++
	fmt.Printf("EXTERNAL_CLOSE:%d\n", r.closed)
	if r.closeError {
		return errors.New("private close detail")
	}
	return nil
}

type requestBody struct {
	blocked           chan struct{}
	closes            int
	readErr, closeErr bool
}

func (b *requestBody) Read(p []byte) (int, error) {
	if b.blocked != nil {
		<-b.blocked
		return 0, context.Canceled
	}
	if b.readErr {
		return 0, io.ErrUnexpectedEOF
	}
	return 0, io.EOF
}
func (b *requestBody) Close() error {
	b.closes++
	if b.blocked != nil {
		close(b.blocked)
	}
	if b.closeErr {
		return errors.New("private request close failure")
	}
	return nil
}

var waitAfterRun bool

func main() {
	onprest.Run(func(ctx context.Context, in Input) (onprest.Result, error) {
		fmt.Println("SDK_HANDLER_ENTERED")
		fail := func(err error) (onprest.Result, error) { return onprest.Result{}, err }
		switch in.Scenario {
		case "decode":
			return onprest.Result{Data: map[string]any{"i32": in.I32, "i64": in.I64, "uint": in.UInt, "int": in.Int, "bits": strconv.IntSize, "float": in.Float, "custom": in.Custom, "pointer": in.Pointer, "raw": in.Raw, "any": in.Any, "embedded": in.Embedded}}, nil
		case "data":
			return onprest.Result{Data: map[string]any{"answer": int64(9007199254740993)}}, nil
		case "nil":
			return onprest.Result{}, nil
		case "files", "mixed", "files-limit-total":
			r := onprest.Result{Files: []onprest.File{{Name: "test.pdf", MIMEType: "application/pdf", Reader: bytes.NewReader([]byte{0, 255, 1, 2})}, {Name: "empty.mp4", MIMEType: "video/mp4", Reader: strings.NewReader("")}}}
			if in.Scenario == "files-limit-total" {
				r.Files[1].Reader = bytes.NewReader([]byte{0, 255, 1, 2})
			}
			if in.Scenario == "mixed" {
				r.Data = map[string]any{"ok": true}
			}
			return r, nil
		case "input":
			r, err := in.Document.Open(ctx)
			if err != nil {
				return fail(err)
			}
			other, err := in.Document.Open(ctx)
			if err != nil {
				return fail(err)
			}
			b, err := io.ReadAll(other)
			if err != nil {
				return fail(err)
			}
			if err = other.Close(); err != nil {
				return fail(err)
			}
			return onprest.Result{Data: map[string]any{"name": in.Document.Name(), "mime": in.Document.MIMEType(), "size": in.Document.Size(), "sha256": in.Document.SHA256(), "reopen": len(b), "count": len(in.Documents), "nested": len(in.Nested)}, Files: []onprest.File{{Name: in.Document.Name(), MIMEType: in.Document.MIMEType(), Reader: r}}}, nil
		case "input-data":
			return onprest.Result{Data: in.Document}, nil
		case "invalid-data":
			return onprest.Result{Data: make(chan int)}, nil
		case "nil-reader":
			var r *bytes.Reader
			return onprest.Result{Files: []onprest.File{{Name: "nil.bin", MIMEType: "application/octet-stream", Reader: r}}}, nil
		case "read-error", "close-error", "handler-error":
			r := &externalReader{reader: strings.NewReader("value"), readError: in.Scenario == "read-error", closeError: in.Scenario == "close-error" || in.Scenario == "handler-error"}
			result := onprest.Result{Files: []onprest.File{{Name: "value.bin", MIMEType: "application/octet-stream", Reader: r}}}
			if in.Scenario == "handler-error" {
				return result, onprest.PublicError("APP_PRIMARY", "primary refusal")
			}
			return result, nil
		case "early-close":
			r, err := onprest.Files.Open(ctx, in.Path)
			if err != nil {
				return fail(err)
			}
			r.Close()
			return onprest.Result{Files: []onprest.File{{Name: "value.bin", MIMEType: "application/octet-stream", Reader: r}}}, nil
		case "panic":
			_, _ = onprest.Files.Open(ctx, in.Path)
			panic("private panic detail")
		case "public":
			return fail(fmt.Errorf("private wrapping: %w", onprest.PublicError(in.Code, in.Message)))
		case "general":
			return fail(errors.New("private handler detail"))
		case "joined":
			return fail(errors.Join(onprest.PublicError("APP_REFUSAL", "refusal"), fmt.Errorf("private path: %w", onprest.ErrLimitExceeded)))
		case "limit":
			_, err := onprest.Files.ReadFile(ctx, in.Path)
			return fail(err)
		case "caught-limit":
			_, err := onprest.Files.ReadFile(ctx, in.Path)
			if !errors.Is(err, onprest.ErrLimitExceeded) {
				return fail(fmt.Errorf("expected limit: %w", err))
			}
			_, err = onprest.Files.ReadFile(ctx, in.Path)
			return onprest.Result{Data: map[string]any{"still_limited": errors.Is(err, onprest.ErrLimitExceeded)}}, nil
		case "fs":
			dir := in.Path + "/child"
			if err := onprest.Files.MkdirAll(ctx, dir, 0750); err != nil {
				return fail(err)
			}
			path := dir + "/file.bin"
			payload := []byte{0, 255, 1, 2}
			if err := onprest.Files.WriteFile(ctx, path, payload, 0640); err != nil {
				return fail(err)
			}
			r, err := onprest.Files.Open(ctx, path)
			if err != nil {
				return fail(err)
			}
			var copied bytes.Buffer
			if _, err = io.Copy(&copied, r); err != nil {
				return fail(err)
			}
			if err = r.Close(); err != nil {
				return fail(err)
			}
			if err = r.Close(); err != nil {
				return fail(err)
			}
			w, err := onprest.Files.Create(ctx, path)
			if err != nil {
				return fail(err)
			}
			if _, err = io.Copy(w, bytes.NewReader(payload)); err != nil {
				return fail(err)
			}
			if err = w.Close(); err != nil {
				return fail(err)
			}
			entries, err := onprest.Files.ReadDir(ctx, dir)
			if err != nil {
				return fail(err)
			}
			info, err := entries[0].Info()
			if err != nil {
				return fail(err)
			}
			stat, err := onprest.Files.Stat(ctx, path)
			if err != nil {
				return fail(err)
			}
			b, err := onprest.Files.ReadFile(ctx, path)
			if err != nil {
				return fail(err)
			}
			next := dir + "/renamed.bin"
			if err = onprest.Files.Rename(ctx, path, next); err != nil {
				return fail(err)
			}
			if err = onprest.Files.Remove(ctx, next); err != nil {
				return fail(err)
			}
			if err = onprest.Files.Remove(ctx, dir); err != nil {
				return fail(err)
			}
			return onprest.Result{Data: map[string]any{"bytes": len(b), "copy": bytes.Equal(payload, copied.Bytes()), "name": info.Name(), "size": stat.Size(), "sys_nil": info.Sys() == nil}}, nil
		case "chunks":
			payload := bytes.Repeat([]byte{0, 255, 1}, int(in.I64)/3+1)[:int(in.I64)]
			err := onprest.Files.WriteFile(ctx, in.Path, payload, 0600)
			if err != nil {
				return fail(err)
			}
			b, err := onprest.Files.ReadFile(ctx, in.Path)
			if err != nil {
				return fail(err)
			}
			return onprest.Result{Data: map[string]any{"bytes": len(b), "equal": bytes.Equal(b, payload)}}, nil
		case "read-dir-partial":
			entries, err := onprest.Files.ReadDir(ctx, in.Path)
			names := []string{}
			for _, entry := range entries {
				names = append(names, entry.Name())
			}
			return onprest.Result{Data: map[string]any{"names": names, "limit": errors.Is(err, onprest.ErrLimitExceeded), "permission": errors.Is(err, fs.ErrPermission), "invalid": errors.Is(err, fs.ErrInvalid), "failed": err != nil, "notfound": errors.Is(err, fs.ErrNotExist)}}, nil
		case "result-timeout":
			waitAfterRun = true
			return onprest.Result{Data: map[string]any{"ok": true}}, nil
		case "envelope-boundary", "large-handler-error":
			data := strings.Repeat("x", int(in.I64)-len(`{"data":"","files":[]}`))
			result := onprest.Result{Data: data}
			if in.Scenario == "large-handler-error" {
				return result, onprest.PublicError("APP_PRIMARY", "primary")
			}
			return result, nil
		case "fs-errors":
			_, notfound := onprest.Files.Open(ctx, in.Path+"/missing")
			err := onprest.Files.WriteFile(ctx, in.Path+"/missing-parent/file", []byte("a"), 0600)
			_, bad := onprest.Files.Stat(ctx, "bad\x00name")
			rename := onprest.Files.Rename(ctx, in.Path+"/missing", "other")
			badPerm := onprest.Files.WriteFile(ctx, in.Path+"/bad-perm", nil, fs.ModeSetuid)
			badDirPerm := onprest.Files.MkdirAll(ctx, in.Path+"/bad-dir-perm", fs.ModeSticky)
			emptyPath := in.Path + "/empty-file"
			emptyErr := onprest.Files.WriteFile(ctx, emptyPath, nil, 0600)
			empty, bread := onprest.Files.ReadFile(ctx, emptyPath)
			before, bstat := onprest.Files.Stat(ctx, emptyPath)
			rewrite := onprest.Files.WriteFile(ctx, emptyPath, nil, 0777)
			after, astat := onprest.Files.Stat(ctx, emptyPath)
			emptyOK := emptyErr == nil && bread == nil && len(empty) == 0 && bstat == nil && rewrite == nil && astat == nil && before.Mode() == after.Mode()
			if remove := onprest.Files.Remove(ctx, emptyPath); remove != nil {
				return fail(remove)
			}
			var pathErr *fs.PathError
			var linkErr *os.LinkError
			return onprest.Result{Data: map[string]any{"notfound": errors.Is(notfound, fs.ErrNotExist) && errors.As(notfound, &pathErr), "no_parent": errors.Is(err, fs.ErrNotExist), "invalid": errors.Is(bad, fs.ErrInvalid), "link": errors.As(rename, &linkErr) && errors.Is(rename, fs.ErrNotExist), "empty": emptyOK, "perm_invalid": errors.Is(badPerm, fs.ErrInvalid) && errors.Is(badDirPerm, fs.ErrInvalid)}}, nil
		case "write-limit":
			err := onprest.Files.WriteFile(ctx, in.Path, []byte("12345"), 0600)
			return onprest.Result{Data: map[string]any{"limit": errors.Is(err, onprest.ErrLimitExceeded)}}, nil
		case "stream-limit":
			w, err := onprest.Files.Create(ctx, in.Path)
			if err != nil {
				return fail(err)
			}
			n, err := w.Write([]byte("12345"))
			closeErr := w.Close()
			return onprest.Result{Data: map[string]any{"count": n, "limit": errors.Is(err, onprest.ErrLimitExceeded), "closed": closeErr == nil}}, nil
		case "total-limit":
			_, err := onprest.Files.ReadFile(ctx, in.Path)
			if err != nil {
				return fail(err)
			}
			_, err = onprest.Files.ReadFile(ctx, in.Path)
			return onprest.Result{Data: map[string]any{"limit": errors.Is(err, onprest.ErrLimitExceeded)}}, nil
		case "request-read-error", "request-close-error", "request-upload-cancel":
			b := &requestBody{readErr: in.Scenario == "request-read-error", closeErr: in.Scenario == "request-close-error"}
			requestCtx := ctx
			if in.Scenario == "request-upload-cancel" {
				var cancel context.CancelFunc
				requestCtx, cancel = context.WithCancel(ctx)
				defer cancel()
				b.blocked = make(chan struct{})
				go func() { time.Sleep(25 * time.Millisecond); cancel() }()
			}
			req, _ := http.NewRequestWithContext(requestCtx, "POST", in.URL, b)
			resp, err := onprest.NewHTTPClient().Do(req)
			if resp != nil {
				resp.Body.Close()
			}
			return onprest.Result{Data: map[string]any{"failed": err != nil, "closes": b.closes}}, nil
		case "path":
			valid := true
			for _, path := range []string{string([]byte{255}), "bad\x00name"} {
				_, e1 := onprest.Files.Open(ctx, path)
				_, e2 := onprest.Files.Create(ctx, path)
				_, e3 := onprest.Files.ReadFile(ctx, path)
				e4 := onprest.Files.WriteFile(ctx, path, nil, 0600)
				_, e5 := onprest.Files.ReadDir(ctx, path)
				_, e6 := onprest.Files.Stat(ctx, path)
				e7 := onprest.Files.MkdirAll(ctx, path, 0700)
				e8 := onprest.Files.Remove(ctx, path)
				e9 := onprest.Files.Rename(ctx, path, "ok")
				e10 := onprest.Files.Rename(ctx, "ok", path)
				for _, err := range []error{e1, e2, e3, e4, e5, e6, e7, e8, e9, e10} {
					valid = valid && errors.Is(err, fs.ErrInvalid)
				}
			}
			err := onprest.Files.WriteFile(ctx, "valid�", []byte{255}, 0600)
			b, readErr := onprest.Files.ReadFile(ctx, "valid�")
			removeErr := onprest.Files.Remove(ctx, "valid�")
			return onprest.Result{Data: map[string]any{"invalid": valid, "replacement": err == nil && readErr == nil && removeErr == nil && bytes.Equal(b, []byte{255})}}, nil
		case "fs-permission":
			_, err := onprest.Files.Open(ctx, in.Path)
			rename := onprest.Files.Rename(ctx, in.Path, in.Path+"-new")
			var link *os.LinkError
			return onprest.Result{Data: map[string]any{"permission": errors.Is(err, fs.ErrPermission), "link": errors.As(rename, &link) && errors.Is(rename, fs.ErrPermission)}}, nil
		case "log":
			for _, level := range []string{"debug", "info", "warn", "error", "invalid"} {
				onprest.Log(level, "private diagnostic\nvalue")
			}
			return onprest.Result{Data: map[string]any{"ok": true}}, nil
		case "http", "library", "http-file", "deadline", "cancel", "redirect", "response-cancel", "default-redirect", "http-denied", "redirect-denied":
			client := onprest.NewHTTPClient()
			if in.Scenario == "library" {
				client = oauth2.NewClient(context.WithValue(ctx, oauth2.HTTPClient, client), oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "fixture-token"}))
			}
			if in.Scenario == "redirect" || in.Scenario == "redirect-denied" {
				client.CheckRedirect = nil
			}
			requestCtx := ctx
			var cancel context.CancelFunc
			if in.Scenario == "deadline" {
				requestCtx, cancel = context.WithTimeout(ctx, 25*time.Millisecond)
				defer cancel()
			}
			if in.Scenario == "cancel" || in.Scenario == "response-cancel" {
				requestCtx, cancel = context.WithCancel(ctx)
				go func() { time.Sleep(25 * time.Millisecond); cancel() }()
				defer cancel()
			}
			req, err := http.NewRequestWithContext(requestCtx, "POST", in.URL, strings.NewReader("payload"))
			if err != nil {
				return fail(err)
			}
			resp, err := client.Do(req)
			if in.Scenario == "deadline" || in.Scenario == "cancel" {
				return onprest.Result{Data: map[string]any{"deadline": errors.Is(err, context.DeadlineExceeded), "cancel": errors.Is(err, context.Canceled)}}, nil
			}
			if in.Scenario == "http-denied" || in.Scenario == "redirect-denied" {
				return onprest.Result{Data: map[string]any{"permission": errors.Is(err, fs.ErrPermission)}}, nil
			}
			if err != nil {
				return fail(err)
			}
			if in.Scenario == "http-file" {
				return onprest.Result{Files: []onprest.File{{Name: "body.bin", MIMEType: "application/octet-stream", Reader: resp.Body}}}, nil
			}
			b, err := io.ReadAll(resp.Body)
			closeErr := resp.Body.Close()
			if in.Scenario == "response-cancel" {
				return onprest.Result{Data: map[string]any{"cancel": errors.Is(err, context.Canceled)}}, nil
			}
			if err != nil {
				return fail(err)
			}
			if closeErr != nil {
				return fail(closeErr)
			}
			return onprest.Result{Data: map[string]any{"status": resp.StatusCode, "body": string(b), "length": resp.ContentLength, "header": resp.Header.Get("X-Fixture")}}, nil
		case "side-effect":
			if err := onprest.Files.WriteFile(ctx, in.Path, []byte("changed"), 0600); err != nil {
				return fail(err)
			}
			return fail(onprest.PublicError("APP_AFTER_WRITE", "written"))
		default:
			fmt.Fprintln(os.Stderr, "unknown scenario")
			return fail(errors.New("unknown scenario"))
		}
	})
	if waitAfterRun {
		time.Sleep(30 * time.Second)
	}
}
