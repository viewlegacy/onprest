package wasmruntime

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental/sysfs"
	"github.com/tetratelabs/wazero/sys"
	"github.com/viewlegacy/onprest/internal/agent/filesystem"
	"path/filepath"
	"strings"
)

// HandleWorker is checked before public CLI dispatch. Bare invocation cannot
// execute a module: only inherited pipes carrying the bootstrap token work.
func HandleWorker(args []string) (bool, int) {
	if len(args) == 0 || args[0] != workerCommand {
		return false, 0
	}
	in, out, e := workerPipes(args[1:])
	if e != nil {
		return true, 1
	}
	defer in.Close()
	defer out.Close()
	if e = worker(in, out); e != nil {
		fmt.Fprintln(os.Stderr, e)
		return true, 1
	}
	return true, 0
}
func worker(in, out *os.File) error {
	var b bootstrap
	if e := receive(in, &b); e != nil {
		return e
	}
	if len(b.Token) != 64 || b.ModuleSize < 1 || int64(b.ModuleSize) > b.Limits.ModuleBytes || b.ParamsSize < 0 || b.ParamsSize > 1<<20 {
		return errors.New("invalid worker bootstrap")
	}
	if _, e := b.Limits.rssLimit(); e != nil {
		return e
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Unix(0, b.Deadline))
	defer cancel()
	if e := setCPU(ctx); e != nil {
		return e
	}
	if e := send(out, packet{Op: "ready", Token: b.Token}); e != nil {
		return e
	}
	var goAhead packet
	if e := receive(in, &goAhead); e != nil || goAhead.Op != "go" || goAhead.Token != b.Token {
		return errors.New("invalid worker authorization")
	}
	module := make([]byte, b.ModuleSize)
	if _, e := io.ReadFull(in, module); e != nil {
		return e
	}
	params := make([]byte, b.ParamsSize)
	if _, e := io.ReadFull(in, params); e != nil {
		return e
	}
	cfg := wazero.NewRuntimeConfig().WithMemoryLimitPages(uint32(b.Limits.MemoryBytes / 65536)).WithCloseOnContextDone(true).WithCustomSections(true)
	runtime := wazero.NewRuntimeWithConfig(ctx, cfg)
	defer runtime.Close(context.Background())
	compiled, e := runtime.CompileModule(ctx, module)
	if e != nil {
		return e
	}
	defer compiled.Close(context.Background())
	var earlyRPC func(packet) packet
	allocation := &rawAllocation{}
	_, e = instantiateWASI(ctx, runtime, func(p packet) packet { return earlyRPC(p) }, allocation)
	if e != nil {
		return e
	}
	metadata, e := validateCompiled(compiled, b.Version, runtime.Module("wasi_snapshot_preview1").ExportedFunctionDefinitions())
	if e != nil {
		return e
	}
	if e = send(out, packet{Op: "compiled"}); e != nil {
		return e
	}
	if b.Validate {
		return workerDone(in, out, &metadata)
	}
	var mu sync.Mutex
	semaphore := make(chan struct{}, b.Limits.HostCalls)
	rpc := func(p packet) packet {
		select {
		case semaphore <- struct{}{}:
			defer func() { <-semaphore }()
		default:
			return packet{Count: limit}
		}
		mu.Lock()
		defer mu.Unlock()
		if ctx.Err() != nil {
			return packet{Count: canceled}
		}
		if e := send(out, p); e != nil {
			panic("worker host channel failed")
		}
		var response packet
		if e := receive(in, &response); e != nil || response.Op != "response" {
			panic("worker host response failed")
		}
		return response
	}
	earlyRPC = rpc
	host := runtime.NewHostModuleBuilder(ABI)
	host.NewFunctionBuilder().WithFunc(func(_ context.Context, m api.Module, ptr, n uint32) int64 {
		if n > Chunk {
			return int64(limit)
		}
		v, ok := m.Memory().Read(ptr, n)
		if !ok {
			return int64(invalid)
		}
		p := rpc(packet{Op: "call", Data: v})
		if p.Count < 0 {
			return int64(p.Count)
		}
		return p.Handle
	}).Export("call")
	host.NewFunctionBuilder().WithFunc(func(_ context.Context, m api.Module, id int64, ptr, n uint32) int32 {
		if n > Chunk {
			return limit
		}
		if _, ok := m.Memory().Read(ptr, n); !ok {
			return invalid
		}
		p := rpc(packet{Op: "read", Handle: id, Count: int32(n)})
		if p.Count < 0 {
			return p.Count
		}
		if p.Count > int32(n) || len(p.Data) != int(p.Count) || !m.Memory().Write(ptr, p.Data) {
			return invalid
		}
		return p.Count
	}).Export("read")
	host.NewFunctionBuilder().WithFunc(func(_ context.Context, m api.Module, id int64, ptr, n uint32) int32 {
		if n > Chunk {
			return limit
		}
		v, ok := m.Memory().Read(ptr, n)
		if !ok {
			return invalid
		}
		return rpc(packet{Op: "write", Handle: id, Data: v}).Count
	}).Export("write")
	host.NewFunctionBuilder().WithFunc(func(_ context.Context, id int64) int32 { return rpc(packet{Op: "close", Handle: id}).Count }).Export("close")
	if _, e = host.Instantiate(ctx); e != nil {
		return e
	}
	// No filesystem mount, host environment, argv, socket or business imports.
	config := wazero.NewModuleConfig().WithName("request").WithStdin(bytes.NewReader(params)).WithStdout(os.Stdout).WithStderr(os.Stderr).WithSysWalltime().WithSysNanotime().WithSysNanosleep().WithRandSource(rand.Reader)
	if b.Business {
		for k, v := range b.Environment {
			config = config.WithEnv(k, v)
		}
		mount := wazero.NewFSConfig().(sysfs.FSConfig).WithSysFSMount(&wasiFS{rpc: rpc, prefix: "/", allocation: allocation}, "/")
		if b.Base != "" {
			prefix := filepath.ToSlash(b.Base)
			if !strings.HasSuffix(prefix, "/") {
				prefix += "/"
			}
			mount = mount.(sysfs.FSConfig).WithSysFSMount(&wasiFS{rpc: rpc, prefix: prefix, allocation: allocation}, filesystem.WASIPath(b.Base))
			config = config.WithEnv("PWD", filesystem.WASIPath(b.Base))
		}
		config = config.WithFSConfig(mount)
	}
	_, e = runtime.InstantiateModule(ctx, compiled, config)
	var exit *sys.ExitError
	if e != nil && !(errors.As(e, &exit) && exit.ExitCode() == 0) {
		return e
	}
	return workerDone(in, out, nil)
}

// A normal worker stays alive until the parent has stopped OS monitoring. This
// prevents exited-process counters from masquerading as a monitoring failure.
func workerDone(in, out *os.File, metadata *Metadata) error {
	if err := send(out, packet{Op: "done", Metadata: metadata}); err != nil {
		return err
	}
	var ack packet
	if err := receive(in, &ack); err != nil || ack.Op != "finish" {
		return errors.New("worker completion not acknowledged")
	}
	return nil
}
