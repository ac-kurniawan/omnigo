package quota

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

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

func TestSyncerFetchErrorMarksUnavailable(t *testing.T) {
	cache := NewCache()
	s := NewSyncer(Options{
		Cache:   cache,
		Targets: fixedTargets(Target{Provider: "cx", Identity: "a"}),
		Fetch: func(context.Context, string, string) (AccountSnapshot, error) {
			return AccountSnapshot{}, errors.New("boom")
		},
	})
	s.PollOnce(context.Background())

	snap, ok := cache.Get("cx", "a")
	if !ok {
		t.Fatal("snapshot missing")
	}
	if snap.Status != StatusUnavailable {
		t.Fatalf("status = %q, want unavailable", snap.Status)
	}
}

// An unreadable quota must never be reported as exhausted: an account that
// cannot be read stays routable, and the dashboard shows it as unavailable.
func TestSyncerUnavailableStaysUnavailable(t *testing.T) {
	cache := NewCache()
	s := NewSyncer(Options{
		Cache:   cache,
		Targets: fixedTargets(Target{Provider: "agy", Identity: "sub-1"}),
		Fetch: func(context.Context, string, string) (AccountSnapshot, error) {
			return AccountSnapshot{Status: StatusUnavailable, Reason: "upstream status 401"}, nil
		},
	})
	s.PollOnce(context.Background())
	snap, ok := cache.Get("agy", "sub-1")
	if !ok || snap.Status != StatusUnavailable {
		t.Fatalf("snapshot = %+v, want unavailable", snap)
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

// A provider may report one window name per group with several windows, so the
// exported window label must stay unique per window or the series overwrite
// each other and drop quota signal for the whole account.
func TestSyncerExportsDistinctWindowLabelsPerGroup(t *testing.T) {
	sink := &recordingSink{}
	s := NewSyncer(Options{
		Cache:   NewCache(),
		Targets: fixedTargets(Target{Provider: "agy", Identity: "sub-1"}),
		Sink:    sink,
		Fetch: func(context.Context, string, string) (AccountSnapshot, error) {
			return AccountSnapshot{Status: StatusAvailable, Windows: []Window{
				{Name: "gemini-weekly", Display: "Gemini Models", WindowMinutes: 10080, UsedPercent: 14.36},
				{Name: "gemini-5h", Display: "Gemini Models", WindowMinutes: 300, UsedPercent: 0},
				{Name: "3p-weekly", Display: "Claude and GPT models", WindowMinutes: 10080, UsedPercent: 0},
				{Name: "3p-5h", Display: "Claude and GPT models", WindowMinutes: 300, UsedPercent: 25},
			}}, nil
		},
	})
	s.PollOnce(context.Background())

	unique := map[string]bool{}
	for _, sample := range sink.samples {
		unique[sample] = true
	}
	if len(unique) != 4 {
		t.Fatalf("distinct sample series = %d, want 4: %v", len(unique), sink.samples)
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
