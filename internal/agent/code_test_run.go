package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/viewlegacy/onprest/internal/agent/artifacts"
	"github.com/viewlegacy/onprest/internal/agent/filesystem"
	"github.com/viewlegacy/onprest/internal/agent/providers"
	"github.com/viewlegacy/onprest/internal/agent/wasmruntime"
	"github.com/viewlegacy/onprest/internal/contract"
)

type codeOutput struct {
	parent       *codeDirectory
	temp         *codeDirectory
	tempName, id string
	committed    bool
}

func beginCodeOutput(path, id string) (*codeOutput, error) {
	parent, e := openCodeDirectory(path)
	if e != nil {
		return nil, e
	}
	name := ".onprest-code-" + id + "-" + randomTransportID()

	created, e := parent.directory.CreatePrivateDirectory(name)
	if e != nil {
		parent.Close()
		return nil, e
	}
	defer created.Close()
	temp, e := openCodeDirectory(filepath.Join(parent.path, name))
	if e != nil {
		parent.root.HostRemove(name)
		parent.Close()
		return nil, e
	}
	if e = parent.bound(); e != nil || temp.directory.Identity() != created.Identity() {
		temp.Close()
		parent.root.HostRemove(name)
		parent.Close()
		return nil, errors.Join(e, errors.New("temporary directory binding changed"))
	}
	return &codeOutput{parent: parent, temp: temp, tempName: name, id: id}, nil
}
func (s *codeOutput) close() error {
	e := s.temp.Close()
	if !s.committed {

		info, check := s.parent.root.HostStat(s.tempName)
		var current *filesystem.Directory
		if check == nil && !info.IsDir() {
			check = errors.New("temporary output is not a directory")
		}
		if check == nil {
			var f *os.File
			f, check = s.parent.root.HostOpen(s.tempName, os.O_RDONLY, 0)
			if check == nil {
				current, check = filesystem.DirectoryFromFile(f, filepath.Join(s.parent.path, s.tempName))
			}
		}
		if check == nil {
			if current.Identity() != s.temp.directory.Identity() {
				check = errors.New("temporary output binding changed")
			}
			current.Close()
		}
		if check == nil {
			e = errors.Join(e, s.parent.removeFlat(s.tempName))
		} else if !errors.Is(check, fs.ErrNotExist) {
			e = errors.Join(e, check)
		}
	}
	return errors.Join(e, s.parent.Close())
}

var codeExtension = regexp.MustCompile(`^[A-Za-z0-9]{1,16}$`)

func codeSavedName(i int, name string) string {
	ext := strings.TrimPrefix(filepath.Ext(name), ".")
	suffix := ""
	if codeExtension.MatchString(ext) {
		suffix = "." + strings.ToLower(ext)
	}
	return fmt.Sprintf("%04d%s", i+1, suffix)
}

type codeSaveWriter interface {
	io.WriteCloser
	Sync() error
}

