package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/viewlegacy/onprest/internal/contract"
)

type codeOptions struct {
	operation, format, config, capability, output, module, source, params, paramsFile, outputDir, logLevel string
	offline, force, build                                                                                  bool
	inputFiles, inputMIMEs                                                                                 codeFlags
	set                                                                                                    map[string]bool
}
type codeFlags []string

func (v *codeFlags) String() string     { return "" }
func (v *codeFlags) Set(s string) error { *v = append(*v, s); return nil }

type codeFile struct {
	contract.ArtifactMetadata
	SavedPath string `json:"saved_path,omitempty"`
}
type codeResult struct {
	Operation string               `json:"operation"`
	OK        bool                 `json:"ok"`
	RequestID string               `json:"request_id,omitempty"`
	Data      any                  `json:"-"`
	Files     []codeFile           `json:"-"`
	Module    string               `json:"module,omitempty"`
	SHA256    string               `json:"sha256,omitempty"`
	ABI       string               `json:"abi,omitempty"`
	Imports   []string             `json:"imports,omitempty"`
	Manifest  string               `json:"manifest,omitempty"`
	Error     *contract.ErrorValue `json:"error,omitempty"`
}

func (r codeResult) MarshalJSON() ([]byte, error) {
	type plain codeResult
	if r.Operation == "test" && r.OK {
		return json.Marshal(struct {
			plain
			Data  any        `json:"data"`
			Files []codeFile `json:"files"`
		}{plain(r), r.Data, r.Files})
	}
	if r.OK && r.Operation != "test" {
		imports := r.Imports
		if imports == nil {
			imports = []string{}
		}
		return json.Marshal(struct {
			plain
			Imports []string `json:"imports"`
		}{plain(r), imports})
	}
	return json.Marshal(plain(r))
}
func codeUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:\n  onprest-agent code build --output FILE [--source DIR] [--offline] [--force] [--format text|json]\n  onprest-agent code build --config FILE --capability NAME [--source DIR] [--offline] [--force] [--format text|json]\n  onprest-agent code validate --module FILE [--format text|json]\n  onprest-agent code test --config FILE --capability NAME [--params JSON | --params-file FILE] [--input-file POINTER=PATH] [--input-mime POINTER=MIME] [--output-dir DIR] [--log-level LEVEL] [--format text|json]\n  onprest-agent code test --build --config FILE --capability NAME [--source DIR] [--offline] [--force] [test options]")
}
func parseCode(args []string) (codeOptions, error) {
	o := codeOptions{format: "text", source: ".", set: map[string]bool{}}
	if len(args) == 0 {
		return o, errors.New("missing operation")
	}
	o.operation = args[0]
	if o.operation == "help" || o.operation == "--help" || o.operation == "-h" {
		return o, flag.ErrHelp
	}
	if o.operation != "build" && o.operation != "validate" && o.operation != "test" {
		return o, errors.New("unknown operation")
	}
	f := flag.NewFlagSet("code "+o.operation, flag.ContinueOnError)
	f.SetOutput(io.Discard)
	for n, p := range map[string]*string{"format": &o.format, "config": &o.config, "capability": &o.capability, "output": &o.output, "module": &o.module, "source": &o.source, "params": &o.params, "params-file": &o.paramsFile, "output-dir": &o.outputDir, "log-level": &o.logLevel} {
		f.StringVar(p, n, *p, "")
	}
	f.BoolVar(&o.offline, "offline", false, "")
	f.BoolVar(&o.force, "force", false, "")
	f.BoolVar(&o.build, "build", false, "")
	f.Var(&o.inputFiles, "input-file", "")
	f.Var(&o.inputMIMEs, "input-mime", "")
	if e := f.Parse(args[1:]); e != nil {
		return o, e
	}
	f.Visit(func(v *flag.Flag) { o.set[v.Name] = true })
	invalid := f.NArg() != 0 || o.format != "text" && o.format != "json"
	allowed := map[string]bool{"format": true}
	switch o.operation {
	case "build":
		for _, n := range []string{"config", "capability", "output", "source", "offline", "force"} {
			allowed[n] = true
		}
		invalid = invalid || (o.output == "") == (o.config == "" && o.capability == "") || (o.config == "") != (o.capability == "")
	case "validate":
		allowed["module"] = true
		invalid = invalid || o.module == ""
	case "test":
		for _, n := range []string{"config", "capability", "params", "params-file", "input-file", "input-mime", "output-dir", "log-level", "build"} {
			allowed[n] = true
		}
		if o.build {
			for _, n := range []string{"source", "offline", "force"} {
				allowed[n] = true
			}
		}
		invalid = invalid || o.config == "" || o.capability == "" || o.set["params"] && o.set["params-file"]
		if o.set["log-level"] {
			_, ok := diagnosticLevel(o.logLevel)
			invalid = invalid || !ok
		}
	}
	for n := range o.set {
		if !allowed[n] {
			invalid = true
		}
	}
	for n := range o.set {
		if n != "params" && n != "format" {
			if p := map[string]string{"config": o.config, "capability": o.capability, "output": o.output, "module": o.module, "source": o.source, "params-file": o.paramsFile, "output-dir": o.outputDir, "log-level": o.logLevel}[n]; p == "" && (n == "config" || n == "capability" || n == "output" || n == "module" || n == "source" || n == "params-file" || n == "output-dir" || n == "log-level") {
				invalid = true
			}
		}
	}
	if invalid {
		return o, errors.New("invalid arguments")
	}
	return o, nil
}

