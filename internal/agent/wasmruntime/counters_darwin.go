//go:build darwin

package wasmruntime

import (
	"context"
	"errors"
	"sync"

	"github.com/ebitengine/purego"
	"github.com/shirou/gopsutil/v4/process"
)

// Darwin libproc returns CPU in Mach ticks and RSS in bytes. Read both in one
// checked call. gopsutil's Darwin task-info calls ignore the native return value.
// Layout: SDK sys/resource.h rusage_info_v0. Conversion: mach_timebase_info.
type darwinUsage struct {
	UUID                                [16]byte
	User, System                        uint64
	Wakeups, Interrupts, Pageins, Wired uint64
	RSS, Footprint, Started, Exited     uint64
}

var darwinCounters struct {
	once  sync.Once
	read  func(int32, int32, *darwinUsage) int32
	scale float64
	err   error
}

func sampleCounters(ctx context.Context, p *process.Process) (uint64, float64, error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	darwinCounters.once.Do(func() {
		lib, err := purego.Dlopen("/usr/lib/libproc.dylib", purego.RTLD_NOW|purego.RTLD_LOCAL)
		if err != nil {
			darwinCounters.err = err
			return
		}
		symbol, err := purego.Dlsym(lib, "proc_pid_rusage")
		if err != nil {
			purego.Dlclose(lib)
			darwinCounters.err = err
			return
		}
		purego.RegisterFunc(&darwinCounters.read, symbol)
		system, err := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_NOW|purego.RTLD_LOCAL)
		if err != nil {
			purego.Dlclose(lib)
			darwinCounters.err = err
			return
		}
		symbol, err = purego.Dlsym(system, "mach_timebase_info")
		if err != nil {
			purego.Dlclose(lib)
			purego.Dlclose(system)
			darwinCounters.err = err
			return
		}
		var timebase func(*struct{ Numer, Denom uint32 }) int32
		purego.RegisterFunc(&timebase, symbol)
		info := struct{ Numer, Denom uint32 }{}
		if timebase(&info) != 0 || info.Numer == 0 || info.Denom == 0 {
			darwinCounters.err = errors.New("Darwin CPU timebase unavailable")
			return
		}
		darwinCounters.scale = float64(info.Numer) / float64(info.Denom) / 1e9
		// Keep the two native library references for the process lifetime: registered
		// functions must not outlive a dlclose, including simultaneous worker runs.
	})
	if darwinCounters.err != nil {
		return 0, 0, darwinCounters.err
	}
	var usage darwinUsage
	if darwinCounters.read(p.Pid, 0, &usage) != 0 || usage.RSS == 0 || usage.Started == 0 || usage.Exited != 0 {
		return 0, 0, errors.New("Darwin worker resource counters unavailable")
	}
	return usage.RSS, (float64(usage.User) + float64(usage.System)) * darwinCounters.scale, nil
}
