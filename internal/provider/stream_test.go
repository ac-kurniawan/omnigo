package provider

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

func TestStalledHTTP2StreamDoesNotBlockSibling(t *testing.T) {
	const bound = 750 * time.Millisecond
	release := make(chan struct{})
	var conns atomic.Int32
	var first atomic.Bool
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("response writer cannot flush")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: start\n\n")
		flusher.Flush()
		if first.CompareAndSwap(false, true) {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
		_, _ = io.WriteString(w, "data: done\n\n")
		flusher.Flush()
	}))
	upstream.EnableHTTP2 = true
	upstream.Config.HTTP2 = &http.HTTP2Config{MaxConcurrentStreams: 1}
	upstream.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			conns.Add(1)
		}
	}
	upstream.StartTLS()
	defer upstream.Close()
	defer close(release)

	base := upstream.Client().Transport.(*http.Transport)
	base.HTTP2 = &http.HTTP2Config{StrictMaxConcurrentRequests: true}
	client := &http.Client{Transport: base}
	stream := StreamClient(client)

	started := make(chan struct{}, 2)
	errc := make(chan error, 2)
	for range 2 {
		go func() {
			req, err := http.NewRequest(http.MethodGet, upstream.URL, nil)
			if err != nil {
				errc <- err
				return
			}
			started <- struct{}{}
			resp, err := stream.Do(req)
			if err != nil {
				errc <- err
				return
			}
			defer resp.Body.Close()
			_, err = io.ReadAll(resp.Body)
			errc <- err
		}()
	}
	for range 2 {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("stream request did not start")
		}
	}

	timer := time.NewTimer(bound)
	defer timer.Stop()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("first completed stream: %v", err)
		}
	case <-timer.C:
		t.Fatalf("stalled stream blocked its sibling past %s", bound)
	}
	if got := conns.Load(); got < 2 {
		t.Fatalf("HTTPS connections = %d, want a dedicated connection for each in-flight stream", got)
	}

	select {
	case err := <-errc:
		t.Fatalf("both streams completed while the first was still stalled: %v", err)
	default:
	}
}
