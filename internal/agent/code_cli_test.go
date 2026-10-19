package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCodeUsage(t *testing.T) {
	for _, args := range [][]string{{}, {"bad"}, {"build"}, {"build", "--output", "x", "--config", "y", "--capability", "z"}, {"build", "--output", "x", "--params", "{}"}, {"validate", "--module", "x", "--force"}, {"test", "--config", "x", "--capability", "y", "--source", "."}, {"test", "--config", "x", "--capability", "y", "--params", "{}", "--params-file", "p"}, {"test", "--config", "x", "--capability", "y", "--log-level", "DEBUG"}, {"test", "--config", "x", "--capability", "y", "--sha256", "a"}} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			var out, err bytes.Buffer
			if n := handleCodeCLI(context.Background(), args, &out, &err); n != 2 || out.Len() != 0 {
				t.Fatalf("exit=%d stdout=%q stderr=%q", n, out.String(), err.String())
			}
		})
	}
	for _, args := range [][]string{{"help"}, {"build", "--help"}, {"validate", "--help"}, {"test", "--help"}} {
		var out bytes.Buffer
		if n := handleCodeCLI(context.Background(), args, &out, io.Discard); n != 0 || !strings.Contains(out.String(), "Usage:") {
			t.Fatal(n, out.String())
		}
	}
}

const codeFixture = `package main
import("bytes"
"context";"errors";"fmt";"io";"os";"time"; onprest "github.com/viewlegacy/onprest/sdk/go")
type Input struct { Scenario string ` + "`json:\"scenario\"`" + `; N int64 ` + "`json:\"n\"`" + `; Small int32 ` + "`json:\"small\"`" + `; Nullable *int64 ` + "`json:\"nullable\"`" + `; Documents []onprest.InputFile ` + "`json:\"documents\"`" + ` }
type lateReader struct {sent bool;closeFail bool}
func(r *lateReader)Read(b []byte)(int,error){if !r.sent{r.sent=true;return copy(b,[]byte("a")),nil};return 0,errors.New("PRIVATE_LATE_READER")}
func(r *lateReader)Close()error{if r.closeFail{return errors.New("PRIVATE_CLOSE_READER")};return nil}
func init(){fmt.Println("BUSINESS_INIT")}
func main(){onprest.Run(func(ctx context.Context,in Input)(onprest.Result,error){
 fmt.Println("BUSINESS_ENTER");fmt.Fprintln(os.Stderr,"PRIVATE_STDERR\x1b[31m");onprest.Log("debug","PRIVATE_DEBUG");onprest.Log("error","PRIVATE_ERROR");onprest.Log("bogus","PRIVATE_INVALID")
 switch in.Scenario {
 case "late-reader":return onprest.Result{Files:[]onprest.File{{Name:"a.txt",MIMEType:"text/plain",Reader:&lateReader{}}}},nil
 case "public":return onprest.Result{},onprest.PublicError("APP_DECLINED","safe declined")
 case "invalid-public":return onprest.Result{},onprest.PublicError("APP_A","private\ncontrol")
 case "general":return onprest.Result{},errors.New("PRIVATE_GENERAL")
 case "limit":return onprest.Result{},errors.Join(errors.New("PRIVATE_LIMIT"),onprest.ErrLimitExceeded)
 case "timeout":time.Sleep(time.Minute)
 case "zero":return onprest.Result{Data:map[string]any{"n":in.N,"nullable":in.Nullable}},nil
 case "rows":return onprest.Result{Data:[]any{map[string]any{"label":"=1+1","n":in.N}}},nil
 }
 data:=map[string]any{"n":in.N,"nullable":in.Nullable,"small":in.Small};files:=[]onprest.File{}
 if in.Scenario=="input" {for _,f:=range in.Documents{r,e:=f.Open(ctx);if e!=nil{return onprest.Result{},e};b,e:=io.ReadAll(r);e=errors.Join(e,r.Close());if e!=nil{return onprest.Result{},e};files=append(files,onprest.File{Name:f.Name(),MIMEType:f.MIMEType(),Reader:bytes.NewReader(b)})};return onprest.Result{Data:data,Files:files},nil}
 for i:=0;i<fixtureCount();i++ {files=append(files,onprest.File{Name:"same.TXT",MIMEType:"text/plain",Reader:bytes.NewBufferString("hello")})}
 return onprest.Result{Data:data,Files:files},nil
})}
`

