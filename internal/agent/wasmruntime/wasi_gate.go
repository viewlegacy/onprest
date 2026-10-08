package wasmruntime

import (
	"context"
	"encoding/json"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// Preserve standard WASI plumbing while making unmetered file mutations fail
// closed. The delegate is invoked with the requesting module, never its host
// module, so stdio and descriptor routing retain standard WASI semantics.
type rawAllocation struct {
	active         bool
	offset, length int64
}

func instantiateWASI(ctx context.Context, r wazero.Runtime, rpc func(packet) packet, allocation *rawAllocation) (api.Module, error) {
	original := r.NewHostModuleBuilder("onprest_wasi_delegate")
	wasi_snapshot_preview1.NewFunctionExporter().ExportFunctions(original)
	native, e := original.Instantiate(ctx)
	if e != nil {
		return nil, e
	}
	b := r.NewHostModuleBuilder("wasi_snapshot_preview1")
	wasi_snapshot_preview1.NewFunctionExporter().ExportFunctions(b)
	unmetered := map[string]bool{"fd_advise": true, "path_link": true, "path_symlink": true, "path_readlink": true, "path_filestat_set_times": true, "fd_filestat_set_times": true}
	for name, d := range native.ExportedFunctionDefinitions() {
		if name == "fd_allocate" {
			delegate := d.GoFunction().(api.GoModuleFunction)
			b.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, m api.Module, stack []uint64) {
				if stack[1] > 1<<63-1 || stack[2] > 1<<63-1 || stack[1]+stack[2] < stack[1] || stack[1]+stack[2] > 1<<63-1 {
					stack[0] = wasiErrno(invalid)
					return
				}
				allocation.active = true
				allocation.offset = int64(stack[1])
				allocation.length = int64(stack[2])
				defer func() { allocation.active = false }()
				delegate.Call(ctx, m, stack)
			}), d.ParamTypes(), d.ResultTypes()).Export(name)
			continue
		}
		if !unmetered[name] {
			continue
		}
		method := "files.unsupported"
		b.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
			raw, _ := json.Marshal(map[string]any{"method": method, "args": map[string]any{}})
			v := rpc(packet{Op: "call", Data: raw})
			stack[0] = wasiErrno(v.Count)
		}), d.ParamTypes(), d.ResultTypes()).Export(name)
	}
	return b.Instantiate(ctx)
}

// WASI preview1 errno values differ from the runtime's experimental sys enum.
func wasiErrno(c int32) uint64 {
	switch c {
	case 0:
		return 0
	case invalid:
		return 28
	case denied:
		return 63
	case notFound:
		return 44
	case exists:
		return 20
	case limit:
		return 68
	case canceled:
		return 27
	case unsupported:
		return 52
	default:
		return 29
	}
}
