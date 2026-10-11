package health

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

func newTest(interval time.Duration) (*Backend, *fakeClock) {
	c := &fakeClock{t: time.Unix(1_000_000, 0)}
	return New("b", Options{Interval: interval, Now: c.Now, Jitter: func() float64 { return 1 }}), c
}

func retryIn(b *Backend, c *fakeClock) time.Duration { return b.retryAt.Sub(c.Now()) }

func TestStartsHealthy(t *testing.T) {
	b, _ := newTest(10 * time.Second)
	if !b.Eligible() || !b.Snapshot().Healthy {
		t.Fatal("must start optimistic")
	}
}

func TestBackoffGrowthAndCap(t *testing.T) {
	b, c := newTest(10 * time.Second)
	want := []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second,
		160 * time.Second, 5 * time.Minute, 5 * time.Minute}
	for i, w := range want {
		b.Failure("boom")
		if got := retryIn(b, c); got != w {
			t.Fatalf("failure %d: backoff %v, want %v", i+1, got, w)
		}
		if b.Eligible() {
			t.Fatalf("failure %d: eligible before retryAt", i+1)
		}
		c.Advance(w) // half-open trial that fails again
	}
	if f := b.Snapshot().ConsecutiveFailures; f != len(want) {
		t.Fatalf("failures = %d", f)
	}
}

func TestJitterApplied(t *testing.T) {
	b, c := newTest(10 * time.Second)
	b.jitter = func() float64 { return 1.1 }
	b.Failure("x")
	if got := retryIn(b, c); got != 11*time.Second {
		t.Fatalf("got %v", got)
	}
}

func TestHalfOpenEligibility(t *testing.T) {
	b, c := newTest(10 * time.Second)
	b.Failure("x")
	c.Advance(9 * time.Second)
	if b.Eligible() {
		t.Fatal("eligible too early")
	}
	c.Advance(time.Second)
	if !b.Eligible() {
		t.Fatal("must be eligible once retryAt passed")
	}
	if b.Snapshot().Healthy {
		t.Fatal("half-open is still reported unhealthy until a success")
	}
	b.Success()
	if !b.Snapshot().Healthy {
		t.Fatal("success must restore health")
	}
}

func TestFlappingKeepsGrowingStableDecays(t *testing.T) {
	b, c := newTest(10 * time.Second)
	b.Failure("x") // failures=1
	c.Advance(10 * time.Second)
	b.ProbeOK() // up, healthySince=now
	c.Advance(5 * time.Second)
	b.Failure("x") // up for 5s < 2*interval: keeps growing
	if f := b.Snapshot().ConsecutiveFailures; f != 2 {
		t.Fatalf("flapping: failures=%d, want 2", f)
	}
	if got := retryIn(b, c); got != 20*time.Second {
		t.Fatalf("flapping backoff = %v", got)
	}

	c.Advance(20 * time.Second)
	b.ProbeOK()
	c.Advance(20 * time.Second) // stable for 2 intervals
	b.Failure("x")
	if f := b.Snapshot().ConsecutiveFailures; f != 1 {
		t.Fatalf("stable: failures=%d, want restart at 1", f)
	}
	if got := retryIn(b, c); got != 10*time.Second {
		t.Fatalf("stable backoff = %v", got)
	}
}

func TestSuccessDoesNotResetFailures(t *testing.T) {
	b, _ := newTest(10 * time.Second)
	b.Failure("x")
	b.Success()
	if f := b.Snapshot().ConsecutiveFailures; f != 1 {
		t.Fatalf("failures reset by single success: %d", f)
	}
}

func TestLastResortFailuresDontEscalate(t *testing.T) {
	b, c := newTest(10 * time.Second)
	b.Failure("x")
	for range 20 { // many requests hammering the last-resort backend
		b.Failure("x")
	}
	if f := b.Snapshot().ConsecutiveFailures; f != 1 {
		t.Fatalf("failures = %d, want 1", f)
	}
	if got := retryIn(b, c); got != 10*time.Second {
		t.Fatalf("retry pushed out: %v", got)
	}
}

