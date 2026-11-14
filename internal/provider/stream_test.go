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

// HTTP/2 can no longer carry streaming: the stream transport forces HTTP/1.1,
// so one in-flight stream per connection is the isolation that matters. A
// stalled stream must still not hold its sibling, and the sibling must get its
// own connection instead of queueing behind the stalled one.
func TestStalledStreamDoesNotBlockSibling(t *testing.T) {
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
	upstream.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			conns.Add(1)
		}
	}
	upstream.StartTLS()
	defer upstream.Close()
	defer close(release)

	base := upstream.Client().Transport.(*http.Transport)
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
		t.Fatalf("%d HTTPS connections, want a dedicated connection for each in-flight stream", got)
	}

	select {
	case err := <-errc:
		t.Fatalf("both streams completed while the first was still stalled: %v", err)
	default:
	}
}

// Sequential streams must ride the connection the previous one released.
// Cloning the transport per request paid a TCP and TLS handshake on every
// generation; isolation only needs one connection per in-flight stream.
func TestStreamClientReusesIdleConnection(t *testing.T) {
	var conns atomic.Int32
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: start\n\n")
		w.(http.Flusher).Flush()
	}))
	upstream.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			conns.Add(1)
		}
	}
	upstream.StartTLS()
	defer upstream.Close()

	base := upstream.Client().Transport.(*http.Transport)
	stream := StreamClient(&http.Client{Transport: base})
	for range 4 {
		resp, err := stream.Get(upstream.URL)
		if err != nil {
			t.Fatalf("stream request: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	if got := conns.Load(); got != 1 {
		t.Fatalf("HTTPS connections = %d for 4 sequential streams, want 1 reused connection", got)
	}
}

// A clone of an HTTP/2-capable transport keeps the "h2" ALPN entry that
// configuring HTTP/2 installed on the base. Switching ForceAttemptHTTP2 off
// then left the clone advertising h2 while speaking HTTP/1.1, so an upstream
// that picked h2 answered with SETTINGS frames and the request died as a
// malformed HTTP response. The stream pool must offer HTTP/1.1 alone.
func TestStreamClientNegotiatesHTTP11AgainstHTTP2Upstream(t *testing.T) {
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: x\n\n")
	}))
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	defer upstream.Close()

	base := upstream.Client().Transport.(*http.Transport)
	if !base.ForceAttemptHTTP2 {
		t.Fatal("test upstream client has HTTP/2 disabled")
	}
	stream := StreamClient(&http.Client{Transport: base})
	resp, err := stream.Get(upstream.URL)
	if err != nil {
		t.Fatalf("stream request to an HTTP/2-capable upstream: %v", err)
	}
	resp.Body.Close()
	if resp.ProtoMajor != 1 {
		t.Fatalf("stream negotiated %s, want HTTP/1.1", resp.Proto)
	}

	// The pool is a clone: pinning its ALPN must not reach the caller's
	// transport, which carries every unary request and still speaks HTTP/2.
	unary, err := upstream.Client().Get(upstream.URL)
	if err != nil {
		t.Fatalf("unary request after a stream: %v", err)
	}
	defer unary.Body.Close()
	if unary.ProtoMajor != 2 {
		t.Fatalf("shared transport negotiated %s after a stream, want HTTP/2", unary.Proto)
	}
}

// Every streaming provider draws from one HTTP/1.1 pool per base transport,
// and that pool is separate from the shared unary transport, so shutdown has to
// drain it explicitly. The observable proof is that the connection is gone.
func TestCloseStreamIdleConnectionsDrainsStreamPools(t *testing.T) {
	var conns atomic.Int32
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: x\n\n")
	}))
	upstream.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	upstream.Start()
	defer upstream.Close()

	transport := &http.Transport{}
	stream := StreamClient(&http.Client{Transport: transport})
	fetch := func() {
		resp, err := stream.Get(upstream.URL)
		if err != nil {
			t.Fatalf("stream request: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	fetch()
	fetch()
	if got := conns.Load(); got != 1 {
		t.Fatalf("connections = %d for two sequential streams, want the idle connection reused", got)
	}

	CloseStreamIdleConnections()
	fetch()
	if got := conns.Load(); got != 2 {
		t.Fatalf("connections = %d after draining the pool, want a fresh dial", got)
	}

	// The caller's transport is untouched: closing the streaming pool must not
	// disturb the shared client the rest of the process rides.
	if transport.IdleConnTimeout != 0 {
		t.Fatal("the caller's transport was reconfigured")
	}
}
