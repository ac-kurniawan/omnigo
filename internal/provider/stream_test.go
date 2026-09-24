package provider

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestIdleGuardCancelsOnStall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	guard := NewIdleGuard(50*time.Millisecond, 0, cancel)
	defer guard.Stop()

	select {
	case <-ctx.Done():
		t.Fatal("context canceled before the idle timeout elapsed")
	case <-time.After(10 * time.Millisecond):
	}

	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("context not canceled after the upstream went silent")
	}
}

// Silence alone cannot bound a stream: an upstream that trickles a byte just
// before every idle deadline would run forever. The budget is the bound that
// actually ends it.
func TestIdleGuardCancelsOnBudgetWhileDataKeepsFlowing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	guard := NewIdleGuard(200*time.Millisecond, 150*time.Millisecond, cancel)
	defer guard.Stop()

	pr, pw := io.Pipe()
	defer pw.Close()
	reader := guard.Wrap(pr)

	// The reader stands in for an upstream body: bytes keep arriving well
	// inside the idle window, so only the budget can end this generation.
	go func() {
		buf := make([]byte, 8)
		for {
			if _, err := reader.Read(buf); err != nil {
				return
			}
		}
	}()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if _, err := pw.Write([]byte("data")); err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	start := time.Now()
	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("trickling stream outlived the budget")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("budget fired after %s, want ~150ms", elapsed)
	}
	got := guard.Err(context.Canceled)
	if !errors.Is(got, ErrUpstreamStall) || !strings.Contains(got.Error(), "generation exceeded") {
		t.Fatalf("guard.Err = %v, want the stream budget named as the bound", got)
	}
}

func TestIdleGuardWithoutBudgetTricklesForever(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	guard := NewIdleGuard(200*time.Millisecond, 0, cancel)
	defer guard.Stop()

	pr, pw := io.Pipe()
	defer pw.Close()
	reader := guard.Wrap(pr)

	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 8)
		for i := 0; i < 6; i++ {
			if _, err := reader.Read(buf); err != nil {
				return
			}
		}
	}()

	for i := 0; i < 6; i++ {
		if _, err := pw.Write([]byte("data")); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		time.Sleep(60 * time.Millisecond)
	}
	<-done

	if err := ctx.Err(); err != nil {
		t.Fatalf("context canceled although no budget was set: %v", err)
	}
}

func TestStreamBudgetAppliesOnlyToStreams(t *testing.T) {
	if got := StreamBudget(10*time.Minute, true); got != 10*time.Minute {
		t.Fatalf("streaming budget = %v, want 10m", got)
	}
	if got := StreamBudget(10*time.Minute, false); got != 0 {
		t.Fatalf("buffered budget = %v, want 0 (bounded by the client timeout)", got)
	}
}

func TestIdleGuardKeepsStreamAliveWhileDataFlows(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	guard := NewIdleGuard(200*time.Millisecond, 0, cancel)
	defer guard.Stop()

	pr, pw := io.Pipe()
	defer pw.Close()
	reader := guard.Wrap(pr)

	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 8)
		for i := 0; i < 5; i++ {
			if _, err := reader.Read(buf); err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	for i := 0; i < 5; i++ {
		if _, err := pw.Write([]byte("data")); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	<-done

	if err := ctx.Err(); err != nil {
		t.Fatalf("context canceled while data kept flowing: %v", err)
	}
}

// A stream that emits faster than idle/2 must not be canceled early; after a
// real silence of idle the guard must still cancel it.
func TestIdleGuardFastStreamCancelsOnlyAfterIdleSilence(t *testing.T) {
	const idle = 200 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	guard := NewIdleGuard(idle, 0, cancel)
	defer guard.Stop()

	pr, pw := io.Pipe()
	defer pw.Close()
	reader := guard.Wrap(pr)

	// Read in the background so writes never block.
	go func() {
		buf := make([]byte, 8)
		for {
			if _, err := reader.Read(buf); err != nil {
				return
			}
		}
	}()

	// Emit faster than idle/2 for longer than idle; if touch reset the timer
	// on every chunk the stream would still survive, but a correct half-window
	// implementation must also not cancel while data is flowing.
	start := time.Now()
	for time.Since(start) < idle+idle/2 {
		if _, err := pw.Write([]byte("x")); err != nil {
			t.Fatalf("write: %v", err)
		}
		time.Sleep(idle / 8)
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("canceled while emitting faster than idle/2: %v", err)
	}

	// Now stop writing. The guard must cancel only after a real silence of
	// idle — not early, and not never.
	silent := time.Now()
	select {
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("context not canceled after the stream went silent")
	}
	elapsed := time.Since(silent)
	// A reset just past the halfway mark leaves about idle/2 of the armed
	// countdown, so the floor is idle/4 rather than idle.
	if elapsed < idle/4 {
		t.Fatalf("canceled after %s of silence, want about %s", elapsed, idle)
	}
	if elapsed > idle+idle {
		t.Fatalf("canceled after %s of silence, want about %s", elapsed, idle)
	}

	got := guard.Err(context.Canceled)
	if !errors.Is(got, ErrUpstreamStall) || !strings.Contains(got.Error(), "no data within") {
		t.Fatalf("guard.Err = %v, want the idle bound named", got)
	}
}
