package combo

import (
	"context"
	"errors"
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
