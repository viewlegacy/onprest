// Command citest runs a deterministic partition of Go tests and retains timing
// evidence on success as well as failure. It is CI tooling, not a runtime tool.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

type testCase struct{ Name, File string }
type goPackage struct {
	Dir, ImportPath           string
	TestGoFiles, XTestGoFiles []string
}

func discover(pkg, tags string) ([]testCase, error) {
	args := []string{"list", "-json", "-tags=" + tags, pkg}
	raw, err := exec.Command("go", args...).Output()
	if err != nil {
		return nil, err
	}
	var p goPackage
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	var tests []testCase
	for _, name := range append(p.TestGoFiles, p.XTestGoFiles...) {
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(p.Dir, name), nil, 0)
		if err != nil {
			return nil, err
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Test") || fn.Name.Name == "TestMain" {
				continue
			}
			if fn.Type.Params.NumFields() != 1 {
				continue
			}
			ptr, ok := fn.Type.Params.List[0].Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			sel, ok := ptr.X.(*ast.SelectorExpr)
			if ok && sel.Sel.Name == "T" {
				tests = append(tests, testCase{fn.Name.Name, name})
			}
		}
	}
	sort.Slice(tests, func(i, j int) bool { return tests[i].Name < tests[j].Name })
	return tests, nil
}

func partition(tests []testCase, include, exclude, files *regexp.Regexp, shard, shards int) []string {
	var eligible []string
	for _, t := range tests {
		if include.MatchString(t.Name) && !exclude.MatchString(t.Name) && files.MatchString(t.File) {
			eligible = append(eligible, t.Name)
		}
	}
	var selected []string
	for i, name := range eligible {
		if i%shards == shard-1 {
			selected = append(selected, name)
		}
	}
	return selected
}

type event struct {
	Action, Package, Test, Output string
	Elapsed                       float64
}

func run(logPath string, args []string) int {
	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	log, err := os.Create(logPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer log.Close()
	cmd := exec.Command("go", args...)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	cmd.Stderr = io.MultiWriter(os.Stderr, log)
	start := time.Now()
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	var results []event
	var recent []string
	passed, skipped, failed := 0, 0, 0
	scanner := bufio.NewScanner(pipe)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		fmt.Fprintln(log, string(line))
		var e event
		if json.Unmarshal(line, &e) != nil {
			fmt.Println(string(line))
			continue
		}
		if e.Output != "" {
			recent = append(recent, e.Output)
			if len(recent) > 200 {
				recent = recent[1:]
			}
		}
		if e.Test != "" {
			switch e.Action {
			case "pass":
				passed++
				results = append(results, e)
			case "skip":
				skipped++
				fmt.Printf("SKIP %s %s\n", e.Package, e.Test)
			case "fail":
				failed++
				fmt.Printf("FAIL %s %s\n", e.Package, e.Test)
			}
		} else if e.Action == "pass" || e.Action == "fail" {
			fmt.Printf("%s %s %.2fs\n", e.Action, e.Package, e.Elapsed)
		}
	}
	if err := scanner.Err(); err != nil {
		_ = cmd.Process.Kill()
		fmt.Fprintln(os.Stderr, err)
	}
	err = cmd.Wait()
	sort.Slice(results, func(i, j int) bool { return results[i].Elapsed > results[j].Elapsed })
	var report strings.Builder
	fmt.Fprintf(&report, "Wall time: %s; passed: %d; failed: %d; skipped: %d\n\n| Test (parents include subtests) | Seconds |\n|---|---:|\n", time.Since(start).Round(time.Millisecond), passed, failed, skipped)
	for i, e := range results {
		if i == 25 {
			break
		}
		fmt.Fprintf(&report, "| %s.%s | %.3f |\n", e.Package, strings.ReplaceAll(e.Test, "|", `\|`), e.Elapsed)
	}
	fmt.Print(report.String())
	if writeErr := os.WriteFile(logPath+".md", []byte(report.String()), 0644); writeErr != nil {
		fmt.Fprintln(os.Stderr, writeErr)
		return 1
	}
	if summary := os.Getenv("GITHUB_STEP_SUMMARY"); summary != "" {
		f, e := os.OpenFile(summary, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0644)
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			return 1
		}
		_, e = f.WriteString(report.String())
		_ = f.Close()
		if e != nil {
			return 1
		}
	}
	if err != nil || scanner.Err() != nil || skipped != 0 || failed != 0 || passed == 0 {
		fmt.Fprintln(os.Stderr, "Test command failed or did not run the required tests; full log:", logPath)
		// Do not make a parent go test mistake the nested command's warning for
		// its own no-tests marker. The original text remains in the JSON log.
		output := strings.ReplaceAll(strings.Join(recent, ""), "testing: warning: no tests to run", "nested go test selected no tests")
		fmt.Fprintln(os.Stderr, output)
		return 1
	}
	return 0
}

func main() {
	pkg := flag.String("package", "", "one package to partition; omit to pass go test arguments after --")
	tags := flag.String("tags", "", "Go build tags")
	include := flag.String("include", "^Test", "included test names")
	exclude := flag.String("exclude", "a^", "excluded test names")
	files := flag.String("files", ".*", "included test source files")
	shard := flag.Int("shard", 1, "one-based shard")
	shards := flag.Int("shards", 1, "shard count")
	log := flag.String("log", "test-results/tests.jsonl", "retained JSON log")
	plan := flag.Bool("plan", false, "print selected names without executing tests")
	flag.Parse()
	if *shards < 1 || *shard < 1 || *shard > *shards {
		fmt.Fprintln(os.Stderr, "invalid shard")
		os.Exit(1)
	}
	args := []string{"test", "-json", "-count=1", "-timeout=8m", "-tags=" + *tags}
	if *pkg != "" {
		tests, err := discover(*pkg, *tags)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		names := partition(tests, regexp.MustCompile(*include), regexp.MustCompile(*exclude), regexp.MustCompile(*files), *shard, *shards)
		if len(names) == 0 {
			fmt.Fprintln(os.Stderr, "empty test selection")
			os.Exit(1)
		}
		fmt.Println("Selected tests:\n" + strings.Join(names, "\n"))
		if *plan {
			return
		}
		args = append(args, "-run=^("+strings.Join(names, "|")+")$", *pkg)
	}
	args = append(args, flag.Args()...)
	if *pkg == "" && len(flag.Args()) == 0 {
		fmt.Fprintln(os.Stderr, "test arguments are required")
		os.Exit(1)
	}
	os.Exit(run(*log, args))
}
