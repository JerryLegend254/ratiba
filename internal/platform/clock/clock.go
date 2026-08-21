// Package clock provides the single seam through which Ratiba reads the
// current time.
//
// Booking rules are time-dependent (a slot must be at least one hour away, an
// appointment must not be in the past), so tests that called time.Now would be
// non-deterministic and would fail differently depending on when CI ran. Every
// component that needs "now" takes a Clock instead.
package clock

import (
	"sync"
	"time"
)

// Clock reports the current instant. Implementations must be safe for
// concurrent use.
type Clock interface {
	// Now returns the current instant in UTC.
	Now() time.Time
}

// System is the production Clock, backed by the operating system.
type System struct{}

// Now returns the current instant, normalised to UTC. Normalising here means no
// downstream code can accidentally depend on the host machine's timezone.
func (System) Now() time.Time { return time.Now().UTC() }

// Fixed is a Clock whose value only changes when a test moves it. The zero
// value is not useful; construct it with NewFixed.
type Fixed struct {
	mu  sync.RWMutex
	now time.Time
}

// NewFixed returns a Fixed clock pinned to t (converted to UTC).
func NewFixed(t time.Time) *Fixed {
	return &Fixed{now: t.UTC()}
}

// Now returns the pinned instant.
func (f *Fixed) Now() time.Time {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.now
}

// Set repositions the clock.
func (f *Fixed) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = t.UTC()
}

// Advance moves the clock forward by d.
func (f *Fixed) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}
