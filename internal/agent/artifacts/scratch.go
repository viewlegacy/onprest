package artifacts

import (
	"context"

	"github.com/viewlegacy/onprest/internal/contract"
)

// Scratch supplies request-local private stages. They share the existing disk
// quota and object table, but never acquire retention or a public descriptor.
// The consumer owns every stage through Finish and must Abort after closing its
// readers. Existing ephemeral recovery handles these ordinary unretained stages.
func (s *Store) Scratch() contract.ArtifactStager { return scratchStager{s} }

// Capacity is the immutable shared disk budget, not a process memory limit.
func (s *Store) Capacity() int64 { return s.options.QuotaBytes }

type scratchStager struct{ store *Store }

func (v scratchStager) Begin(ctx context.Context, name, media string) (contract.ArtifactStage, error) {
	s := v.store
	s.mu.Lock()
	defer s.mu.Unlock()
	// These objects cannot be retained or looked up through the public output
	// API. Their generated stage IDs still remain private and unique.
	return s.newStage(ctx, Owner{"scratch", "scratch", "scratch", "scratch", "scratch"}, "o", name, media)
}