// The fixture's per-request compile can exceed the 5s default timeout under load.
const codeRunPolicy = "    policy: {timeout: 60s}"

const codeConfig = `schema_version: 1
service: {title: Author CLI, version: 1.0.0}
gateway: {url: ws://127.0.0.1:1/ws/agent, agent_private_key_file: ./unavailable.key}
capabilities:
  run:
    executor: {type: wasm, module: ./module.wasm, effects: none, sandbox: {enabled: true}}
    input:
      properties:
        scenario: {type: string, default: files}
        n: {type: integer, default: 9007199254740993}
        small: {type: integer}
        nullable: {type: [integer, 'null']}
        documents: {items: {type: file, max_bytes: 8}}
    output: {data: {}, files: {}}
` + codeRunPolicy + `
  unrelated:
    executor: {type: wasm, module: ./missing.wasm, sandbox: {enabled: true}}
    output: {data: {}}
`

func TestCodeCLIRealAuthorWorkflow(t *testing.T) {
	repo, e := filepath.Abs("../..")
	if e != nil {
		t.Fatal(e)
	}
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "agent")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command("go", "build", "-trimpath", "-mod=readonly", "-o", bin, "./cmd/agent")
	build.Dir = repo
	if b, e := build.CombinedOutput(); e != nil {
		t.Fatal(e, string(b))
	}
	goroot := exec.Command("go", "env", "GOROOT")
	root, e := goroot.Output()
	if e != nil {
		t.Fatal(e)
	}
	env := append(os.Environ(), "PATH="+filepath.Join(strings.TrimSpace(string(root)), "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	source := filepath.Join(tmp, "author")
	configDir := filepath.Join(tmp, "config")
	outDir := filepath.Join(tmp, "saved")
	for _, d := range []string{source, configDir, outDir} {
		if e := os.Mkdir(d, 0700); e != nil {
			t.Fatal(e)
		}
	}
	write := func(p, s string) {
		t.Helper()
		if e := os.WriteFile(p, []byte(s), 0600); e != nil {
			t.Fatal(e)
		}
	}
	write(filepath.Join(source, "go.mod"), "module author\n\ngo 1.26.9\n\nrequire github.com/viewlegacy/onprest/sdk/go v1.0.0\nreplace github.com/viewlegacy/onprest/sdk/go => "+filepath.Join(repo, "sdk/go")+"\n")
	write(filepath.Join(source, "entry.go"), codeFixture)
	write(filepath.Join(source, "helper.go"), "package main\nfunc fixtureCount()int{return 2}\n")
	write(filepath.Join(source, "excluded_test.go"), "package main\nfunc TestNever(){doesNotExist()}\n")
	write(filepath.Join(source, "ignored_linux.go"), "//go:build linux\n\npackage main\nimport _ \"missing.example/module\"\n")
	config := filepath.Join(configDir, "agent.yaml")
	write(config, codeConfig)
	module := filepath.Join(configDir, "module.wasm")
	manifest := module + ".manifest.json"
	run := func(want int, dir string, args ...string) (map[string]any, string, string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, append([]string{"code"}, args...)...)
		cmd.Dir = dir
		cmd.Env = env
		var out, err bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &err
		e := cmd.Run()
		exit := 0
		if e != nil {
			var ee *exec.ExitError
			if !errors.As(e, &ee) {
				t.Fatal(e)
			}
			exit = ee.ExitCode()
		}
		if exit != want {
			t.Fatalf("%v exit %d want %d\nstdout %s\nstderr %s", args, exit, want, out.String(), err.String())
		}
		var v map[string]any
		if strings.Contains(strings.Join(args, " "), "--format json") {
			if e := json.Unmarshal(out.Bytes(), &v); e != nil {
				t.Fatal(e, out.String(), err.String())
			}
		}
		return v, out.String(), err.String()
	}
	test := func(want int, params string, extras ...string) (map[string]any, string, string) {
		args := []string{"test", "--config", config, "--capability", "run", "--format", "json"}
		if params != "" {
			args = append(args, "--params", params)
		}
		return run(want, source, append(args, extras...)...)
	}
	t.Run("initial-default-source-multiple-files-build-validate-no-business", func(t *testing.T) {
		v, _, diag := run(0, source, "build", "--config", config, "--capability", "run", "--offline", "--format", "json")
		if len(v["sha256"].(string)) != 64 || strings.Contains(diag, "BUSINESS_INIT") {
			t.Fatal(v, diag)
		}
		run(0, source, "validate", "--module", module, "--format", "json")
		m, e := os.ReadFile(manifest)
		if e != nil {
			t.Fatal(e)
		}
		var mm codeManifest
		if e = json.Unmarshal(m, &mm); e != nil || mm.SHA256 != v["sha256"] || mm.ABI == "" || len(mm.RequiredImports) == 0 || len(mm.DependencyHash) != 64 {
			t.Fatal(mm, e)
		}
		b, e := os.ReadFile(module)
		if e != nil || codeHash(b) != mm.SHA256 {
			t.Fatal(e)
		}
	})
	previous, _ := os.ReadFile(module)
	previousManifest, _ := os.ReadFile(manifest)
	pairUnchanged := func() {
		t.Helper()
		b, _ := os.ReadFile(module)
		m, _ := os.ReadFile(manifest)
		if !bytes.Equal(b, previous) || !bytes.Equal(m, previousManifest) {
			t.Fatal("previous pair changed")
		}
	}
	t.Run("force-required-and-success", func(t *testing.T) {
		run(1, source, "build", "--output", module, "--format", "json")
		pairUnchanged()
		run(0, source, "build", "--output", module, "--force", "--offline", "--format", "json")
		pairUnchanged()
	})
	t.Run("compiler-failure-test-build-does-not-run-old-module", func(t *testing.T) {
		write(filepath.Join(source, "broken.go"), "package main\n invalid compiler input")
		_, raw, diag := test(1, "{}", "--build", "--force", "--offline")
		if strings.Contains(diag, "BUSINESS_INIT") || strings.Contains(raw, "invalid compiler") {
			t.Fatal(raw, diag)
		}
		pairUnchanged()
		os.Remove(filepath.Join(source, "broken.go"))
	})
	t.Run("expected-sha-keeps-pair-and-config", func(t *testing.T) {
		bad := strings.Replace(codeConfig, "module: ./module.wasm", "module: ./module.wasm, sha256: "+strings.Repeat("a", 64), 1)
		write(config, bad)
		run(1, source, "build", "--config", config, "--capability", "run", "--force", "--offline", "--format", "json")
		pairUnchanged()
		after, _ := os.ReadFile(config)
		if string(after) != bad {
			t.Fatal("expected digest changed")
		}
		write(config, codeConfig)
	})
	t.Run("optional-and-mismatching-manifest", func(t *testing.T) {
		write(manifest, `{"sha256":"bad","abi":"bad","compiler_version":"go1.26.9","dependency_hash":"bad","required_imports":[]}`)
		run(1, source, "validate", "--module", module, "--format", "json")
		os.Remove(manifest)
		run(0, source, "validate", "--module", module, "--format", "json")
		write(manifest, string(previousManifest))
	})
	t.Run("default-null-int64-files-and-escaped-diagnostics", func(t *testing.T) {
		v, raw, diag := test(0, `{"nullable":null}`, "--log-level", "debug")
		data := v["data"].(map[string]any)
		if !strings.Contains(raw, "9007199254740993") || data["nullable"] != nil || len(v["files"].([]any)) != 2 || strings.Contains(raw, "PRIVATE_") || !strings.Contains(diag, "PRIVATE_DEBUG") || strings.ContainsRune(diag, '\x1b') {
			t.Fatal(raw, diag)
		}
		for _, f := range v["files"].([]any) {
			m := f.(map[string]any)
			if m["sha256"] != codeHash([]byte("hello")) || m["size_bytes"] != float64(5) || m["saved_path"] != nil || m["uri"] != nil {
				t.Fatal(m)
			}
		}
	})
	t.Run("log-level-does-not-change-results", func(t *testing.T) {
		a, _, ad := test(0, `{"scenario":"zero"}`, "--log-level", "debug")
		b, _, bd := test(0, `{"scenario":"zero"}`, "--log-level", "error")
		delete(a, "request_id")
		delete(b, "request_id")
		ab, _ := json.Marshal(a)
		bb, _ := json.Marshal(b)
		if !bytes.Equal(ab, bb) || !strings.Contains(ad, "PRIVATE_DEBUG") || strings.Contains(bd, "PRIVATE_DEBUG") || strings.Contains(bd, "BUSINESS_ENTER") || !strings.Contains(bd, "PRIVATE_ERROR") {
			t.Fatal(a, b, ad, bd)
		}
	})
	t.Run("input-prevents-business", func(t *testing.T) {
		// Contract validation rejects before the worker; an int32 overflow passes
		// the integer contract and fails in the SDK decoder before the handler.
		for params, code := range map[string]string{`[]`: "VALIDATION_FAILED", `{"n":"bad"}`: "VALIDATION_FAILED", `{"n":null}`: "VALIDATION_FAILED", `{"unknown":1}`: "VALIDATION_FAILED", `{"n":1,"n":2}`: "VALIDATION_FAILED", `{"small":2147483648}`: "EXECUTION_FAILED"} {
			v, raw, diag := test(1, params)
			failure, _ := v["error"].(map[string]any)
			if strings.Contains(diag, "BUSINESS_ENTER") || failure == nil || failure["code"] != code {
				t.Fatal(params, raw, diag)
			}
		}
	})
	t.Run("public-general-limit-error-safe", func(t *testing.T) {
		for scenario, want := range map[string]string{"public": "APP_DECLINED", "invalid-public": "EXECUTION_FAILED", "general": "EXECUTION_FAILED", "limit": "LIMIT_EXCEEDED"} {
			v, raw, _ := test(1, fmt.Sprintf(`{"scenario":%q}`, scenario))
			if v["error"].(map[string]any)["code"] != want || strings.Contains(raw, "PRIVATE_") || v["data"] != nil || v["files"] != nil {
				t.Fatal(raw)
			}
		}
	})
	t.Run("params-file-and-local-input-array-spaces-equals-mime", func(t *testing.T) {
		local := filepath.Join(tmp, "input = unknown.ext")
		write(local, "12345678")
		params := filepath.Join(tmp, "params.json")
		write(params, `{"scenario":"input","documents":[null,null]}`)
		v, _, _ := test(0, "", "--params-file", params, "--input-file", "/documents/0="+local, "--input-file", "/documents/1="+local, "--input-mime", "/documents/1=text/plain", "--output-dir", outDir)
		files := v["files"].([]any)
		if len(files) != 2 {
			t.Fatal(v)
		}
		for i, f := range files {
			m := f.(map[string]any)
			b, e := os.ReadFile(m["saved_path"].(string))
			if e != nil || string(b) != "12345678" || m["sha256"] != codeHash(b) || !strings.HasSuffix(m["saved_path"].(string), fmt.Sprintf("%04d.ext", i+1)) {
				t.Fatal(m, e)
			}
		}
		if files[0].(map[string]any)["mime_type"] != "application/octet-stream" || files[1].(map[string]any)["mime_type"] != "text/plain" {
			t.Fatal(files)
		}
	})
	t.Run("inline-equivalent-and-strict-base64", func(t *testing.T) {
		v, _, _ := test(0, `{"scenario":"input","documents":[{"name":"input.bin","mime_type":"application/octet-stream","data":"MTIzNDU2Nzg="}]}`)
		if v["files"].([]any)[0].(map[string]any)["sha256"] != codeHash([]byte("12345678")) {
			t.Fatal(v)
		}
		for _, data := range []string{"MTIz\n", "Zh==", "!!!!", "MTIzNDU2Nzg5"} {
			v, _, diag := test(1, fmt.Sprintf(`{"scenario":"input","documents":[{"name":"a","mime_type":"text/plain","data":%q}]}`, data))
			if strings.Contains(diag, "BUSINESS_ENTER") {
				t.Fatal(v, diag)
			}
		}
	})
	t.Run("bad-file-bindings-before-business", func(t *testing.T) {
		p := filepath.Join(tmp, "small.txt")
		write(p, "a")
		for _, extras := range [][]string{{"--input-file", "/documents/0=" + p, "--input-file", "/documents/0=" + p}, {"--input-file", "/documents/1=" + p}, {"--input-file", "/documents/0=" + tmp}, {"--input-file", "/documents/0=" + p, "--input-mime", "/documents/0=bad"}, {"--input-file", "/documents/0=" + p, "--input-mime", "/missing=text/plain"}, {"--input-file", "/documents/0=" + filepath.Join(tmp, "missing")}} {
			_, _, diag := test(1, `{"scenario":"input","documents":[null]}`, extras...)
			if strings.Contains(diag, "BUSINESS_ENTER") {
				t.Fatal(extras, diag)
			}
		}
	})

	t.Run("strict-local-input-and-params-paths-before-business", func(t *testing.T) {
		inputs := filepath.Join(tmp, "strict-inputs")
		if e := os.Mkdir(inputs, 0700); e != nil {
			t.Fatal(e)
		}
		cases := codeUnsafeInputs(t, inputs)
		for name, path := range cases {
			t.Run(name, func(t *testing.T) {
				destination := filepath.Join(inputs, "saved-"+name)
				if e := os.Mkdir(destination, 0700); e != nil {
					t.Fatal(e)
				}
				v, raw, diag := test(1, `{"scenario":"input","documents":[null]}`, "--input-file", "/documents/0="+path, "--output-dir", destination)
				if strings.Contains(diag, "BUSINESS_ENTER") || v["error"].(map[string]any)["code"] != "VALIDATION_FAILED" || strings.Contains(raw, path) {
					t.Fatal(v, raw, diag)
				}
				entries, e := os.ReadDir(destination)
				if e != nil || len(entries) != 0 {
					t.Fatal("rejected input leaked output/temp", entries, e)
				}
				v, raw, diag = test(1, "", "--params-file", path, "--output-dir", destination)
				if strings.Contains(diag, "BUSINESS_ENTER") || v["error"].(map[string]any)["code"] != "VALIDATION_FAILED" || strings.Contains(raw, path) {
					t.Fatal(v, raw, diag)
				}
				entries, e = os.ReadDir(destination)
				if e != nil || len(entries) != 0 {
					t.Fatal("rejected params leaked output/temp", entries, e)
				}
			})
		}
		v, _, diag := test(0, `{"scenario":"input","documents":[null]}`, "--input-file", "/documents/0="+filepath.Join(inputs, "regular.txt"))
		if !strings.Contains(diag, "BUSINESS_ENTER") || v["files"].([]any)[0].(map[string]any)["sha256"] != codeHash([]byte("valid")) {
			t.Fatal(v, diag)
		}
	})
	t.Run("output-directory-invalid-before-business-and-zero-no-final", func(t *testing.T) {
		_, _, diag := test(1, `{}`, "--output-dir", filepath.Join(tmp, "missing"))
		if strings.Contains(diag, "BUSINESS_ENTER") {
			t.Fatal(diag)
		}
		v, _, _ := test(0, `{"scenario":"zero"}`, "--output-dir", outDir)
		if _, e := os.Stat(filepath.Join(outDir, v["request_id"].(string))); !os.IsNotExist(e) {
			t.Fatal(e)
		}
	})
	t.Run("same-name-files-generated-names-all-delivery", func(t *testing.T) {
		for _, delivery := range []string{"reference", "inline"} {
			write(config, strings.Replace(codeConfig, "files: {}", "files: {delivery: {rest: reference, mcp: "+delivery+"}}", 1))
			v, _, _ := test(0, `{}`, "--output-dir", outDir)
			if len(v["files"].([]any)) != 2 {
				t.Fatal(v)
			}
			for i, f := range v["files"].([]any) {
				m := f.(map[string]any)
				if !strings.HasSuffix(m["saved_path"].(string), fmt.Sprintf("%04d.txt", i+1)) {
					t.Fatal(m)
				}
			}
		}
		direct := strings.Replace(codeConfig, "data: {}, files: {}", "files: {delivery: {rest: direct, mcp: inline}}", 1)
		direct = strings.Replace(direct, codeRunPolicy, "    policy: {timeout: 60s, max_output_files: 1}", 1)
		write(config, direct)
		v, _, _ := test(0, `{"scenario":"zero"}`, "--output-dir", outDir)
		if len(v["files"].([]any)) != 0 {
			t.Fatal(v)
		}
		write(config, codeConfig)
	})

	t.Run("generated-csv-and-reader-tail-failure", func(t *testing.T) {
		csv := strings.Replace(codeConfig, "output: {data: {}, files: {}}", "output: {items: {properties: {label: {type: string}, n: {type: integer}}}, data: {}, files: {format: csv, filename: report.csv}}", 1)
		write(config, csv)
		v, _, _ := test(0, `{"scenario":"rows"}`, "--output-dir", outDir)
		files := v["files"].([]any)
		if len(files) != 1 {
			t.Fatal(v)
		}
		b, e := os.ReadFile(files[0].(map[string]any)["saved_path"].(string))
		if e != nil || string(b) != "label,n\r\n'=1+1,9007199254740993\r\n" {
			t.Fatal(string(b), e)
		}
		write(config, codeConfig)
		v, raw, _ := test(1, `{"scenario":"late-reader"}`, "--output-dir", outDir)
		if v["files"] != nil || strings.Contains(raw, "PRIVATE_LATE_READER") {
			t.Fatal(v)
		}
		if _, e := os.Stat(filepath.Join(outDir, v["request_id"].(string))); !os.IsNotExist(e) {
			t.Fatal("partial output published", e)
		}
	})
	t.Run("source-boundaries-offline-import-rejection-preserve-pair", func(t *testing.T) {
		for name, body := range map[string]string{"nonmain": "package other\nfunc F(){}", "missing-main": "package main\nfunc F(){}", "offline-missing": "package main\nimport _ \"missing.invalid/dependency\"\nfunc main(){}", "unsupported-import": "package main\n//go:wasmimport forbidden call\nfunc call()\nfunc main(){call()}"} {
			d := filepath.Join(tmp, name)
			os.Mkdir(d, 0700)
			mod := "module " + name + "\n\ngo 1.26.9\n"
			if name == "offline-missing" {
				mod += "require missing.invalid/dependency v1.0.0\n"
			}
			write(filepath.Join(d, "go.mod"), mod)
			write(filepath.Join(d, "entry.go"), body)
			run(1, source, "build", "--source", d, "--output", module, "--force", "--offline", "--format", "json")
			pairUnchanged()
		}
		run(1, source, "build", "--source", filepath.Join(source, "entry.go"), "--output", module, "--force", "--format", "json")
		pairUnchanged()
	})
	t.Run("required-extension-mime-empty-byte-count-boundaries", func(t *testing.T) {
		required := strings.Replace(codeConfig, "scenario: {type: string, default: files}", "scenario: {type: string}", 1)
		required = strings.Replace(required, "    output:", "      required: [scenario]\n    output:", 1)
		write(config, required)
		_, _, diag := test(1, `{}`)
		if strings.Contains(diag, "BUSINESS_ENTER") {
			t.Fatal(diag)
		}
		limited := strings.Replace(codeConfig, "type: file, max_bytes: 8", "type: file, max_bytes: 8, extensions: [txt], mime_types: [text/plain], allow_empty: false", 1)
		write(config, limited)
		test(0, `{"scenario":"input","documents":[{"name":"a.txt","mime_type":"text/plain","data":"Zg=="}]}`)
		for _, params := range []string{`{"scenario":"input","documents":[{"name":"a.csv","mime_type":"text/plain","data":"Zg=="}]}`, `{"scenario":"input","documents":[{"name":"a.txt","mime_type":"application/octet-stream","data":"Zg=="}]}`, `{"scenario":"input","documents":[{"name":"a.txt","mime_type":"text/plain","data":""}]}`, `{"scenario":"input","documents":[{"name":"a.txt","mime_type":"text/plain","data":"MTIzNDU2Nzg5"}]}`} {
			_, _, diag := test(1, params)
			if strings.Contains(diag, "BUSINESS_ENTER") {
				t.Fatal(diag)
			}
		}
		write(config, strings.Replace(codeConfig, codeRunPolicy, "    policy: {timeout: 60s, max_input_files: 1}", 1))
		test(1, `{"scenario":"input","documents":[{"name":"a","mime_type":"text/plain","data":"Zg=="},{"name":"b","mime_type":"text/plain","data":"Zg=="}]}`)
		write(config, codeConfig)
	})
	t.Run("timeout-and-output-limit", func(t *testing.T) {
		write(config, strings.Replace(codeConfig, codeRunPolicy, "    policy: {timeout: 2s}", 1))
		v, _, _ := test(1, `{"scenario":"timeout"}`)
		if v["error"].(map[string]any)["code"] != "EXECUTION_TIMEOUT" {
			t.Fatal(v)
		}
		write(config, strings.Replace(codeConfig, codeRunPolicy, "    policy: {timeout: 60s, max_output_file_bytes: 4}", 1))
		v, _, _ = test(1, `{}`)
		if v["error"].(map[string]any)["code"] != "LIMIT_EXCEEDED" {
			t.Fatal(v)
		}
		write(config, codeConfig)
	})
	t.Run("stdout-failure-preserves-completed-output", func(t *testing.T) {
		before, _ := os.ReadDir(outDir)
		r, w, e := os.Pipe()
		if e != nil {
			t.Fatal(e)
		}
		r.Close()
		cmd := exec.Command(bin, "code", "test", "--config", config, "--capability", "run", "--format", "json", "--output-dir", outDir)
		cmd.Dir = source
		cmd.Env = env
		cmd.Stdout = w
		var diag bytes.Buffer
		cmd.Stderr = &diag
		e = cmd.Run()
		w.Close()
		ee, ok := e.(*exec.ExitError)
		if !ok || ee.ExitCode() != 1 || !strings.Contains(diag.String(), "saved output remains:") {
			t.Fatal(e, diag.String())
		}
		after, _ := os.ReadDir(outDir)
		if len(after) != len(before)+1 {
			t.Fatal("completed output removed", before, after)
		}
	})

	t.Run("forced-termination-is-unfinished-and-manually-removable", func(t *testing.T) {
		isolated := filepath.Join(tmp, "forced")
		os.Mkdir(isolated, 0700)
		cmd := exec.Command(bin, "code", "test", "--config", config, "--capability", "run", "--params", `{"scenario":"timeout"}`, "--output-dir", isolated, "--format", "json")
		cmd.Dir = source
		spool := filepath.Join(tmp, "forced-spool")
		os.Mkdir(spool, 0700)
		cmd.Env = append(env, "TMPDIR="+spool)
		cmd.Stdout = io.Discard
		stderr, e := cmd.StderrPipe()
		if e != nil {
			t.Fatal(e)
		}
		if e = cmd.Start(); e != nil {
			t.Fatal(e)
		}
		scanner := bufio.NewScanner(stderr)
		entered := false
		for scanner.Scan() {
			if strings.Contains(scanner.Text(), "BUSINESS_ENTER") {
				entered = true
				break
			}
		}
		if !entered {
			cmd.Process.Kill()
			cmd.Wait()
			t.Fatal("business marker missing")
		}
		cmd.Process.Kill()
		cmd.Wait()
		entries, e := os.ReadDir(isolated)
		if e != nil || len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), ".onprest-code-") {
			t.Fatal("unfinished output indistinguishable", entries, e)
		}
		if e = os.RemoveAll(filepath.Join(isolated, entries[0].Name())); e != nil {
			t.Fatal(e)
		}
	})
	t.Run("successful-test-build-text-exit-and-source-unchanged", func(t *testing.T) {
		run(0, source, "test", "--build", "--force", "--offline", "--config", config, "--capability", "run", "--params", `{"scenario":"zero"}`)
		b, _ := os.ReadFile(filepath.Join(source, "entry.go"))
		if string(b) != codeFixture {
			t.Fatal("source changed")
		}
		if _, e := os.Stat(filepath.Join(source, "go.sum")); !os.IsNotExist(e) {
			t.Fatal("go.sum created", e)
		}
		entries, _ := os.ReadDir(configDir)
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".onprest-") {
				t.Fatal("build temp leaked", e.Name())
			}
		}
		entries, _ = os.ReadDir(outDir)
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".onprest-") {
				t.Fatal("output temp leaked", e.Name())
			}
		}
	})
}
