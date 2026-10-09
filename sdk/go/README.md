# Onprest Go SDK

Use `github.com/viewlegacy/onprest/sdk/go` (package `onprest`) in Go WASI preview1 command modules. The SDK is an independent module and requires Go 1.26.9 or later. Pin its version in your application's `go.mod`; SDK tags use `sdk/go/vX.Y.Z`. Build with `GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0`. Deploy the completed module and its `onprest` import metadata with the Agent; the installed Agent includes the runtime and needs no compiler. The [complete customer CSV example](../../examples/sdk/README.md) uses the author CLI to build and locally test the module.

Build your application's package directory with the Agent author CLI, which adds and verifies the required custom metadata:

```sh
onprest-agent code build --source ./code --output ./module.wasm
onprest-agent code validate --module ./module.wasm
```

The [CLI reference](../../docs/app/reference/cli/page.mdx#build-and-test-wasm-code) covers configuration-based builds and local tests, including input files, saved output and diagnostics.

## Handler and results

```go
package main

import (
    "context"

    onprest "github.com/viewlegacy/onprest/sdk/go"
)

type Input struct {
    Customer string `json:"customer"`
}

func main() {
    onprest.Run(func(ctx context.Context, in Input) (onprest.Result, error) {
        return onprest.Result{Data: map[string]any{"customer": in.Customer}}, nil
    })
}
```

`Run[P any]` reads one normalized JSON input, decodes a new zero value of `P`, calls the handler, and sends a terminal result through the dedicated host ABI. The Agent applies input validation and defaults before the module runs. Defaults fill missing fields; explicit null is preserved.

`P` must be a struct or a map with string keys. JSON field names match exactly, including `json` tags; unknown fields, ambiguous embedded fields and `json:",string"` are rejected before the handler. Missing fields retain their Go zero values. Null requires a pointer, map, slice, interface or RawMessage.

Integers retain precision and must fit their Go bit width; fractional values cannot become integers. `int` and `uint` use the Wasm target's width. `any` keeps numbers as `json.Number`. `json.RawMessage` distinguishes missing (nil) from explicit null (`null`).

Custom JSON/text decoders receive checked ordinary JSON, with the same null and numeric checks. File descriptors cannot be exposed through `any`, RawMessage or custom decoding. Decode diagnostics contain the JSON Pointer, expected type and reason, without recording input values automatically.

```go
type Result struct {
    Data any
    Files []File
}
type File struct {
    Name string
    MIMEType string
    Reader io.Reader
}
```

Data may be any JSON-serializable value, including null; omitted Data is null. Files retain their order. Supply a valid basename, lowercase parameter-free MIME type, and non-nil reader for every file. Arbitrary file bytes are streamed without encoding them into control JSON. YAML output settings determine what is published.

Returning a File transfers reader ownership to Run even if the handler also returns an error. Run reads successful files and closes every returned closer. SDK readers/writers have close-once ownership; explicit Close is supported and unreturned resources are reclaimed after success, error or handler panic. Do not defer-close a reader returned in Result.Files. External resources remain the author's responsibility until returned. Forced worker termination does not guarantee arbitrary Go Close code; the Agent stops and waits for the worker and recovers its host resources. Close does not roll back business writes.

## Input files

Use `InputFile`, `[]InputFile`, or nested typed fields for YAML file inputs. Access metadata through `Name()`, `MIMEType()`, `Size()` and `SHA256()`. `Open(ctx)` returns an `io.ReadCloser` and may reopen the request-owned input. It works with sandbox deny-all because the input belongs to that request. Zero values, foreign execution contexts and expired executions fail. InputFile cannot be serialized in Result.Data.

```go
reader, err := in.Document.Open(ctx)
if err != nil { return onprest.Result{}, err }
return onprest.Result{Files: []onprest.File{{
    Name: in.Document.Name(), MIMEType: in.Document.MIMEType(), Reader: reader,
}}}, nil
```

## Files on the Agent

`onprest.Files` provides these context-bound methods:

| Method | Result |
| --- | --- |
| `Open(ctx, path)` | `io.ReadCloser, error` |
| `Create(ctx, path)` | `io.WriteCloser, error` |
| `ReadFile(ctx, path)` | `[]byte, error` |
| `WriteFile(ctx, path, data, perm)` | `error` |
| `ReadDir(ctx, path)` | `[]fs.DirEntry, error` |
| `Stat(ctx, path)` | `fs.FileInfo, error` |
| `MkdirAll(ctx, path, perm)` | `error` |
| `Remove(ctx, path)` | `error` |
| `Rename(ctx, oldPath, newPath)` | `error` |

Relative paths use the Agent configuration directory. All paths require valid UTF-8 and no NUL. The host applies OS permissions and configured sandbox grants. Create truncates existing files and creates new files with mode 0666 subject to OS umask/ACL. WriteFile and MkdirAll accept only the low nine permission bits; they do not change existing permissions. Create and WriteFile do not create parents. Remove deletes a file or empty directory. Rename uses host OS move/replace semantics without copy/delete fallback.

ReadDir returns name-sorted entries and can return partial entries with an error. Entry.Info uses the same execution context and authorization; FileInfo.Sys is nil. For larger files use Open/Create with `io.Copy`. Body chunks are at most 64 KiB. Reads, writes, metadata, failed attempts and entry.Info consume the shared host file budgets; reopening does not reset the execution total. WriteFile reserves the known size before create/truncate, so a size-limit rejection leaves the target unchanged. A streaming Create can leave partial changes on failure. Explicitly Close a writer and check the error to confirm completion.

I/O errors wrap `fs.PathError`, or `os.LinkError` for Rename. Use `errors.Is` with `fs.ErrNotExist`, `fs.ErrPermission`, `fs.ErrExist`, `fs.ErrInvalid`, `fs.ErrClosed`, `errors.ErrUnsupported`, `context.Canceled`, `context.DeadlineExceeded` or `onprest.ErrLimitExceeded`. Run contexts and their derivatives are required for host operations.

## HTTP clients

`onprest.NewHTTPClient()` returns a separate `*http.Client` using `*onprest.Transport`; the zero-value Transport also implements `http.RoundTripper`. Inject that client/transport into compatible external libraries. No global default client is changed.

```go
client := onprest.NewHTTPClient()
req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
if err != nil { return onprest.Result{}, err }
resp, err := client.Do(req)
if err != nil { return onprest.Result{}, err }
defer resp.Body.Close() // when consuming the body inside the handler
```

Each RoundTrip authorizes and sends one hop after the complete request body is ready. It never retries, and NewHTTPClient disables redirects with `http.ErrUseLastResponse`. If you explicitly enable the standard client's redirect handling, every hop is authorized separately by the Agent. Request Body is closed on success and failure; close response Body after consuming it, or return it as a File reader. Request deadlines and cancellation apply during upload, waiting for headers and reading the response. HTTP status, headers and body remain available for application handling, including non-2xx responses. TLS verification and sandbox destination rules remain enforced.

## Errors and diagnostics

`PublicError(code, message)` marks an intentionally public business error. Code must match `^APP_[A-Z][A-Z0-9_]{0,59}$`; message must contain 1–1024 UTF-8 bytes without control characters. Wrapped valid public errors preserve their message through REST 422 and MCP `isError:true`; invalid values and ordinary errors become safe `EXECUTION_FAILED` responses. The Agent rechecks public values and derives outcome from observed effects.

`ErrLimitExceeded` identifies all host limits, also through wrapped PathError/LinkError. Unhandled limits become the system `LIMIT_EXCEEDED` response. A chain containing both a limit and PublicError uses the limit. An existing handler error takes priority over cleanup failures. Catching a limit and succeeding is allowed, but does not restore the budget. The final result JSON envelope has a 32 MiB limit; oversized envelopes and host handle/spool exhaustion remain `LIMIT_EXCEEDED`. Result encoding, write, close and file failures become `OUTPUT_INVALID`, with limits retaining their category. Host timeout, cancellation and worker failure take precedence.

`Log(level, message)` writes local Agent diagnostics; levels are `debug`, `info`, `warn`, `error`. Invalid levels are discarded and cause one warning. Logging at error level does not fail the handler. stdout/stderr are diagnostics and never the result channel. See the [Agent logging reference](../../docs/app/reference/logging/page.mdx#agent) to enable debug messages and locate the bounded local log. Avoid logging secrets; internal details stay local.

Native builds provide stubs without an Agent host connection. The SDK runs in the Agent as a Go WASI command module and supports packages compatible with that profile. It does not provide CGO, general sockets/HTTP servers, child processes, or universal compatibility with existing Go packages.
