# Onprest Go SDK

Package `onprest` (`github.com/viewlegacy/onprest/sdk/go`) lets you write Onprest code capabilities in Go. Your module runs inside the agent as a WebAssembly (WASI preview 1) command; the agent includes the runtime, so the agent host needs no compiler.

The SDK is an independent module and requires Go 1.26.9 or later. Pin a release in your `go.mod`; SDK tags are `sdk/go/vX.Y.Z`. [Writing code in Go](https://docs.onprest.viewlegacy.com/capabilities/code/go) walks through building, testing, and deploying a module, and the [customer CSV example](https://github.com/viewlegacy/onprest/tree/main/examples/sdk) is a complete one.

Build with the agent's author CLI, which adds the metadata the agent checks:

```sh
onprest-agent code build --source ./code --output ./module.wasm
onprest-agent code validate --module ./module.wasm
```

The [CLI reference](https://docs.onprest.viewlegacy.com/reference/cli#author-commands) lists every build and test option.

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

`Run` decodes the input into a new `P` and calls your handler once. The agent has already validated the input and applied defaults, so the handler only sees values the capability's schema allows.

- `P` must be a struct or a map with string keys. Field names match the JSON exactly, including `json` tags. Unknown fields, ambiguous embedded fields, and `json:",string"` fail before the handler runs.
- A missing field keeps its Go zero value. To receive `null`, use a pointer, map, slice, interface, or `json.RawMessage`; `RawMessage` is nil when the field was missing and `null` when it was sent as null.
- Integers keep full precision and must fit the Go type. A fractional number cannot decode into an integer. In `any`, numbers arrive as `json.Number`.

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

`Data` can be any JSON-serializable value; leaving it out returns `null`. Each file needs a plain file name, a lowercase MIME type without parameters, and a non-nil reader. Files keep their order. The capability's `output` setting still decides what reaches the caller.

When you return a file, `Run` takes over its reader: it reads it to the end and closes it, even if the handler also returns an error. Do not `defer` closing a reader you return in `Result.Files`. Resources you open but do not return are yours to close. Closing never undoes a write that already happened.

## Input files

Use `InputFile`, `[]InputFile`, or nested fields of those types for file inputs. `Name()`, `MIMEType()`, `Size()`, and `SHA256()` describe the file, and `Open(ctx)` returns an `io.ReadCloser`; you can open it more than once. Uploaded files are readable even in a sandbox with no permissions, but only during the call that received them. An `InputFile` cannot be returned in `Result.Data`.

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

Relative paths start at the agent's configuration directory. The agent's OS permissions and, with the sandbox on, its grants decide what each call may touch. Things to keep in mind:

- `Create` and `WriteFile` do not create parent directories; call `MkdirAll` first. `Create` truncates an existing file.
- `Rename` is a plain OS rename or replace, with no copy fallback across filesystems.
- `ReadDir` returns entries sorted by name and can return some entries together with an error.
- Every read, write, and metadata call counts toward the capability's file budgets for the whole call; reopening a file does not reset them. Use `Open` or `Create` with `io.Copy` for large files.
- `WriteFile` checks the size limit before touching the target. A streamed `Create` can leave a partial file if it fails, so close the writer and check its error.

Errors wrap `fs.PathError` (`os.LinkError` for `Rename`). Test them with `errors.Is` against `fs.ErrNotExist`, `fs.ErrPermission`, `fs.ErrExist`, `fs.ErrInvalid`, `fs.ErrClosed`, `errors.ErrUnsupported`, `context.Canceled`, `context.DeadlineExceeded`, or `onprest.ErrLimitExceeded`. Pass the handler's `ctx` (or a context derived from it) to every call.

## HTTP clients

`onprest.NewHTTPClient()` returns an `*http.Client` whose requests go through the agent; `*onprest.Transport` (for example `&onprest.Transport{}`) is the matching `http.RoundTripper` for libraries that take one. `http.DefaultClient` is not changed.

```go
client := onprest.NewHTTPClient()
req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
if err != nil { return onprest.Result{}, err }
resp, err := client.Do(req)
if err != nil { return onprest.Result{}, err }
defer resp.Body.Close() // when consuming the body inside the handler
```

Each request is sent once and never retried. Redirects are not followed unless you enable the client's redirect handling, in which case the agent checks every hop. You get the status, headers, and body of every response, including non-2xx ones; close the response body when you are done, or return it as a file reader. Deadlines and cancellation from `ctx` apply throughout, and TLS verification and the sandbox's HTTP grants always apply.

## Errors and diagnostics

`PublicError(code, message)` returns an error the caller is allowed to see. The code must match `^APP_[A-Z][A-Z0-9_]{0,59}$`, and the message must be 1 to 1024 bytes of UTF-8 without control characters. It reaches the caller unchanged, with HTTP 422 over REST and `isError: true` over MCP, also when wrapped. Any other error, or an invalid code or message, becomes `EXECUTION_FAILED` with a generic message.

`errors.Is(err, onprest.ErrLimitExceeded)` detects any host limit, including through a wrapped `PathError` or `LinkError`. If the handler returns a limit error, the caller gets `LIMIT_EXCEEDED`, even when a `PublicError` is in the same chain. You may catch a limit and still succeed, but the used budget is not restored. The encoded result (data plus file metadata) may be up to 32 MiB. Failures while writing the result or reading returned files become `OUTPUT_INVALID`. A timeout or cancellation from the host takes precedence over all of these.

`Log(level, message)` writes to the agent's local log, at level `debug`, `info`, `warn`, or `error`. An invalid level is dropped with one warning, and logging an error does not fail the handler. Anything the module writes to stdout or stderr also goes to the local log, never to the caller. See [Agent Local Logs](https://docs.onprest.viewlegacy.com/agent/local-logs#level-and-rotation) to enable debug messages, and keep secrets out of log messages.

Modules run as Go WASI programs, so cgo, listening sockets, and child processes are not available, and not every existing Go package works. A native build of your code compiles against the SDK but has no agent to talk to.
