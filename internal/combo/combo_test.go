package combo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
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
