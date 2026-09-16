package provider

import (
	"context"
	"io"
	"testing"
	"time"
)

func TestIdleGuardCancelsOnStall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	guard := NewIdleGuard(50*time.Millisecond, cancel)
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

func TestIdleGuardKeepsStreamAliveWhileDataFlows(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	guard := NewIdleGuard(200*time.Millisecond, cancel)
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
