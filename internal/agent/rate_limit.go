package agent

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
)

const rateLimitRejectionLogInterval = time.Minute

const rateLimitedMessage = "agent rate limit exceeded"

type agentClock func() time.Time

type capabilityRateLimiter struct {
	mu sync.Mutex

	// Credit is measured in token-nanoseconds. One admitted request costs
	// policy.rate_limit.per nanoseconds, while every elapsed nanosecond adds
	// policy.rate_limit.requests credits. This preserves fractional tokens
	// without floating point arithmetic.
	credit   int64
	capacity int64
	cost     int64
	refill   int64
	last     time.Time

	lastRejectionLog time.Time
	rejectionLogged  bool
}

func (r RateLimitDef) lint() error {
	if r.Requests <= 0 {
		return errors.New("requests must be > 0")
	}
	if r.Per == "" {
		return errors.New("per is required")
	}
	per, err := time.ParseDuration(r.Per)
	if err != nil {
		return errors.New("per is invalid")
	}
	if per <= 0 {
		return errors.New("per must be > 0")
	}
	if r.Burst <= 0 {
		return errors.New("burst must be > 0")
	}
	if r.Burst > math.MaxInt64/int64(per) {
		return fmt.Errorf("burst and per are too large")
	}
	return nil
}

func newCapabilityRateLimiter(def RateLimitDef, now time.Time) *capabilityRateLimiter {
	per := int64(mustRateLimitDuration(def.Per))
	capacity := def.Burst * per // checked during capability lint
	return &capabilityRateLimiter{
		credit:   capacity,
		capacity: capacity,
		cost:     per,
		refill:   def.Requests,
		last:     now,
	}
}

func mustRateLimitDuration(raw string) time.Duration {
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		panic("invalid linted rate-limit duration")
	}
	return d
}

// take atomically admits one request and reports whether this rejection should
// be sampled to the bounded local detail log. Rejection sampling state lives in
// the same fixed per-capability object as the bucket, so hostile request IDs or
// caller identities cannot grow memory.
func (b *capabilityRateLimiter) take(now time.Time) (allowed, logRejection bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if elapsed := now.Sub(b.last); elapsed > 0 {
		deficit := b.capacity - b.credit
		if deficit > 0 {
			// Clamp before multiplying. If elapsed is below the ceiling needed
			// to fill the bucket, elapsed*refill is strictly below deficit and
			// therefore cannot overflow int64.
			fillAfter := deficit / b.refill
			if deficit%b.refill != 0 {
				fillAfter++
			}
			if int64(elapsed) >= fillAfter {
				b.credit = b.capacity
			} else {
				b.credit += int64(elapsed) * b.refill
			}
		}
		b.last = now
	}
	if b.credit >= b.cost {
		b.credit -= b.cost
		return true, false
	}
	if !b.rejectionLogged || now.Sub(b.lastRejectionLog) >= rateLimitRejectionLogInterval {
		b.lastRejectionLog = now
		b.rejectionLogged = true
		return false, true
	}
	return false, false
}
