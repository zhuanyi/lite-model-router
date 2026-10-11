// Package health tracks per-backend availability (PLAN §5).
//
// A Backend is a small state machine fed by two sources: an optional active
// prober (Run) and passive marks from proxied requests (Success / Failure).
// It does no I/O itself; the probe is a callback, and the clock is injectable
// so the state machine is unit-testable without sleeping.
package health

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"
)

// MaxBackoff caps the retry delay of an unhealthy backend.
const MaxBackoff = 5 * time.Minute

// minProbeGap keeps a misbehaving clock from turning the prober into a hot loop.
const minProbeGap = 10 * time.Millisecond

// Options configures a Backend. Zero values select production defaults.
type Options struct {
	Interval time.Duration    // probe interval and backoff base (default 10s)
	Now      func() time.Time // clock (default time.Now)
	Jitter   func() float64   // multiplier applied to delays, ~[0.9,1.1] (default random)
	Logger   *slog.Logger     // health transitions are logged here (default: discard)
}

// Backend is the health state of one upstream.
type Backend struct {
	name     string
	interval time.Duration
	now      func() time.Time
	jitter   func() float64
	log      *slog.Logger

	mu           sync.Mutex
	healthy      bool
	failures     int
	retryAt      time.Time // when an unhealthy backend becomes eligible again
	healthySince time.Time
	lastChange   time.Time
	lastErr      string
	lastProxyOK  time.Time // last successful proxied response
	probing      bool
	nextProbe    time.Time
	wake         chan struct{} // 1-slot: state changed, prober should reschedule
}

// New returns a Backend in the optimistic healthy state.
func New(name string, o Options) *Backend {
	if o.Interval <= 0 {
		o.Interval = 10 * time.Second
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Jitter == nil {
		o.Jitter = func() float64 { return 0.9 + 0.2*rand.Float64() }
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	t := o.Now()
	return &Backend{
		name: name, interval: o.Interval, now: o.Now, jitter: o.Jitter, log: o.Logger,
		healthy: true, healthySince: t, lastChange: t, wake: make(chan struct{}, 1),
	}
}

// Backoff returns min(interval * 2^(n-1), MaxBackoff), without jitter.
func (b *Backend) Backoff(n int) time.Duration {
	d := b.interval
	for i := 1; i < n && d < MaxBackoff; i++ {
		d *= 2
	}
	return min(d, MaxBackoff)
}

// Eligible reports whether the backend should be tried: it is healthy, or its
// retry time has passed (half-open).
func (b *Backend) Eligible() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.healthy || !b.now().Before(b.retryAt)
}

// Success records a proxied response with status < 500.
func (b *Backend) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	t := b.now()
	b.lastProxyOK = t
	b.up(t)
}

// Failure records a passive failure (connect error, first-byte timeout,
// 502/503/504). msg must already be free of URLs and secrets.
func (b *Backend) Failure(msg string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.down(msg, b.now())
}

// ProbeOK records a successful (2xx) probe.
func (b *Backend) ProbeOK() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.up(b.now())
}

// ProbeFail records a failed probe. While healthy it is ignored if a proxied
// request succeeded within the last interval (a saturated server may fail
// probes while serving fine). Once unhealthy it always counts, otherwise
// retryAt would never advance and the prober would spin.
func (b *Backend) ProbeFail(msg string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t := b.now()
	if b.healthy && t.Sub(b.lastProxyOK) < b.interval {
		return
	}
	b.down(msg, t)
}

// up marks the backend healthy. failures is deliberately not reset (anti-flap).
func (b *Backend) up(t time.Time) {
	if b.healthy {
		return
	}
	b.healthy, b.healthySince, b.lastChange = true, t, t
	b.log.Info("backend health", "backend", b.name, "healthy", true)
	b.signal()
}

// signal tells the prober its timer is stale. Caller holds mu.
func (b *Backend) signal() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

// down records a failure and schedules the next eligibility time.
func (b *Backend) down(msg string, t time.Time) {
	b.lastErr = msg
	switch {
	case b.healthy:
		// A backend that was stable for 2 intervals starts from a clean slate;
		// a flapping one keeps its failure count and so a growing backoff.
		if t.Sub(b.healthySince) >= 2*b.interval {
			b.failures = 0
		}
		b.failures++
		b.healthy, b.lastChange = false, t
		b.log.Warn("backend health", "backend", b.name, "healthy", false, "err", msg)
	case t.Before(b.retryAt):
		// Failure of a last-resort attempt or a concurrent half-open trial:
		// don't let request volume inflate the backoff.
		return
	default:
		b.failures++
	}
	b.retryAt = t.Add(time.Duration(float64(b.Backoff(b.failures)) * b.jitter()))
	b.signal()
}

// Snapshot is the /status view of a backend. It carries no URLs or secrets.
type Snapshot struct {
	Healthy             bool      `json:"healthy"`
	LastError           string    `json:"last_error"`
	LastChange          time.Time `json:"last_change"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	NextProbeIn         string    `json:"next_probe_in"` // "" when passive-only
}

// Snapshot returns the current state.
func (b *Backend) Snapshot() Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := Snapshot{Healthy: b.healthy, LastError: b.lastErr, LastChange: b.lastChange, ConsecutiveFailures: b.failures}
	if b.probing {
		s.NextProbeIn = max(b.nextProbe.Sub(b.now()), 0).Round(time.Millisecond).String()
	}
	return s
}

// Run is the active prober loop: probe every interval while healthy, or when
// retryAt passes while unhealthy. It returns when ctx is cancelled.
func (b *Backend) Run(ctx context.Context, probe func(context.Context) error) {
	b.mu.Lock()
	b.probing = true
	b.mu.Unlock()
	for {
		t := time.NewTimer(b.schedule())
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-b.wake: // state changed under us: recompute the schedule
			t.Stop()
			continue
		case <-t.C:
		}
		err := probe(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			b.ProbeFail(err.Error())
		} else {
			b.ProbeOK()
		}
	}
}

// schedule computes the delay until the next probe.
func (b *Backend) schedule() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	var d time.Duration
	if b.healthy {
		d = time.Duration(float64(b.interval) * b.jitter())
	} else {
		d = b.retryAt.Sub(b.now())
	}
	d = max(d, minProbeGap)
	b.nextProbe = b.now().Add(d)
	return d
}

// Order returns the candidates to try, in chain order: those eligible, or all
// of them if none is (a stale health view must never refuse a servable request).
func Order[T any](chain []T, eligible func(T) bool) []T {
	out := make([]T, 0, len(chain))
	for _, c := range chain {
		if eligible(c) {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		out = append(out, chain...)
	}
	return out
}
