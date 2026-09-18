package quota

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type recordingDrainer struct {
	mu     sync.Mutex
	drains []drainCall
}

type drainCall struct {
	provider string
	identity string
	cooldown time.Duration
	reason   string
}

func (d *recordingDrainer) MarkQuotaDrained(provider, identity string, cooldown time.Duration, reason string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.drains = append(d.drains, drainCall{provider, identity, cooldown, reason})
}

func (d *recordingDrainer) calls() []drainCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]drainCall(nil), d.drains...)
}

type recordingSink struct {
	mu      sync.Mutex
	samples []string
}

func (s *recordingSink) RecordQuotaSample(sample Sample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.samples = append(s.samples, sample.Provider+"|"+sample.Account+"|"+sample.Window)
}

func (s *recordingSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.samples)
}

func fixedTargets(targets ...Target) TargetsFunc {
	return func() []Target { return targets }
}

func TestSyncerStoresSnapshot(t *testing.T) {
	cache := NewCache()
	now := time.Unix(2_000_000_000, 0)
	s := NewSyncer(Options{
		Cache:   cache,
		Targets: fixedTargets(Target{Provider: "cx", Identity: "a"}),
		Fetch: func(context.Context, string, string) (AccountSnapshot, error) {
			return AccountSnapshot{Provider: "cx", Identity: "a", Status: StatusAvailable, Windows: []Window{{Name: "primary", UsedPercent: 7, ResetAt: now.Add(time.Hour)}}}, nil
		},
		Now: func() time.Time { return now },
	})
	s.PollOnce(context.Background())

	got, ok := cache.Get("cx", "a")
	if !ok {
		t.Fatal("snapshot not cached")
	}
	if got.Status != StatusAvailable || got.ObservedAt.IsZero() {
		t.Fatalf("snapshot = %+v", got)
	}
}

func TestSyncerFillsMissingProviderAndIdentity(t *testing.T) {
	cache := NewCache()
	s := NewSyncer(Options{
		Cache:   cache,
		Targets: fixedTargets(Target{Provider: "agy", Identity: "sub-1"}),
		Fetch: func(context.Context, string, string) (AccountSnapshot, error) {
			return AccountSnapshot{Status: StatusAvailable}, nil
		},
	})
	s.PollOnce(context.Background())
	if _, ok := cache.Get("agy", "sub-1"); !ok {
		t.Fatal("snapshot not attributed to the polled target")
	}
}

func TestSyncerFetchErrorNeverDrains(t *testing.T) {
	cache := NewCache()
	drainer := &recordingDrainer{}
	s := NewSyncer(Options{
		Cache:     cache,
		Targets:   fixedTargets(Target{Provider: "cx", Identity: "a"}),
		Drainer:   drainer,
		AutoDrain: func() bool { return true },
		Fetch: func(context.Context, string, string) (AccountSnapshot, error) {
			return AccountSnapshot{}, errors.New("boom")
		},
	})
	s.PollOnce(context.Background())

	if got := drainer.calls(); len(got) != 0 {
		t.Fatalf("fetch failure drained the account: %+v", got)
	}
	snap, ok := cache.Get("cx", "a")
	if !ok {
		t.Fatal("snapshot missing")
	}
	if snap.Status != StatusUnavailable {
		t.Fatalf("status = %q, want unavailable", snap.Status)
	}
}

// TestSyncerUnavailableNeverDrains is the blackhole-prevention guard: an
// unreadable quota must never remove capacity from the pool.
func TestSyncerUnavailableNeverDrains(t *testing.T) {
	drainer := &recordingDrainer{}
	s := NewSyncer(Options{
		Cache:     NewCache(),
		Targets:   fixedTargets(Target{Provider: "agy", Identity: "sub-1"}),
		Drainer:   drainer,
		AutoDrain: func() bool { return true },
		Fetch: func(context.Context, string, string) (AccountSnapshot, error) {
			return AccountSnapshot{Status: StatusUnavailable, Reason: "upstream status 401"}, nil
		},
	})
	s.PollOnce(context.Background())
	if got := drainer.calls(); len(got) != 0 {
		t.Fatalf("unavailable quota drained the account: %+v", got)
	}
}

