// Owner: Swayam Kanjilal (Choking & Experiments)

// Package ratelimit is a token bucket for limiting bytes per second.
package ratelimit

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Limiter refills Rate tokens (bytes) per second, up to Burst. It is safe for
// concurrent use: callers that must wait queue up in call order.
type Limiter struct {
	rate  float64 // bytes per second
	burst int

	mu     sync.Mutex
	tokens float64 // may go negative: tokens already promised to waiters
	last   time.Time
}

// New returns a limiter for bytesPerSec. The bucket holds 20 ms worth of
// tokens, at least 16 KiB, so throughput is not capped by timer resolution
// and an idle sender cannot save up more than a short burst.
func New(bytesPerSec int64) *Limiter {
	burst := max(16<<10, int(bytesPerSec/50))
	return &Limiter{rate: float64(bytesPerSec), burst: burst, tokens: float64(burst), last: time.Now()}
}

// Burst is the largest n WaitN accepts.
func (l *Limiter) Burst() int { return l.burst }

// WaitN blocks until n bytes may be sent, or ctx ends (then the tokens are
// returned and ctx's error is returned).
func (l *Limiter) WaitN(ctx context.Context, n int) error {
	if n > l.burst {
		return fmt.Errorf("ratelimit: %d bytes exceeds burst %d", n, l.burst)
	}
	l.mu.Lock()
	now := time.Now()
	l.tokens = min(float64(l.burst), l.tokens+now.Sub(l.last).Seconds()*l.rate)
	l.last = now
	l.tokens -= float64(n) // reserve now, so later callers wait behind us
	deficit := -l.tokens
	l.mu.Unlock()
	if deficit <= 0 {
		return nil
	}

	wait := time.Duration(deficit / l.rate * float64(time.Second))
	slog.Debug("rate limited", "bytes", n, "wait", wait)
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		l.mu.Lock()
		l.tokens += float64(n)
		l.mu.Unlock()
		return ctx.Err()
	}
}
