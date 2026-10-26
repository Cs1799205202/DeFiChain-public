package chain

// Framework file: caps a miner's hashrate.

import (
	"context"
	"sync"
	"time"
)

// Throttle limits how many hashes per second Seal may compute. Without it every miner
// would run one CPU core flat out, and all miners on one laptop would have about the same
// power. With it you can give miners different shares of the network's hashrate (e.g. one
// miner with 60% to try a 51% attack) and keep the fans quiet.
//
// A nil *Throttle means no limit. Seal calls Wait after every batch of hashes.
type Throttle struct {
	mu    sync.Mutex
	rate  float64   // hashes per second, 0 = unlimited
	start time.Time // virtual clock: when the hashes counted so far "should" have started
	count float64
}

// NewThrottle limits to rate hashes per second (0 = unlimited).
func NewThrottle(rate float64) *Throttle { return &Throttle{rate: rate} }

// Rate is the current limit (0 = unlimited).
func (t *Throttle) Rate() float64 {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.rate
}

// SetRate changes the limit; it takes effect immediately.
func (t *Throttle) SetRate(rate float64) {
	t.mu.Lock()
	t.rate, t.count = rate, 0
	t.mu.Unlock()
}

// Wait accounts for n more hashes and sleeps until computing them was allowed. It
// returns ctx.Err() if ctx is cancelled first, so Seal can also use it to notice that it
// should stop.
func (t *Throttle) Wait(ctx context.Context, n int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if t == nil {
		return nil
	}
	t.mu.Lock()
	if t.rate <= 0 {
		t.mu.Unlock()
		return nil
	}
	now := time.Now()
	// After an idle period (no mining), start a fresh budget instead of letting the
	// miner burst through all the hashes it "saved".
	if t.count == 0 || now.Sub(t.start) > time.Duration(float64(time.Second)*t.count/t.rate)+time.Second {
		t.start, t.count = now, 0
	}
	t.count += float64(n)
	due := t.start.Add(time.Duration(float64(time.Second) * t.count / t.rate))
	t.mu.Unlock()
	if wait := time.Until(due); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	return nil
}
