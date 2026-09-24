package combo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

func TestPriorityFallsBackOnFailure(t *testing.T) {
	c := Combo{Name: "auto", Strategy: "priority", Targets: []Target{
		{Provider: "a", Model: "m1"},
		{Provider: "b", Model: "m2"},
	}}
	var calls []string
	dispatch := func(_ context.Context, t Target) error {
		calls = append(calls, t.Provider)
		if t.Provider == "a" {
			return errors.New("boom")
		}
		return nil
	}
	got, err := c.Run(context.Background(), dispatch)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Provider != "b" {
		t.Fatalf("got %q, want b", got.Provider)
	}
	if len(calls) != 2 || calls[0] != "a" || calls[1] != "b" {
		t.Fatalf("calls = %v", calls)
	}
}

func TestStopsAtFirstSuccess(t *testing.T) {
	c := Combo{Name: "auto", Strategy: "priority", Targets: []Target{
		{Provider: "a", Model: "m1"},
		{Provider: "b", Model: "m2"},
	}}
	calls := 0
	dispatch := func(_ context.Context, t Target) error {
		calls++
		return nil
	}
	got, err := c.Run(context.Background(), dispatch)
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != "a" {
		t.Fatalf("got %q, want a", got.Provider)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestFillFirstPreservesOrder(t *testing.T) {
	c := Combo{Name: "auto", Strategy: "fill-first", Targets: []Target{
		{Provider: "a", Model: "m1"},
		{Provider: "b", Model: "m2"},
	}}
	var calls []string
	dispatch := func(_ context.Context, t Target) error {
		calls = append(calls, t.Provider)
		if t.Provider == "a" {
			return errors.New("boom")
		}
		return nil
	}
	got, err := c.Run(context.Background(), dispatch)
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != "b" || len(calls) != 2 {
		t.Fatalf("got %q, calls %v", got.Provider, calls)
	}
}

func TestAllFailReturnsLastError(t *testing.T) {
	c := Combo{Name: "auto", Strategy: "priority", Targets: []Target{
		{Provider: "a", Model: "m1"},
		{Provider: "b", Model: "m2"},
	}}
	dispatch := func(_ context.Context, t Target) error {
		return errors.New("err-" + t.Provider)
	}
	_, err := c.Run(context.Background(), dispatch)
	if err == nil {
		t.Fatal("expected error when all targets fail")
	}
}

func TestFillFirstSkipsDrainedTarget(t *testing.T) {
	tr := NewTracker("")
	t1 := Target{Provider: "a", Model: "m1"}
	t2 := Target{Provider: "b", Model: "m2"}

	tr.MarkDrained(t1, 1*time.Minute, "previously exhausted")

	c := Combo{
		Name:     "auto",
		Strategy: "fill-first",
		Targets:  []Target{t1, t2},
		Tracker:  tr,
		DrainTTL: 1 * time.Minute,
	}

	var called []string
	dispatch := func(_ context.Context, tgt Target) error {
		called = append(called, tgt.Provider)
		return nil
	}

	got, err := c.Run(context.Background(), dispatch)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Provider != "b" {
		t.Fatalf("got provider %q, want b (since a was drained)", got.Provider)
	}
	if len(called) != 1 || called[0] != "b" {
		t.Fatalf("called = %v, want only b", called)
	}
}

type cooldownTestError struct {
	ttl time.Duration
}

func (e cooldownTestError) Error() string           { return "quota exhausted" }
func (e cooldownTestError) Cooldown() time.Duration { return e.ttl }
func (e cooldownTestError) DrainReason() string     { return e.Error() }

func TestFillFirstUsesFailureCooldownForAffectedTarget(t *testing.T) {
	tr := NewTracker("")
	t1 := Target{Provider: "codex", Model: "gpt"}
	t2 := Target{Provider: "other", Model: "gpt"}
	c := Combo{Name: "auto", Strategy: "fill-first", Targets: []Target{t1, t2}, Tracker: tr, DrainTTL: time.Minute}

	got, err := c.Run(context.Background(), func(_ context.Context, target Target) error {
		if target == t1 {
			return fmt.Errorf("request failed: %w", cooldownTestError{ttl: 5 * time.Minute})
		}
		return nil
	})
	if err != nil || got != t2 {
		t.Fatalf("got = %+v, error = %v", got, err)
	}
	remaining := tr.DrainRemaining(t1)
	if remaining < 4*time.Minute+59*time.Second || remaining > 5*time.Minute {
		t.Fatalf("cooldown = %v", remaining)
	}
	if tr.IsDrained(t2) {
		t.Fatal("unrelated target was drained")
	}
	if got := tr.DrainReason(t1); got != "quota exhausted" {
		t.Fatalf("drain reason = %q", got)
	}
}

func TestFillFirstDoesNotPersistRawErrorReason(t *testing.T) {
	tr := NewTracker("")
	t1 := Target{Provider: "a", Model: "m1"}
	t2 := Target{Provider: "b", Model: "m2"}
	secret := "Bearer secret-token"
	c := Combo{Name: "auto", Strategy: "fill-first", Targets: []Target{t1, t2}, Tracker: tr}
	_, err := c.Run(context.Background(), func(_ context.Context, target Target) error {
		if target == t1 {
			return errors.New(secret)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if reason := tr.DrainReason(t1); reason != "upstream failure" || strings.Contains(reason, secret) {
		t.Fatalf("drain reason = %q", reason)
	}
}

func TestFillFirstMarksDrainedOnFailure(t *testing.T) {
	tr := NewTracker("")
	t1 := Target{Provider: "a", Model: "m1"}
	t2 := Target{Provider: "b", Model: "m2"}

	c := Combo{
		Name:     "auto",
		Strategy: "fill-first",
		Targets:  []Target{t1, t2},
		Tracker:  tr,
		DrainTTL: 1 * time.Minute,
	}

	dispatch := func(_ context.Context, tgt Target) error {
		if tgt.Provider == "a" {
			return errors.New("upstream failure")
		}
		return nil
	}

	got, err := c.Run(context.Background(), dispatch)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Provider != "b" {
		t.Fatalf("got provider %q, want b", got.Provider)
	}

	// Verify that t1 was marked drained in tracker
	if !tr.IsDrained(t1) {
		t.Fatalf("target 1 should have been marked drained after failure")
	}
	if got := tr.DrainReason(t1); got != "upstream failure" {
		t.Fatalf("drain reason = %q, want 'upstream failure'", got)
	}
}

func TestReliableSkipsDrainedAndMarksFailures(t *testing.T) {
	tr := NewTracker("")
	t1 := Target{Provider: "a", Model: "m1"}
	t2 := Target{Provider: "b", Model: "m2"}
	t3 := Target{Provider: "c", Model: "m3"}
	tr.MarkDrained(t1, time.Minute, "previous failure")

	c := Combo{Name: "safe", Strategy: "reliable", Targets: []Target{t1, t2, t3}, Tracker: tr, DrainTTL: time.Minute}
	var called []string
	got, err := c.Run(context.Background(), func(_ context.Context, target Target) error {
		called = append(called, target.Provider)
		if target == t2 {
			return errors.New("upstream status 503")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got != t3 || len(called) != 2 || called[0] != "b" || called[1] != "c" {
		t.Fatalf("got %+v, called %v", got, called)
	}
	if !tr.IsDrained(t2) {
		t.Fatal("failed target should be drained")
	}
}

func TestReliableAllFailReturnsAggregateError(t *testing.T) {
	c := Combo{Name: t.Name() + "-safe", Strategy: "reliable", Targets: []Target{
		{Provider: "a", Model: "m1"},
		{Provider: "b", Model: "m2"},
	}}
	_, err := c.Run(context.Background(), func(_ context.Context, target Target) error {
		return fmt.Errorf("failure-%s", target.Provider)
	})
	if err == nil {
		t.Fatal("expected aggregate error")
	}
	want := fmt.Sprintf(`combo %q failed: a/m1: failure-a; b/m2: failure-b`, c.Name)
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
}

func TestRoundRobinConcurrentRotation(t *testing.T) {
	tr := NewTracker("")
	targets := []Target{
		{Provider: "a", Model: "m1"},
		{Provider: "b", Model: "m2"},
		{Provider: "c", Model: "m3"},
	}
	tr.MarkDrained(targets[1], time.Minute, "unavailable")
	c := Combo{Name: t.Name() + "-balanced", Strategy: "round-robin", Targets: targets, Tracker: tr, DrainTTL: time.Minute}

	const requests = 100
	counts := make(map[string]int)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := c.Run(context.Background(), func(_ context.Context, target Target) error { return nil })
			if err != nil {
				t.Errorf("Run: %v", err)
				return
			}
			mu.Lock()
			counts[got.Provider]++
			mu.Unlock()
		}()
	}
	wg.Wait()

	if counts["a"] != requests/2 || counts["c"] != requests/2 || counts["b"] != 0 {
		t.Fatalf("counts = %v, want a=50 c=50 b=0", counts)
	}
}

func TestReliableDrainTTLExpires(t *testing.T) {
	tr := NewTracker("")
	t1 := Target{Provider: "a", Model: "m1"}
	t2 := Target{Provider: "b", Model: "m2"}
	c := Combo{Name: "safe", Strategy: "reliable", Targets: []Target{t1, t2}, Tracker: tr, DrainTTL: 10 * time.Millisecond}
	_, err := c.Run(context.Background(), func(_ context.Context, target Target) error {
		if target == t1 {
			return errors.New("temporary failure")
		}
		return nil
	})
	if err != nil || !tr.IsDrained(t1) {
		t.Fatalf("Run = %v, drained = %v", err, tr.IsDrained(t1))
	}
	time.Sleep(20 * time.Millisecond)
	var first Target
	_, err = c.Run(context.Background(), func(_ context.Context, target Target) error {
		first = target
		return nil
	})
	if err != nil || first != t1 {
		t.Fatalf("Run = %v, first target after TTL = %+v", err, first)
	}
}

func TestRoundRobinFailedTargetDrainsAndFallsBack(t *testing.T) {
	tr := NewTracker("")
	t1 := Target{Provider: "a", Model: "m1"}
	t2 := Target{Provider: "b", Model: "m2"}
	c := Combo{Name: t.Name() + "-balanced", Strategy: "round-robin", Targets: []Target{t1, t2}, Tracker: tr, DrainTTL: time.Minute}

	var called []Target
	got, err := c.Run(context.Background(), func(_ context.Context, target Target) error {
		called = append(called, target)
		if target == t1 {
			return errors.New("connection refused")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got != t2 || len(called) != 2 || !tr.IsDrained(t1) {
		t.Fatalf("got %+v, called %+v, drained=%v", got, called, tr.IsDrained(t1))
	}
}

func TestPriorityDoesNotMarkDrainedOrSkip(t *testing.T) {
	tr := NewTracker("")
	t1 := Target{Provider: "a", Model: "m1"}
	t2 := Target{Provider: "b", Model: "m2"}

	tr.MarkDrained(t1, 1*time.Minute, "ignore me")

	c := Combo{
		Name:     "auto",
		Strategy: "priority",
		Targets:  []Target{t1, t2},
		Tracker:  tr,
	}

	var called []string
	dispatch := func(_ context.Context, tgt Target) error {
		called = append(called, tgt.Provider)
		return nil
	}

	got, err := c.Run(context.Background(), dispatch)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// priority is stateless; it tries t1 first regardless of drain state
	if got.Provider != "a" {
		t.Fatalf("got %q, want a (priority ignores drain)", got.Provider)
	}
	if len(called) != 1 || called[0] != "a" {
		t.Fatalf("called = %v, want a", called)
	}
}

func TestRunDoesNotDrainOnContextCanceled(t *testing.T) {
	tr := NewTracker("")
	t1 := Target{Provider: "a", Model: "m1"}
	t2 := Target{Provider: "b", Model: "m2"}
	c := Combo{Name: "safe", Strategy: "reliable", Targets: []Target{t1, t2}, Tracker: tr, DrainTTL: time.Minute}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var called []Target
	_, err := c.Run(ctx, func(_ context.Context, target Target) error {
		called = append(called, target)
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if tr.IsDrained(t1) {
		t.Fatal("t1 should NOT be drained on context.Canceled")
	}
	if len(called) != 1 {
		t.Fatalf("expected exactly 1 call (no failover on canceled context), got %d", len(called))
	}
}

type mockClientError struct {
	status int
}

func (e mockClientError) Error() string   { return fmt.Sprintf("upstream status %d", e.status) }
func (e mockClientError) HTTPStatus() int { return e.status }

type mockUpstreamRateLimit struct{ mockClientError }

func (mockUpstreamRateLimit) RetryAfter() time.Duration { return time.Minute }

func TestUpstreamRateLimitIsNotGatewayBackpressure(t *testing.T) {
	err := mockUpstreamRateLimit{mockClientError{status: http.StatusTooManyRequests}}
	if isBackpressure(err) {
		t.Fatal("upstream 429 with Retry-After classified as gateway backpressure")
	}
}

func TestReliableAllUpstreamRateLimitedPreserves429(t *testing.T) {
	c := Combo{Name: "rate-limited", Strategy: "reliable", Targets: []Target{{Provider: "a", Model: "m1"}, {Provider: "b", Model: "m2"}}}
	_, err := c.Run(context.Background(), func(context.Context, Target) error {
		return mockUpstreamRateLimit{mockClientError{status: http.StatusTooManyRequests}}
	})
	var status interface{ HTTPStatus() int }
	if !errors.As(err, &status) || status.HTTPStatus() != http.StatusTooManyRequests {
		t.Fatalf("error = %v, want preserved upstream 429", err)
	}
	var retry interface{ RetryAfter() time.Duration }
	if !errors.As(err, &retry) || retry.RetryAfter() != time.Minute {
		t.Fatalf("error = %v, want preserved Retry-After", err)
	}
}

func TestReliableMixed429FailuresPreserves429(t *testing.T) {
	c := Combo{Name: "rate-limited", Strategy: "reliable", Targets: []Target{{Provider: "a", Model: "m1"}, {Provider: "b", Model: "m2"}}}
	_, err := c.Run(context.Background(), func(_ context.Context, target Target) error {
		if target.Provider == "a" {
			return &provider.ProviderBusyError{Limit: 1}
		}
		return mockUpstreamRateLimit{mockClientError{status: http.StatusTooManyRequests}}
	})
	var status interface{ HTTPStatus() int }
	if !errors.As(err, &status) || status.HTTPStatus() != http.StatusTooManyRequests {
		t.Fatalf("error = %v, want preserved 429", err)
	}
}

func TestRunDoesNotDrainOnClientError(t *testing.T) {
	tr := NewTracker("")
	t1 := Target{Provider: "a", Model: "m1"}
	t2 := Target{Provider: "b", Model: "m2"}
	c := Combo{Name: "safe", Strategy: "reliable", Targets: []Target{t1, t2}, Tracker: tr, DrainTTL: time.Minute}

	_, err := c.Run(context.Background(), func(_ context.Context, target Target) error {
		if target == t1 {
			return mockClientError{status: 400}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tr.IsDrained(t1) {
		t.Fatal("t1 should NOT be drained on 400 Bad Request")
	}
}

// A combo budget is a chain deadline, not a slice per target. The first
// attempt may use the whole budget; once it is spent the chain stops instead
// of handing the next target a shrinking remainder.
func TestComboTimeoutBoundsTheChain(t *testing.T) {
	c := Combo{
		Name:     "safe",
		Strategy: "reliable",
		Timeout:  200 * time.Millisecond,
		Targets: []Target{
			{Provider: "a", Model: "m1"},
			{Provider: "b", Model: "m2"},
			{Provider: "c", Model: "m3"},
		},
	}

	var attempts int
	start := time.Now()
	_, err := c.Run(context.Background(), func(ctx context.Context, target Target) error {
		if _, ok := ctx.Deadline(); !ok {
			t.Errorf("target %s got no deadline", target.Provider)
			return errors.New("no deadline")
		}
		attempts++
		<-ctx.Done()
		return ctx.Err()
	})
	elapsed := time.Since(start)

	if attempts != 1 {
		t.Fatalf("%d targets were attempted; the first one spent the whole budget", attempts)
	}
	if elapsed > 600*time.Millisecond {
		t.Fatalf("chain ran %s, want it bounded near 200ms", elapsed)
	}
	if !errors.Is(err, ErrComboTimeout) {
		t.Fatalf("err = %v, want ErrComboTimeout", err)
	}
}

// A healthy generation that needs 25s must finish inside a 60s combo budget
// even when two more targets sit behind it.
func TestComboTimeoutGivesEachAttemptTheFullBudget(t *testing.T) {
	c := Combo{
		Name:     "safe",
		Strategy: "reliable",
		Timeout:  60 * time.Second,
		Targets: []Target{
			{Provider: "a", Model: "m1"},
			{Provider: "b", Model: "m2"},
			{Provider: "c", Model: "m3"},
		},
	}

	_, err := c.Run(context.Background(), func(ctx context.Context, target Target) error {
		if target.Provider != "a" {
			t.Fatalf("later target %s was attempted before the first finished", target.Provider)
		}
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("first target got no deadline")
		}
		if remaining := time.Until(deadline); remaining < 25*time.Second {
			t.Fatalf("first target deadline is %s away, want at least 25s of a 60s budget", remaining)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("first target should have been allowed to finish, got %v", err)
	}
}

// A target that simply runs out the gateway's own combo deadline is not an
// upstream failure, so the next request must still be able to select it.
func TestComboDeadlineDoesNotDrainTarget(t *testing.T) {
	tr := NewTracker("")
	t1 := Target{Provider: "a", Model: "m1"}
	t2 := Target{Provider: "b", Model: "m2"}
	c := Combo{
		Name:     "safe",
		Strategy: "reliable",
		Timeout:  40 * time.Millisecond,
		DrainTTL: time.Minute,
		Tracker:  tr,
		Targets:  []Target{t1, t2},
	}

	_, err := c.Run(context.Background(), func(ctx context.Context, _ Target) error {
		<-ctx.Done()
		return ctx.Err()
	})
	if !errors.Is(err, ErrComboTimeout) && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the combo deadline", err)
	}
	if tr.IsDrained(t1) || tr.IsDrained(t2) {
		t.Fatal("a combo-imposed deadline drained a target")
	}

	var called []string
	got, err := c.Run(context.Background(), func(_ context.Context, target Target) error {
		called = append(called, target.Provider)
		if target == t1 {
			return nil
		}
		return errors.New("should not be reached")
	})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if got != t1 || len(called) != 1 || called[0] != "a" {
		t.Fatalf("second run called %v and returned %+v, want only a/m1", called, got)
	}
}

// A real upstream stall is still an upstream fault and must drain.
func TestUpstreamStallStillDrains(t *testing.T) {
	tr := NewTracker("")
	t1 := Target{Provider: "a", Model: "m1"}
	t2 := Target{Provider: "b", Model: "m2"}
	c := Combo{Name: "safe", Strategy: "fill-first", Targets: []Target{t1, t2}, Tracker: tr, DrainTTL: time.Minute}

	_, err := c.Run(context.Background(), func(_ context.Context, target Target) error {
		if target == t1 {
			return fmt.Errorf("silent: %w", provider.ErrUpstreamStall)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("fallback failed: %v", err)
	}
	if !tr.IsDrained(t1) {
		t.Fatal("upstream stall did not drain the target")
	}
}

func TestSingle5xxDoesNotDrainButRepeatedDoes(t *testing.T) {
	tr := NewTracker("")
	t1 := Target{Provider: "a", Model: "m1"}
	t2 := Target{Provider: "b", Model: "m2"}
	c := Combo{Name: "safe", Strategy: "fill-first", Targets: []Target{t1, t2}, Tracker: tr, DrainTTL: time.Minute}
	boom := mockClientError{status: http.StatusBadGateway}

	if _, err := c.Run(context.Background(), func(_ context.Context, target Target) error {
		if target == t1 {
			return boom
		}
		return nil
	}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if tr.IsDrained(t1) {
		t.Fatal("a single 502 drained the target")
	}

	for range 4 {
		_, _ = c.Run(context.Background(), func(_ context.Context, target Target) error {
			if target == t1 {
				return boom
			}
			return nil
		})
	}
	if !tr.IsDrained(t1) {
		t.Fatal("repeated 502s did not drain the target")
	}
}

func Test429DrainsImmediately(t *testing.T) {
	tr := NewTracker("")
	t1 := Target{Provider: "a", Model: "m1"}
	t2 := Target{Provider: "b", Model: "m2"}
	c := Combo{Name: "safe", Strategy: "reliable", Targets: []Target{t1, t2}, Tracker: tr, DrainTTL: time.Minute}

	_, err := c.Run(context.Background(), func(_ context.Context, target Target) error {
		if target == t1 {
			return mockUpstreamRateLimit{mockClientError{status: http.StatusTooManyRequests}}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("fallback failed: %v", err)
	}
	if !tr.IsDrained(t1) {
		t.Fatal("429 did not drain the target immediately")
	}
}

func TestLastHealthyTargetIsNeverDrained(t *testing.T) {
	tr := NewTracker("")
	only := Target{Provider: "a", Model: "m1"}
	c := Combo{Name: "safe", Strategy: "reliable", Targets: []Target{only}, Tracker: tr, DrainTTL: time.Minute}

	_, err := c.Run(context.Background(), func(context.Context, Target) error {
		return mockClientError{status: http.StatusBadGateway}
	})
	if err == nil {
		t.Fatal("expected the upstream error")
	}
	if tr.IsDrained(only) {
		t.Fatal("the last healthy target was drained")
	}

	for range 6 {
		_, _ = c.Run(context.Background(), func(context.Context, Target) error {
			return mockClientError{status: http.StatusBadGateway}
		})
	}
	if tr.IsDrained(only) {
		t.Fatal("repeated 502s drained the last healthy target")
	}
}

func Test429BackoffGrowsWithoutRetryAfter(t *testing.T) {
	tr := NewTracker("")
	t1 := Target{Provider: "a", Model: "m1"}
	t2 := Target{Provider: "b", Model: "m2"}
	c := Combo{Name: "safe", Strategy: "fill-first", Targets: []Target{t1, t2}, Tracker: tr, DrainTTL: time.Minute}
	limited := mockClientError{status: http.StatusTooManyRequests}

	fail := func(_ context.Context, target Target) error {
		if target == t1 {
			return limited
		}
		return nil
	}
	_, _ = c.Run(context.Background(), fail)
	first := tr.DrainRemaining(t1)
	tr.Clear(t1)

	for range 3 {
		_, _ = c.Run(context.Background(), fail)
		tr.Clear(t1)
	}
	_, _ = c.Run(context.Background(), fail)
	later := tr.DrainRemaining(t1)
	if later <= first+time.Second {
		t.Fatalf("429 cooldown did not grow: first %s, later %s", first, later)
	}
}

// A blip 502 is retried once on the same target. The second attempt succeeding
// means the target is not drained and the caller sees the success.
func Test502RetriesOnceOnTheSameTarget(t *testing.T) {
	tr := NewTracker("")
	t1 := Target{Provider: "a", Model: "m1"}
	t2 := Target{Provider: "b", Model: "m2"}
	c := Combo{Name: "safe", Strategy: "fill-first", Targets: []Target{t1, t2}, Tracker: tr, DrainTTL: time.Minute}

	var calls int
	got, err := c.Run(context.Background(), func(_ context.Context, target Target) error {
		if target != t1 {
			t.Fatalf("fell over to %s before retrying a", target.Provider)
		}
		calls++
		if calls == 1 {
			return mockClientError{status: http.StatusBadGateway}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("retry should have succeeded, got %v", err)
	}
	if got != t1 {
		t.Fatalf("returned %+v, want a/m1", got)
	}
	if calls != 2 {
		t.Fatalf("target was called %d times, want 2", calls)
	}
	if tr.IsDrained(t1) {
		t.Fatal("a recovered 502 drained the target")
	}
}

func Test429IsNotRetriedOnTheSameTarget(t *testing.T) {
	tr := NewTracker("")
	t1 := Target{Provider: "a", Model: "m1"}
	t2 := Target{Provider: "b", Model: "m2"}
	c := Combo{Name: "safe", Strategy: "reliable", Targets: []Target{t1, t2}, Tracker: tr, DrainTTL: time.Minute}

	var calls int
	_, err := c.Run(context.Background(), func(_ context.Context, target Target) error {
		if target == t1 {
			calls++
			return mockClientError{status: http.StatusTooManyRequests}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("fallback failed: %v", err)
	}
	if calls != 1 {
		t.Fatalf("429 was retried %d times on the same target", calls)
	}
}
