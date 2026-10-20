package agent

import (
	"context"
	"github.com/viewlegacy/onprest/internal/protocol"
	"sync"
	"time"
)

type controlBucket struct {
	tokens      float64
	rate        float64
	burst       float64
	active, max int64
	at, sampled time.Time
}
type controlAdmission struct {
	mu                      sync.Mutex
	buckets                 map[string]*controlBucket
	bytes                   controlBucket
	transfers, maxTransfers int64
}

func newControlAdmission(runtime map[string]any) *controlAdmission {
	c := &controlAdmission{buckets: map[string]*controlBucket{}}
	m := runtime["control_limits"].(map[string]any)
	now := time.Now()
	for _, kind := range []string{"read", "write", "cancel", "transfer"} {
		v := m[kind].(map[string]any)
		per, _ := time.ParseDuration(v["per"].(string))
		burst := float64(v["burst"].(int64))
		c.buckets[kind] = &controlBucket{tokens: burst, burst: burst, rate: float64(v["requests"].(int64)) / per.Seconds(), max: v["max_concurrent"].(int64), at: now}
	}
	burst := float64(m["burst_bytes"].(int64))
	c.bytes = controlBucket{tokens: burst, burst: burst, rate: float64(m["bytes_per_second"].(int64)), at: now}
	c.maxTransfers = min(m["max_concurrent_transfers"].(int64), int64(protocol.ConnectionWindow/protocol.TransferWindow))
	return c
}
func (b *controlBucket) refill(now time.Time) {
	b.tokens += now.Sub(b.at).Seconds() * b.rate
	if b.tokens > b.burst {
		b.tokens = b.burst
	}
	b.at = now
}
func (c *controlAdmission) admit(kind string) (func(), string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.buckets[kind]
	now := time.Now()
	b.refill(now)
	code := ""
	if b.tokens < 1 {
		code = "RATE_LIMITED"
	} else {
		b.tokens--
		if b.active >= b.max {
			code = "BUSY"
		}
	}
	if code != "" {
		sample := now.Sub(b.sampled) >= 10*time.Second
		if sample {
			b.sampled = now
		}
		return nil, code, sample
	}
	b.active++
	var once sync.Once
	return func() { once.Do(func() { c.mu.Lock(); b.active--; c.mu.Unlock() }) }, "", false
}
func (c *controlAdmission) transferSlot() (func(), bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.transfers >= c.maxTransfers {
		return nil, false
	}
	c.transfers++
	var once sync.Once
	return func() { once.Do(func() { c.mu.Lock(); c.transfers--; c.mu.Unlock() }) }, true
}
func (c *controlAdmission) consume(ctx context.Context, n int) error {
	for n > 0 {
		if e := ctx.Err(); e != nil {
			return e
		}
		c.mu.Lock()
		now := time.Now()
		c.bytes.refill(now)
		amount := n
		if float64(amount) > c.bytes.burst {
			amount = int(c.bytes.burst)
		}
		if c.bytes.tokens >= float64(amount) {
			c.bytes.tokens -= float64(amount)
			c.mu.Unlock()
			n -= amount
			continue
		}
		wait := time.Duration((float64(amount) - c.bytes.tokens) / c.bytes.rate * float64(time.Second))
		c.mu.Unlock()
		if wait < time.Millisecond {
			wait = time.Millisecond
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
	return nil
}
