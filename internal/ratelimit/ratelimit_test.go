// Tests for the token bucket: throughput accuracy, no saved-up bursts, refunds on cancel, and oversized requests.
// Owner: Swayam Kanjilal (Choking & Experiments)

package ratelimit

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// send pushes total bytes through l in burst-sized chunks from writers
// goroutines and returns how long it took.
func send(t *testing.T, l *Limiter, total, writers int) time.Duration {
	t.Helper()
	start := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for sent := 0; sent < total/writers; sent += l.Burst() {
				if err := l.WaitN(context.Background(), l.Burst()); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	return time.Since(start)
}

func TestLimiter_Throughput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		rate    int64
		writers int
	}{
		{"one writer", 512 << 10, 1},
		{"four writers share one limit", 512 << 10, 4},
		{"high rate, large bucket", 64 << 20, 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			l := New(tt.rate)
			total := int(tt.rate) // one second's worth after the initial burst
			got := send(t, l, total+l.Burst(), tt.writers)
			if got < 900*time.Millisecond || got > 1300*time.Millisecond {
				t.Errorf("sent %d B at %d B/s in %v, want about 1s", total+l.Burst(), tt.rate, got)
			}
		})
	}
}

// An idle sender must not save up more than one bucket of tokens.
func TestLimiter_NoUnboundedAccumulation(t *testing.T) {
	t.Parallel()
	l := New(256 << 10) // burst is the 16 KiB minimum here
	time.Sleep(300 * time.Millisecond)
	got := send(t, l, 5*l.Burst(), 1) // 1 burst free, 4 paid: 64 KiB at 256 KiB/s
	if want := 250 * time.Millisecond; got < want-30*time.Millisecond {
		t.Errorf("took %v after idling, want about %v: tokens accumulated past the burst", got, want)
	}
}

func TestLimiter_CancelRefunds(t *testing.T) {
	t.Parallel()
	l := New(16 << 10) // 16 KiB/s, 16 KiB bucket
	if err := l.WaitN(context.Background(), l.Burst()); err != nil {
		t.Fatal(err) // empties the bucket
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := l.WaitN(ctx, l.Burst()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if d := time.Since(start); d > 300*time.Millisecond {
		t.Errorf("cancelled wait took %v", d)
	}
	// The cancelled reservation was refunded: after a full second the
	// bucket is full again, so a burst goes through at once.
	time.Sleep(time.Second)
	start = time.Now()
	if err := l.WaitN(context.Background(), l.Burst()); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Errorf("refunded tokens missing: waited %v", d)
	}
}

func TestLimiter_RejectsOversizedRequest(t *testing.T) {
	t.Parallel()
	l := New(1 << 20)
	if err := l.WaitN(context.Background(), l.Burst()+1); err == nil {
		t.Error("WaitN accepted more than the burst")
	}
}