// codeDiagnostics accepts common JSON records but emits escaped contextual text.
// Its error is retained even where a provider intentionally ignores sink writes.
type codeDiagnostics struct {
	mu  sync.Mutex
	w   io.Writer
	err error
}

func (d *codeDiagnostics) Write(b []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var v map[string]any
	if e := json.Unmarshal(b, &v); e != nil {
		d.err = e
		return 0, e
	}
	line := fmt.Sprintf("%v %v source=%v event=%v", v["ts"], v["level"], v["source"], v["event"])
	for _, k := range []string{"capability", "request_id", "error_code", "message", "detail"} {
		if x, ok := v[k]; ok {
			line += fmt.Sprintf(" %s=%q", k, fmt.Sprint(x))
		}
	}
	e := checkedWrite(d.w, []byte(line+"\n"))
	if e != nil {
		d.err = e
		return 0, e
	}
	return len(b), nil
}
func (d *codeDiagnostics) failure() error { d.mu.Lock(); defer d.mu.Unlock(); return d.err }
func handleCodeCLI(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	signal.Ignore(syscall.SIGPIPE)
	o, e := parseCode(args)
	if errors.Is(e, flag.ErrHelp) {
		codeUsage(stdout)
		return 0
	}
	if e != nil {
		fmt.Fprintln(stderr, "code: invalid arguments")
		codeUsage(stderr)
		return 2
	}
	diag := &codeDiagnostics{w: stderr}
	result := codeResult{Operation: o.operation}
	var cfg *contract.Config
	var cap *contract.Capability
	if o.config != "" {
		o.config, e = filepath.Abs(o.config)
		if e == nil {
			var b []byte
			b, e = os.ReadFile(o.config)
			if e == nil {
				cfg, e = contract.Compile(b)
			}
		}
		if e == nil {
			cap = cfg.Capabilities[o.capability]
			if cap == nil || cap.Executor.Type != "wasm" {
				e = errors.New("selected capability must be wasm")
			}
		}
	}
	if e == nil {
		switch o.operation {
		case "build":
			result, e = buildCode(ctx, o, cfg, cap, diag)
		case "validate":
			result, e = validateCode(ctx, o.module)
		case "test":
			result, e = testCode(ctx, o, cfg, cap, diag)
		}
	}
	if e != nil && result.Error == nil {
		result.Error = &contract.ErrorValue{Code: "EXECUTION_FAILED", Message: "code operation could not complete", Outcome: "not_started"}
	}
	if e != nil {
		_ = checkedWrite(stderr, []byte(fmt.Sprintf("code %s: %q\n", o.operation, e.Error())))
	}
	result.Operation = o.operation
	result.OK = e == nil && result.Error == nil
	if diag.failure() != nil {
		result.OK = false
		if result.Error == nil {
			result.Error = &contract.ErrorValue{Code: "CLI_OUTPUT_FAILED", Message: "CLI diagnostics could not be written", Outcome: "unknown"}
		}
	}
	var b []byte
	if o.format == "json" {
		b, e = json.Marshal(result)
		b = append(b, '\n')
	} else {
		var v bytes.Buffer
		if result.OK {
			if o.operation == "test" {
				raw, _ := json.Marshal(result.Data)
				fmt.Fprintf(&v, "code test succeeded: request_id=%s data=%s\n", result.RequestID, raw)
				for _, f := range result.Files {
					fmt.Fprintf(&v, "file name=%q mime_type=%q size_bytes=%d sha256=%s", f.Name, f.MIMEType, f.SizeBytes, f.SHA256)
					if f.SavedPath != "" {
						fmt.Fprintf(&v, " saved_path=%q", f.SavedPath)
					}
					v.WriteByte('\n')
				}
			} else {
				fmt.Fprintf(&v, "code %s succeeded: module=%q sha256=%s abi=%s\n", o.operation, result.Module, result.SHA256, result.ABI)
			}
		} else {
			fmt.Fprintf(&v, "code %s failed: code=%s message=%q outcome=%s\n", o.operation, result.Error.Code, result.Error.Message, result.Error.Outcome)
		}
		b = v.Bytes()
	}
	if e != nil || checkedWrite(stdout, b) != nil {
		_ = checkedWrite(stderr, []byte("code: result output failed\n"))
		for _, f := range result.Files {
			if f.SavedPath != "" {
				_ = checkedWrite(stderr, []byte(fmt.Sprintf("saved output remains: %q\n", f.SavedPath)))
			}
		}
		return 1
	}
	if !result.OK {
		return 1
	}
	return 0
}
