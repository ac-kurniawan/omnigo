package quota

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"
)

// Sample is one quota observation for metrics export. Status carries the
// tri-state numerically: 1 available, 0 exhausted, -1 unavailable.
type Sample struct {
	Provider       string
	Account        string
	Window         string
	RemainingRatio float64
	Status         int64
	ResetsInSecond float64
}

// SampleSink exports a quota observation. The observability package implements
// it; declaring the interface here keeps this package free of an import of
// internal/observability.
type SampleSink interface {
	RecordQuotaSample(sample Sample)
}

// TargetsFunc enumerates the credentials to poll, as (provider, identity)
// pairs. It is called on every cycle so config and credential reloads are
// picked up without restarting the syncer.
type TargetsFunc func() []Target
type Target struct {
	Provider string
	Identity string
	Account  string // label for metrics: account id when known, else identity
}

// Syncer polls provider quota in the background and stores the snapshots for
// the dashboard. It never changes routing.
type Syncer struct {
	cache    *Cache
	targets  TargetsFunc
	fetch    func(ctx context.Context, provider, identity string) (AccountSnapshot, error)
	sink     SampleSink
	interval func() time.Duration
	now      func() time.Time
	jitter   func(time.Duration) time.Duration

	// failCount backs per-target exponential backoff so a persistently
	// failing private endpoint is probed less often instead of hammering it.
	mu        sync.Mutex
	failCount map[string]int
}

// Options configures a Syncer.
//
// Interval is a function rather than a value so a config reload takes effect
// on the next cycle without restarting the process, matching how the rest of
// the gateway reads live configuration.
type Options struct {
	Cache    *Cache
	Targets  TargetsFunc
	Fetch    func(ctx context.Context, provider, identity string) (AccountSnapshot, error)
	Sink     SampleSink
	Interval func() time.Duration
	// Now and Jitter are test seams; nil selects the real clock and jitter.
	Now    func() time.Time
	Jitter func(time.Duration) time.Duration
}

// maxBackoffShift caps exponential backoff at interval<<4 (5m -> 80m).
const maxBackoffShift = 4

func NewSyncer(opts Options) *Syncer {
	s := &Syncer{
		cache:     opts.Cache,
		targets:   opts.Targets,
		fetch:     opts.Fetch,
		sink:      opts.Sink,
		interval:  opts.Interval,
		now:       opts.Now,
		jitter:    opts.Jitter,
		failCount: make(map[string]int),
	}
	if s.interval == nil {
		s.interval = func() time.Duration { return 5 * time.Minute }
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.jitter == nil {
		s.jitter = defaultJitter
	}
	if s.cache == nil {
		s.cache = NewCache()
	}
	return s
}

// Cache exposes the snapshot store so the dashboard can render it.
func (s *Syncer) Cache() *Cache { return s.cache }

// Run polls until ctx is cancelled. Each cycle is spaced by the configured
// interval plus jitter, so pooled accounts do not stampede the upstream.
func (s *Syncer) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		s.PollOnce(ctx)
		interval := s.interval()
		if interval <= 0 {
			interval = 5 * time.Minute
		}
		timer := time.NewTimer(interval + s.jitter(interval))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// PollOnce performs a single pass over every target. It is exported so the
// dashboard's manual Refresh can reuse the exact same logic.
func (s *Syncer) PollOnce(ctx context.Context) {
	if s.targets == nil || s.fetch == nil {
		return
	}
	for _, target := range s.targets() {
		if ctx.Err() != nil {
			return
		}
		if s.backingOff(target) {
			continue
		}
		s.pollTarget(ctx, target)
	}
}

func (s *Syncer) pollTarget(ctx context.Context, target Target) {
	snapshot, err := s.fetch(ctx, target.Provider, target.Identity)
	key := target.Provider + "/" + target.Identity

	s.mu.Lock()
	if err != nil {
		s.failCount[key]++
	} else {
		delete(s.failCount, key)
	}
	s.mu.Unlock()

	if snapshot.Identity == "" {
		snapshot.Identity = target.Identity
	}
	if snapshot.Provider == "" {
		snapshot.Provider = target.Provider
	}
	if snapshot.ObservedAt.IsZero() {
		snapshot.ObservedAt = s.now()
	}
	if err != nil && snapshot.Status == "" {
		// A fetch failure with no classified snapshot is unknown, never
		// exhausted: an unreadable quota must not drain an account.
		snapshot.Status = StatusUnavailable
		snapshot.Reason = "quota fetch failed"
	}

	s.cache.Put(snapshot)
	s.record(snapshot, target)

}

func (s *Syncer) record(snapshot AccountSnapshot, target Target) {
	if s.sink == nil {
		return
	}
	account := target.Account
	if account == "" {
		account = snapshot.AccountID
	}
	if account == "" {
		account = snapshot.Identity
	}
	status := statusValue(snapshot.Status)

	// With no windows (or an unavailable read) the account-level status is
	// still meaningful, so it is always exported; ratio series are emitted
	// only for windows that actually reported one.
	if len(snapshot.Windows) == 0 {
		s.sink.RecordQuotaSample(Sample{
			Provider: snapshot.Provider,
			Account:  account,
			Status:   status,
		})
		return
	}
	for _, w := range snapshot.Windows {
		resetsIn := 0.0
		if !w.ResetAt.IsZero() {
			if d := w.ResetAt.Sub(s.now()); d > 0 {
				resetsIn = d.Seconds()
			}
		}
		s.sink.RecordQuotaSample(Sample{
			Provider:       snapshot.Provider,
			Account:        account,
			Window:         w.Name,
			RemainingRatio: w.RemainingPercent() / 100,
			Status:         status,
			ResetsInSecond: resetsIn,
		})
	}
}

// statusValue maps the tri-state to the numeric metric encoding.
func statusValue(s Status) int64 {
	switch s {
	case StatusAvailable:
		return 1
	case StatusExhausted:
		return 0
	default:
		return -1
	}
}

// backingOff reports whether a target is currently in exponential backoff
// after consecutive fetch failures.
func (s *Syncer) backingOff(target Target) bool {
	s.mu.Lock()
	failures := s.failCount[target.Provider+"/"+target.Identity]
	s.mu.Unlock()
	if failures == 0 {
		return false
	}
	// One full interval is skipped per failure, capped, so a failing endpoint
	// is retried at most every interval<<maxBackoffShift.
	if failures > maxBackoffShift {
		failures = maxBackoffShift
	}
	return failures > 0 && rand.IntN(1<<failures) != 0
}

// defaultJitter returns a random duration in [0, d/10), spreading poll
// traffic so simultaneous accounts do not align on the same instant.
func defaultJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(d/10) + 1))
}
