package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"github.com/viewlegacy/onprest/internal/agent/artifacts"
	"github.com/viewlegacy/onprest/internal/contract"
	"io"
	"math"
	"path/filepath"
	"time"
)

func (r *Runner) initializeTransport() error {
	if r.versioned == nil {
		return nil
	}
	runtime := r.versioned.Runtime()
	r.controls = newControlAdmission(runtime)
	directory := runtime["spool_dir"].(string)
	if !filepath.IsAbs(directory) {
		base, e := filepath.Abs(filepath.Dir(r.cfg.CapabilityFile))
		if e != nil {
			return e
		}
		directory = filepath.Join(base, directory)
	}
	limits := runtime["input_limits"].(map[string]any)
	pending := limits["max_pending_requests"].(int64)
	recent := limits["max_recent_requests"].(int64)
	if pending > int64(math.MaxInt)/16 || recent > int64(math.MaxInt)/16-pending {
		return io.ErrShortBuffer
	}
	timeout, _ := time.ParseDuration(runtime["transfer_timeout"].(string))
	store, e := artifacts.Open(artifacts.Options{Directory: directory, QuotaBytes: runtime["spool_total_bytes"].(int64), MaxPending: int(pending), MaxObjects: int(pending*16 + recent*16), TransferTimeout: timeout})
	if e != nil {
		return e
	}
	r.store = store
	return nil
}
func randomTransportID() string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b[:])
}

type inputAccessKey struct{}
type inputAccess struct {
	request *artifacts.Request
	owner   artifacts.Owner
}

// OpenInput reads only an executing request's verified snapshot. Future
// providers share this accessor without receiving a spool directory or path.
func OpenInput(ctx context.Context, value contract.FileValue) (io.ReadCloser, error) {
	a, ok := ctx.Value(inputAccessKey{}).(inputAccess)
	if !ok {
		return nil, io.ErrClosedPipe
	}
	return a.request.Open(a.owner, value)
}