func TestSyncerAutoDrainUsesEarliestResetBoundedByMaxCooldown(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	drainer := &recordingDrainer{}
	s := NewSyncer(Options{
		Cache:       NewCache(),
		Targets:     fixedTargets(Target{Provider: "agy", Identity: "sub-1"}),
		Drainer:     drainer,
		AutoDrain:   func() bool { return true },
		MaxCooldown: func() time.Duration { return 30 * time.Minute },
		Now:         func() time.Time { return now },
		Fetch: func(context.Context, string, string) (AccountSnapshot, error) {
			// Upstream reports a 3-day reset on the binding window.
			return AccountSnapshot{
				Status: StatusExhausted,
				Reason: "claude-opus-4-6-thinking",
				Windows: []Window{
					{Name: "gemini-3.8-flash-tiered", UsedPercent: 0, ResetAt: now.Add(2 * time.Hour)},
					{Name: "claude-opus-4-6-thinking", UsedPercent: 100, ResetAt: now.Add(72 * time.Hour)},
				},
			}, nil
		},
	})
	s.PollOnce(context.Background())

	got := drainer.calls()
	if len(got) != 1 {
		t.Fatalf("drains = %+v, want exactly 1", got)
	}
	if got[0].cooldown != 30*time.Minute {
		t.Fatalf("cooldown = %s, want the 30m ceiling (upstream said 72h)", got[0].cooldown)
	}
	if got[0].reason != "claude-opus-4-6-thinking" {
		t.Fatalf("reason = %q", got[0].reason)
	}
}

func TestSyncerAutoDrainHonorsShortReset(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	drainer := &recordingDrainer{}
	s := NewSyncer(Options{
		Cache:       NewCache(),
		Targets:     fixedTargets(Target{Provider: "cx", Identity: "a"}),
		Drainer:     drainer,
		AutoDrain:   func() bool { return true },
		MaxCooldown: func() time.Duration { return 30 * time.Minute },
		Now:         func() time.Time { return now },
		Fetch: func(context.Context, string, string) (AccountSnapshot, error) {
			return AccountSnapshot{Status: StatusExhausted, Windows: []Window{{Name: "primary", UsedPercent: 100, ResetAt: now.Add(10 * time.Minute)}}}, nil
		},
	})
	s.PollOnce(context.Background())

	got := drainer.calls()
	if len(got) != 1 || got[0].cooldown != 10*time.Minute {
		t.Fatalf("drains = %+v, want a single 10m drain", got)
	}
}

func TestSyncerAutoDrainSkipsWhenNoUsableReset(t *testing.T) {
	drainer := &recordingDrainer{}
	s := NewSyncer(Options{
		Cache:     NewCache(),
		Targets:   fixedTargets(Target{Provider: "cx", Identity: "a"}),
		Drainer:   drainer,
		AutoDrain: func() bool { return true },
		Fetch: func(context.Context, string, string) (AccountSnapshot, error) {
			return AccountSnapshot{Status: StatusExhausted, Windows: []Window{{Name: "primary", UsedPercent: 100}}}, nil
		},
	})
	s.PollOnce(context.Background())
	if got := drainer.calls(); len(got) != 0 {
		t.Fatalf("drained without a reset target: %+v", got)
	}
}

func TestSyncerAutoDrainDisabledRecordsButDoesNotDrain(t *testing.T) {
	drainer := &recordingDrainer{}
	cache := NewCache()
	s := NewSyncer(Options{
		Cache:     cache,
		Targets:   fixedTargets(Target{Provider: "cx", Identity: "a"}),
		Drainer:   drainer,
		AutoDrain: func() bool { return false },
		Fetch: func(context.Context, string, string) (AccountSnapshot, error) {
			return AccountSnapshot{Status: StatusExhausted, Windows: []Window{{Name: "primary", UsedPercent: 100, ResetAt: time.Now().Add(time.Hour)}}}, nil
		},
	})
	s.PollOnce(context.Background())

	if got := drainer.calls(); len(got) != 0 {
		t.Fatalf("auto_drain=false drained the account: %+v", got)
	}
	if _, ok := cache.Get("cx", "a"); !ok {
		t.Fatal("snapshot must still be cached with auto-drain off")
	}
}

