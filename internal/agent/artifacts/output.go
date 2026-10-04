package artifacts

import (
	"context"
	"errors"
	"io/fs"
	"time"

	"github.com/viewlegacy/onprest/internal/contract"
)

type OutputLimits struct {
	MaxFileBytes, MaxTotalBytes, MaxFiles int64
	TTL                                   time.Duration
}
type Descriptor struct {
	ID string
	contract.ArtifactMetadata
	ExpiresAt time.Time
}

// OutputScope implements the existing P02/P05 private ArtifactStager. Retain is
// called only after the complete output has passed common publication checks.
type OutputScope struct {
	store     *Store
	owner     Owner
	limits    OutputLimits
	stages    []*Stage
	bytes     int64
	published bool
}

func (s *Store) Output(o Owner, l OutputLimits) (*OutputScope, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.available(); e != nil {
		return nil, e
	}
	if !o.valid() || l.MaxFileBytes < 1 || l.MaxTotalBytes < l.MaxFileBytes || l.MaxFiles < 1 || l.TTL <= 0 {
		return nil, fail("VALIDATION_FAILED", fs.ErrInvalid)
	}
	return &OutputScope{store: s, owner: o, limits: l}, nil
}
func (v *OutputScope) Begin(ctx context.Context, name, media string) (contract.ArtifactStage, error) {
	s := v.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if v.published {
		return nil, fail("CONFLICT", nil)
	}
	if int64(len(v.stages)) >= v.limits.MaxFiles {
		return nil, fail("LIMIT_EXCEEDED", nil)
	}
	st, e := s.newStage(ctx, v.owner, "o", name, media)
	if e != nil {
		return nil, e
	}
	st.scope = v
	v.stages = append(v.stages, st)
	return st, nil
}
func (v *OutputScope) Retain(files []contract.OutputArtifact) ([]Descriptor, error) {
	s := v.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.available(); e != nil {
		return nil, e
	}
	if v.published || len(files) != len(v.stages) {
		return nil, fail("OUTPUT_INVALID", nil)
	}
	for i, st := range v.stages {
		r, ok := files[i].Reader.(*Reader)
		if !ok || r.stage != st || r.closed || !st.ready || st.retired || files[i].ArtifactMetadata != st.metadata() {
			return nil, fail("OUTPUT_INVALID", nil)
		}
	}
	expiry := time.Now().Add(v.limits.TTL)
	out := make([]Descriptor, 0, len(files))
	for _, st := range v.stages {
		st.retained = true
		st.expires = expiry
		out = append(out, Descriptor{st.id, st.metadata(), expiry})
	}
	v.published = true
	return out, nil
}
func (s *Store) OpenOutput(ctx context.Context, o Owner, id string) (*Reader, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.available(); e != nil {
		return nil, e
	}
	st := s.stages[id]
	if st == nil || st.kind != "o" || !st.retained || st.owner != o || !time.Now().Before(st.expires) {
		return nil, fail("NOT_FOUND", nil)
	}
	if ctx == nil {
		return nil, fail("VALIDATION_FAILED", nil)
	}
	return st.openLocked(ctx)
}

// Sweep expires output and unexecuted input. It never reclaims executing input,
// and never evicts live output to make room for a new allocation.
func (s *Store) Sweep() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var errs []error
	now := time.Now()
	for _, r := range s.requests {
		if !r.started && (r.state == "receiving" || r.state == "ready" || r.state == "aborted") && (!now.Before(r.deadline) || r.ctx.Err() != nil) {
			r.cancel()
			r.state = "released"
			for _, st := range r.files {
				st.retired = true
				errs = append(errs, st.cleanupLocked())
			}
			r.releaseSlotLocked()
		}
	}
	for _, st := range s.stages {
		if st.retained && !now.Before(st.expires) {
			st.retired = true
			errs = append(errs, st.cleanupLocked())
		}
	}
	return safe(errors.Join(errs...))
}

// RevokeGeneration invalidates ownership on disconnect/re-authentication. The
// dispatcher stops executing workers, then calls ReleaseStopped for their input.
func (s *Store) RevokeGeneration(generation string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var errs []error
	for _, r := range s.requests {
		if r.owner.AuthGeneration == generation {
			r.cancel()
			if r.stopExecution != nil {
				r.stopExecution()
			}
			r.state = "aborted"
		}
	}
	for _, st := range s.stages {
		if st.kind == "o" && st.owner.AuthGeneration == generation {
			st.retired = true
			for reader := range st.readers {
				reader.cancel()
			}
			errs = append(errs, st.cleanupLocked())
		}
	}
	return safe(errors.Join(errs...))
}
