//go:build !darwin

package wasmruntime

import (
	"context"
	"errors"
	"github.com/shirou/gopsutil/v4/process"
)

func sampleCounters(ctx context.Context, p *process.Process) (uint64, float64, error) {
	memory, err := p.MemoryInfoWithContext(ctx)
	if err != nil {
		return 0, 0, err
	}
	cpu, err := p.TimesWithContext(ctx)
	if err != nil {
		return 0, 0, err
	}
	if memory.RSS == 0 {
		return 0, 0, errors.New("worker RSS counter unavailable")
	}
	return memory.RSS, cpu.User + cpu.System, nil
}