func TestSyncerRecordsSamplesForWindows(t *testing.T) {
	sink := &recordingSink{}
	s := NewSyncer(Options{
		Cache:   NewCache(),
		Targets: fixedTargets(Target{Provider: "agy", Identity: "sub-1", Account: "105124432875589731973"}),
		Sink:    sink,
		Fetch: func(context.Context, string, string) (AccountSnapshot, error) {
			return AccountSnapshot{Status: StatusAvailable, Windows: []Window{
				{Name: "gemini-3.8-flash-tiered", UsedPercent: 0},
				{Name: "claude-sonnet-4-6", UsedPercent: 100},
			}}, nil
		},
	})
	s.PollOnce(context.Background())

	if got := sink.count(); got != 2 {
		t.Fatalf("samples = %d, want one per window", got)
	}
}

func TestSyncerRecordsStatusWithoutWindows(t *testing.T) {
	sink := &recordingSink{}
	s := NewSyncer(Options{
		Cache:   NewCache(),
		Targets: fixedTargets(Target{Provider: "cx", Identity: "a"}),
		Sink:    sink,
		Fetch: func(context.Context, string, string) (AccountSnapshot, error) {
			return AccountSnapshot{Status: StatusUnavailable}, nil
		},
	})
	s.PollOnce(context.Background())
	if got := sink.count(); got != 1 {
		t.Fatalf("samples = %d, want the account-level status series", got)
	}
}

func TestStatusValueEncoding(t *testing.T) {
	tests := []struct {
		status Status
		want   int64
	}{
		{StatusAvailable, 1},
		{StatusExhausted, 0},
		{StatusUnavailable, -1},
		{"", -1},
	}
	for _, tt := range tests {
		if got := statusValue(tt.status); got != tt.want {
			t.Fatalf("statusValue(%q) = %d, want %d", tt.status, got, tt.want)
		}
	}
}

func TestSyncerRunStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls int
	var mu sync.Mutex
	s := NewSyncer(Options{
		Cache:    NewCache(),
		Interval: func() time.Duration { return time.Millisecond },
		Targets:  fixedTargets(Target{Provider: "cx", Identity: "a"}),
		Jitter:   func(time.Duration) time.Duration { return 0 },
		Fetch: func(context.Context, string, string) (AccountSnapshot, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			cancel()
			return AccountSnapshot{Status: StatusAvailable}, nil
		},
	})
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls == 0 {
		t.Fatal("Run never polled")
	}
}

func TestSyncerPollOnceRespectsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cache := NewCache()
	s := NewSyncer(Options{
		Cache:   cache,
		Targets: fixedTargets(Target{Provider: "cx", Identity: "a"}),
		Fetch: func(context.Context, string, string) (AccountSnapshot, error) {
			t.Fatal("fetch must not run with a cancelled context")
			return AccountSnapshot{}, nil
		},
	})
	s.PollOnce(ctx)
	if len(cache.All()) != 0 {
		t.Fatal("cancelled poll mutated the cache")
	}
}

func TestSyncerNilFetcherOrTargetsIsNoop(t *testing.T) {
	s := NewSyncer(Options{Cache: NewCache()})
	s.PollOnce(context.Background())
}

func TestDefaultJitterIsBoundedByTenPercent(t *testing.T) {
	for range 100 {
		got := defaultJitter(time.Minute)
		if got < 0 || got > 6*time.Second {
			t.Fatalf("jitter = %s, want within [0, 0.1*interval]", got)
		}
	}
	if got := defaultJitter(0); got != 0 {
		t.Fatalf("jitter(0) = %s, want 0", got)
	}
}
