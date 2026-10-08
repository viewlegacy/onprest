package wasmruntime

import (
	"errors"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/viewlegacy/onprest/internal/contract"
)

type Metadata struct {
	ABI                 string   `json:"abi"`
	MinimumAgentVersion string   `json:"minimum_agent_version"`
	Imports             []string `json:"imports"`
}

var releaseVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

func versionParts(v string) ([3]uint64, error) {
	var out [3]uint64
	if !releaseVersion.MatchString(v) {
		return out, errors.New("unknown Agent version")
	}
	for i, s := range strings.Split(v, ".") {
		n, e := strconv.ParseUint(s, 10, 64)
		if e != nil {
			return out, e
		}
		out[i] = n
	}
	return out, nil
}
func versionSupported(actual, minimum string) bool {
	if actual == "dev" {
		actual = CompatibilityVersion
	}
	a, e := versionParts(actual)
	if e != nil {
		return false
	}
	b, e := versionParts(minimum)
	if e != nil {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return true
}

var wasiPlumbing = map[string]bool{
	"args_get": true, "args_sizes_get": true, "environ_get": true, "environ_sizes_get": true,
	"clock_time_get": true, "clock_res_get": true, "random_get": true, "sched_yield": true,
	"proc_exit": true, "poll_oneoff": true, "fd_read": true, "fd_write": true, "fd_close": true,
	"fd_prestat_get": true, "fd_prestat_dir_name": true, "fd_fdstat_set_flags": true,
	"fd_seek": true, "fd_tell": true, "fd_fdstat_get": true, "fd_filestat_get": true, "fd_sync": true, "fd_datasync": true,
}

func validateCompiled(c wazero.CompiledModule, version string, wasi map[string]api.FunctionDefinition) (Metadata, error) {
	var m Metadata
	seenSection := false
	for _, section := range c.CustomSections() {
		if section.Name() != "onprest" {
			continue
		}
		if seenSection {
			return m, errors.New("duplicate onprest section")
		}
		seenSection = true
		v, e := contract.DecodeJSON(section.Data(), contract.Limits{MaxBytes: Chunk, MaxDepth: 4, MaxItems: 1024})
		if e != nil {
			return m, e
		}
		obj, ok := v.(map[string]any)
		if !ok || len(obj) != 3 {
			return m, errors.New("invalid onprest metadata")
		}
		m.ABI, _ = obj["abi"].(string)
		m.MinimumAgentVersion, _ = obj["minimum_agent_version"].(string)
		raw, ok := obj["imports"].([]any)
		if !ok {
			return m, errors.New("invalid declared imports")
		}
		m.Imports = make([]string, 0, len(raw))
		for _, v := range raw {
			s, ok := v.(string)
			if !ok {
				return m, errors.New("invalid declared import")
			}
			m.Imports = append(m.Imports, s)
		}
	}
	if m.ABI != ABI || !versionSupported(version, m.MinimumAgentVersion) {
		return m, errors.New("unsupported module ABI or Agent version")
	}
	if !sort.StringsAreSorted(m.Imports) {
		return m, errors.New("imports must be sorted")
	}
	for i, s := range m.Imports {
		if s == "" || i > 0 && s == m.Imports[i-1] {
			return m, errors.New("duplicate declared import")
		}
	}
	if len(c.ImportedMemories()) != 0 {
		return m, errors.New("imported memory is unsupported")
	}
	start := c.ExportedFunctions()["_start"]
	if start == nil || len(start.ParamTypes()) != 0 || len(start.ResultTypes()) != 0 || c.ExportedMemories()["memory"] == nil {
		return m, errors.New("WASI command exports required")
	}
	actual := []string{}
	for _, f := range c.ImportedFunctions() {
		module, name, ok := f.Import()
		if !ok {
			return m, errors.New("invalid import")
		}
		actual = append(actual, module+"."+name)
		switch module {
		case "wasi_snapshot_preview1":
			definition := wasi[name]
			if !wasiPlumbing[name] || definition == nil {
				return m, errors.New("business WASI import is unavailable")
			}
			if !reflect.DeepEqual(f.ParamTypes(), definition.ParamTypes()) || !reflect.DeepEqual(f.ResultTypes(), definition.ResultTypes()) {
				return m, errors.New("invalid WASI function signature")
			}
		case ABI:
			var p, r []api.ValueType
			switch name {
			case "call":
				p = []api.ValueType{api.ValueTypeI32, api.ValueTypeI32}
				r = []api.ValueType{api.ValueTypeI64}
			case "read", "write":
				p = []api.ValueType{api.ValueTypeI64, api.ValueTypeI32, api.ValueTypeI32}
				r = []api.ValueType{api.ValueTypeI32}
			case "close":
				p = []api.ValueType{api.ValueTypeI64}
				r = []api.ValueType{api.ValueTypeI32}
			default:
				return m, errors.New("unsupported host import")
			}
			if !reflect.DeepEqual(f.ParamTypes(), p) || !reflect.DeepEqual(f.ResultTypes(), r) {
				return m, errors.New("invalid host function signature")
			}
		default:
			return m, errors.New("unsupported module import")
		}
	}
	sort.Strings(actual)
	actual = compactImports(actual)
	if !reflect.DeepEqual(actual, m.Imports) {
		return m, errors.New("declared and actual imports differ")
	}
	return m, nil
}

func compactImports(in []string) []string {
	out := in[:0]
	for _, s := range in {
		if len(out) == 0 || out[len(out)-1] != s {
			out = append(out, s)
		}
	}
	return out
}