func TestProbeFailureIgnoredAfterRecentProxySuccess(t *testing.T) {
	b, c := newTest(10 * time.Second)
	b.Success()
	c.Advance(5 * time.Second)
	b.ProbeFail("probe timeout")
	if !b.Snapshot().Healthy {
		t.Fatal("probe failure within one interval of a proxied success must be ignored")
	}
	c.Advance(6 * time.Second) // now > interval since last success
	b.ProbeFail("probe timeout")
	if b.Snapshot().Healthy {
		t.Fatal("probe failure must count once the proxied success is stale")
	}
	if s := b.Snapshot(); s.LastError != "probe timeout" {
		t.Fatalf("last error = %q", s.LastError)
	}
}

func TestProbeFailCountsOnceUnhealthy(t *testing.T) {
	b, c := newTest(10 * time.Second)
	b.Success()
	b.Failure("x") // demoted although a proxied success just happened
	c.Advance(10 * time.Second)
	b.ProbeFail("probe status 503") // fires at retryAt: must advance retryAt
	if got := retryIn(b, c); got != 20*time.Second {
		t.Fatalf("retryAt not advanced: %v", got)
	}
}

func TestOrder(t *testing.T) {
	type cand struct {
		n  string
		ok bool
	}
	elig := func(c cand) bool { return c.ok }
	names := func(cs []cand) []string {
		var out []string
		for _, c := range cs {
			out = append(out, c.n)
		}
		return out
	}
	got := names(Order([]cand{{"a", false}, {"b", true}, {"c", true}}, elig))
	if !slices.Equal(got, []string{"b", "c"}) {
		t.Fatalf("eligible only, chain order: %v", got)
	}
	got = names(Order([]cand{{"a", false}, {"b", false}, {"c", false}}, elig))
	if !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Fatalf("all-unhealthy fallback must keep chain order: %v", got)
	}
}

func TestSnapshotNextProbe(t *testing.T) {
	b, _ := newTest(10 * time.Second)
	if s := b.Snapshot(); s.NextProbeIn != "" {
		t.Fatalf("passive-only backend: %q", s.NextProbeIn)
	}
	b.probing = true
	b.nextProbe = b.now().Add(3 * time.Second)
	if s := b.Snapshot(); s.NextProbeIn != "3s" {
		t.Fatalf("next_probe_in = %q", s.NextProbeIn)
	}
}

func TestRunProbesAndStopsOnCancel(t *testing.T) {
	b := New("b", Options{Interval: 20 * time.Millisecond})
	var calls atomic.Int32
	var fail atomic.Bool
	fail.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		b.Run(ctx, func(context.Context) error {
			calls.Add(1)
			if fail.Load() {
				return errors.New("probe status 503")
			}
			return nil
		})
		close(done)
	}()

	waitFor(t, func() bool { return !b.Snapshot().Healthy })
	if s := b.Snapshot(); s.LastError != "probe status 503" || s.NextProbeIn == "" {
		t.Fatalf("snapshot = %+v", s)
	}
	fail.Store(false)
	waitFor(t, func() bool { return b.Snapshot().Healthy })

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop on cancel")
	}
	n := calls.Load()
	time.Sleep(60 * time.Millisecond)
	if calls.Load() != n {
		t.Fatal("probing continued after cancel")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRunWakesOnPassiveStateChange(t *testing.T) {
	b := New("b", Options{Interval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	go b.Run(ctx, func(context.Context) error { calls.Add(1); return nil })
	waitFor(t, func() bool { return b.Snapshot().NextProbeIn != "" })
	// Passive failure: retryAt = now+1h, but the prober must have re-armed its
	// timer for the new schedule rather than sleep on the stale one.
	b.mu.Lock()
	b.interval = 30 * time.Millisecond // so backoff(1) is short
	b.mu.Unlock()
	b.Failure("boom")
	waitFor(t, func() bool { return calls.Load() > 0 && b.Snapshot().Healthy })
}
