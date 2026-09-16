package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
)

// ErrUpstreamStall reports that a streaming upstream sent no data for longer
// than the idle timeout. It is an upstream fault, so combo routing drains the
// target and fails over.
var ErrUpstreamStall = errors.New("upstream stalled: no data within idle timeout")

// StreamClient returns a client with no wall-clock deadline, for streaming
// generations. Total generation time cannot bound a stream: a long answer and
// a dead connection look identical to a clock. IdleGuard bounds silence
// instead, and the transport's ResponseHeaderTimeout bounds time to first byte.
func StreamClient(client *http.Client) *http.Client {
	if client == nil {
		return &http.Client{}
	}
	return &http.Client{Transport: client.Transport, CheckRedirect: client.CheckRedirect, Jar: client.Jar}
}

// ClientFor selects the transport client for a request. Only a stream may run
// without a wall-clock deadline: a non-streaming response is bounded by its
// configured timeout, and a trickling upstream must not hold the request (and
// a concurrency slot) open indefinitely.
func ClientFor(stream, buffered *http.Client, streaming bool) *http.Client {
	if streaming {
		return stream
	}
	return buffered
}

// IdleGuard cancels a generation when the upstream goes silent for longer than
// idle. Wrap the response body with Wrap, then Stop the guard once the body is
// closed. A non-positive idle or nil cancel disables the guard.
type IdleGuard struct {
	idle   time.Duration
	cancel context.CancelFunc

	mu      sync.Mutex
	last    time.Time
	timer   *time.Timer
	stalled bool
}

func NewIdleGuard(idle time.Duration, cancel context.CancelFunc) *IdleGuard {
	g := &IdleGuard{idle: idle, cancel: cancel, last: time.Now()}
	if idle <= 0 || cancel == nil {
		return g
	}
	g.timer = time.AfterFunc(idle, g.onIdle)
	return g
}

func (g *IdleGuard) onIdle() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stalled {
		return
	}
	// Data may have arrived between the fire and this lock; recheck instead of
	// trusting the timer alone.
	if time.Since(g.last) < g.idle {
		g.timer.Reset(g.idle)
		return
	}
	g.stalled = true
	g.cancel()
}

func (g *IdleGuard) touch() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stalled || g.timer == nil {
		return
	}
	g.last = time.Now()
	g.timer.Reset(g.idle)
}

// Wrap restarts the idle countdown whenever bytes arrive from r.
func (g *IdleGuard) Wrap(r io.Reader) io.Reader {
	if g.timer == nil {
		return r
	}
	return &idleReader{reader: r, guard: g}
}

// Stop releases the timer once the response body is no longer read.
func (g *IdleGuard) Stop() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.timer != nil {
		g.timer.Stop()
	}
}

// Stalled reports whether the guard canceled the generation.
func (g *IdleGuard) Stalled() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stalled
}

// Err reports ErrUpstreamStall in place of the context cancellation this guard
// caused, so callers do not see a bare "context canceled" for a dead upstream.
func (g *IdleGuard) Err(err error) error {
	if err != nil && g.Stalled() {
		return ErrUpstreamStall
	}
	return err
}

type idleReader struct {
	reader io.Reader
	guard  *IdleGuard
}

func (r *idleReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		r.guard.touch()
	}
	if err != nil && r.guard.Stalled() {
		// The cancel that aborted this read was ours, so report the stall
		// rather than a bare context cancellation.
		return n, ErrUpstreamStall
	}
	return n, err
}