func (s *codeOutput) save(ctx context.Context, out *contract.PreparedOutput) ([]codeFile, error) {
	return s.saveWithIO(ctx, out, func(name string) (codeSaveWriter, error) {
		return s.temp.root.HostOpen(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	}, nil)
}

// Tests inject writer faults while retaining real descriptor-owned run storage.
func (s *codeOutput) saveWithIO(ctx context.Context, out *contract.PreparedOutput, create func(string) (codeSaveWriter, error), afterPublish func() error) (files []codeFile, err error) {
	files = []codeFile{}
	for i, a := range out.Files {
		if e := ctx.Err(); e != nil {
			return nil, e
		}
		name := codeSavedName(i, a.Name)
		f, e := create(name)
		if e != nil {
			return nil, e
		}
		hash := sha256.New()
		n, e := io.CopyBuffer(io.MultiWriter(f, hash), &codeContextReader{ctx: ctx, reader: a.Reader}, make([]byte, 64<<10))
		if e == nil && (n != a.SizeBytes || hex.EncodeToString(hash.Sum(nil)) != a.SHA256) {
			e = errors.New("output snapshot size changed")
		}
		if e == nil {
			e = f.Sync()
		}
		e = errors.Join(e, f.Close())
		if e != nil {
			return nil, e
		}
		files = append(files, codeFile{ArtifactMetadata: a.ArtifactMetadata, SavedPath: filepath.Join(s.parent.path, s.id, name)})
	}
	if len(files) == 0 {
		return files, nil
	}
	if e := out.Close(); e != nil {
		return nil, e
	}
	if e := s.temp.directory.Sync(); e != nil {
		return nil, e
	}
	if e := s.parent.bound(); e != nil {
		return nil, e
	}
	moved, e := s.parent.directory.PublishDirectory(ctx, s.tempName, s.id, s.temp.directory.Identity())
	s.committed = moved
	if moved && afterPublish != nil {
		e = errors.Join(e, afterPublish())
	}
	if e == nil {
		e = s.parent.directory.Sync()
	}
	return files, e
}
func testCode(ctx context.Context, o codeOptions, cfg *contract.Config, cap *contract.Capability, diag *codeDiagnostics) (result codeResult, err error) {
	result = codeResult{Operation: "test", RequestID: ulid.Make().String(), Files: []codeFile{}}
	failure := func(code, outcome string, e error) (codeResult, error) {
		result.Error = &contract.ErrorValue{Code: code, Message: executionMessage(code), Outcome: outcome}
		if code == "CLI_OUTPUT_FAILED" {
			result.Error.Message = "local CLI output could not be completed"
		}
		return result, e
	}
	if o.build {
		if _, e := buildCode(ctx, o, cfg, cap, diag); e != nil {
			return failure("EXECUTION_FAILED", "not_started", e)
		}
	}
	path, e := codeModulePath(o, cap)
	if e != nil {
		return failure("EXECUTION_FAILED", "not_started", e)
	}
	expected, _ := cap.Executor.Settings["sha256"].(string)
	module, e := (&wasmruntime.Runner{}).Prepare(ctx, path, expected, wasmLimits(cap))
	if e != nil {
		return failure("EXECUTION_FAILED", "not_started", e)
	}
	selected := *cfg
	selected.Capabilities = map[string]*contract.Capability{cap.Name: cap.Clone()}
	registry, _ := providers.NewRegistry(map[string]providers.Factory{"wasm": func(ctx context.Context, snapshot *providers.Snapshot) (providers.Provider, error) {
		return &wasmProvider{snapshot: snapshot, module: module, limits: wasmLimits(cap)}, nil
	}})
	prepared, e := registry.Prepare(ctx, &selected, o.config, providers.ResolveOptions{ModuleDigests: map[string]string{cap.Name: module.Digest()}, Environment: providers.ProcessEnvironment()})
	if e != nil {
		return failure("EXECUTION_FAILED", "not_started", e)
	}
	defer func() {
		if e := prepared.Close(); e != nil {
			err = errors.Join(err, e)
			if result.Error == nil {
				result.Error = &contract.ErrorValue{Code: "CLI_OUTPUT_FAILED", Message: "CLI cleanup failed", Outcome: "unknown"}
			}
		}
	}()
	level := cfg.Logging["level"].(string)
	if o.logLevel != "" {
		level = o.logLevel
	}
	sink, e := NewDiagnosticSink(diag, nil, level)
	if e != nil {
		return failure("EXECUTION_FAILED", "not_started", e)
	}
	controller, e := NewExecutionController(prepared, nil, sink)
	if e != nil {
		return failure("EXECUTION_FAILED", "not_started", e)
	}
	var saver *codeOutput
	if o.outputDir != "" {
		saver, e = beginCodeOutput(o.outputDir, result.RequestID)
		if e != nil {
			return failure("CLI_OUTPUT_FAILED", "not_started", e)
		}
		defer func() {
			if e := saver.close(); e != nil {
				err = errors.Join(err, e)
				_ = checkedWrite(diag.w, []byte(fmt.Sprintf("incomplete CLI output remains: %q\n", filepath.Join(saver.parent.path, saver.tempName))))
				if result.Error == nil {
					result.Error = &contract.ErrorValue{Code: "CLI_OUTPUT_FAILED", Message: "CLI cleanup failed", Outcome: "unknown"}
				}
			}
		}()
	}
	spool, e := os.MkdirTemp("", "onprest-code-"+result.RequestID+"-")
	if e != nil {
		return failure("EXECUTION_FAILED", "not_started", e)
	}
	options, e := runtimeArtifactOptions(cfg.Runtime, spool)
	if e != nil {
		os.RemoveAll(spool)
		return failure("EXECUTION_FAILED", "not_started", e)
	}
	store, e := artifacts.Open(options)
	if e != nil {
		os.RemoveAll(spool)
		return failure("EXECUTION_FAILED", "not_started", e)
	}
	defer func() {
		e := store.Close()
		if e == nil {
			e = os.RemoveAll(spool)
		}
		if e != nil {
			err = errors.Join(err, e)
			_ = checkedWrite(diag.w, []byte(fmt.Sprintf("CLI spool cleanup failed; inspect %q\n", spool)))
			if result.Error == nil {
				result.Error = &contract.ErrorValue{Code: "CLI_OUTPUT_FAILED", Message: "CLI cleanup failed", Outcome: "unknown"}
			}
		}
	}()
	snapshot, _ := prepared.Snapshot(cap.Name)
	owner := artifacts.Owner{RequestID: result.RequestID, CallerBinding: "code-cli", Capability: cap.Name, DefinitionHash: snapshot.DefinitionHash(), AuthGeneration: result.RequestID}
	params, e := codeReadParams(o, cap)
	if e != nil {
		return failure("VALIDATION_FAILED", "not_started", e)
	}
	var input *artifacts.Request
	if schemaHasFile(cap.Input) {
		input, e = store.BeginInput(ctx, owner, cap, time.Now().Add(mustCodeDuration(cfg.Runtime["transfer_timeout"].(string))))
		if e != nil {
			return failure(artifactCode(e), "not_started", e)
		}
	}
	params, e = codeReceiveInput(ctx, o, cap, input, owner, params)
	if e != nil {
		code := artifactCode(e)
		var output *contract.OutputError
		if errors.As(e, &output) {
			code = output.Code
		}
		if code == "EXECUTION_FAILED" {
			code = "VALIDATION_FAILED"
		}
		return failure(code, "not_started", e)
	}
	req := ExecutionRequest{ID: result.RequestID, Capability: cap.Name, DefinitionHash: snapshot.DefinitionHash(), Params: params, input: input, owner: owner, stager: store.Scratch(), localOutput: true}
	var scope *artifacts.OutputScope
	if cap.Output.Files != nil {
		scope, e = store.Output(owner, artifacts.OutputLimits{MaxFileBytes: cap.Policy["max_output_file_bytes"].(int64), MaxTotalBytes: cap.Policy["max_total_output_file_bytes"].(int64), MaxFiles: cap.Policy["max_output_files"].(int64), TTL: mustCodeDuration(cap.Policy["file_ttl"].(string))})
		if e != nil {
			return failure(artifactCode(e), "not_started", e)
		}
		req.stager = scope
	}
	out, public := controller.Execute(context.WithValue(ctx, artifactStoreKey{}, store), req)
	if public != nil {
		result.Error = public
		return result, nil
	}
	defer func() {
		if e := out.Close(); e != nil {
			err = errors.Join(err, e)
			if result.Error == nil {
				result.Error = &contract.ErrorValue{Code: "CLI_OUTPUT_FAILED", Message: "CLI cleanup failed", Outcome: "unknown"}
			}
		}
	}()
	result.Data = out.Data
	if saver != nil {
		result.Files, e = saver.save(ctx, out)
		if e != nil {
			if saver.committed {
				_ = checkedWrite(diag.w, []byte(fmt.Sprintf("saved output remains: %q\n", filepath.Join(saver.parent.path, saver.id))))
			}
			return failure("CLI_OUTPUT_FAILED", "unknown", e)
		}
	} else {
		for _, a := range out.Files {
			result.Files = append(result.Files, codeFile{ArtifactMetadata: a.ArtifactMetadata})
		}
	}
	result.OK = true
	return result, nil
}
func mustCodeDuration(v string) time.Duration { d, _ := time.ParseDuration(v); return d }

type codeContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *codeContextReader) Read(b []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	return r.reader.Read(b)
}
