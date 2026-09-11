package combo

import (
	"context"
	"errors"
	"testing"
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
