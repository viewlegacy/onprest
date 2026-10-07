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

// NewScratch reserves a known body length atomically with its private stage.
// Writes consume that reservation rather than counting the same bytes twice.
// Unknown lengths use zero and reserve each chunk before writing.
func (s *Store) NewScratch(ctx context.Context, name, media string, bytes int64) (*Stage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if bytes < 0 {
		return nil, fail("VALIDATION_FAILED", nil)
	}
	if err := s.reserveLocked(bytes); err != nil {
		return nil, err
	}
	stage, err := s.newStage(ctx, Owner{"scratch", "scratch", "scratch", "scratch", "scratch"}, "o", name, media)
	if err != nil {
		s.used -= bytes
		return nil, err
	}
	stage.reserved = bytes
	return stage, nil
}

type scratchStager struct{ store *Store }

func (v scratchStager) Begin(ctx context.Context, name, media string) (contract.ArtifactStage, error) {
	s := v.store
	s.mu.Lock()
	defer s.mu.Unlock()
	// These objects cannot be retained or looked up through the public output
	// API. Their generated stage IDs still remain private and unique.
	return s.newStage(ctx, Owner{"scratch", "scratch", "scratch", "scratch", "scratch"}, "o", name, media)
}
