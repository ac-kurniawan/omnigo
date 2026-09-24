package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// ErrUpstreamStall reports that a streaming upstream stopped making progress
// within its bounds: it went silent past the idle timeout, or the whole
// generation outlived the stream timeout. Either way it is an upstream fault,
// so combo routing drains the target and fails over. The bound that tripped is
// named in the error returned by IdleGuard.Err.
var ErrUpstreamStall = errors.New("upstream stalled")

// StreamClient returns a client with no wall-clock deadline, for streaming
// generations. Total generation time cannot bound a stream: a long answer and
// a dead connection look identical to a clock. IdleGuard bounds silence
// instead, and its stream budget bounds a generation that never finishes.
func StreamClient(client *http.Client) *http.Client {
	if client == nil {
		return &http.Client{}
	}
	return &http.Client{Transport: client.Transport, CheckRedirect: client.CheckRedirect, Jar: client.Jar}
}

// StreamBudget returns the total wall-clock budget for one generation: the
// configured stream timeout when streaming, and 0 (unbounded) otherwise, since
// a buffered exchange is already bounded by its client timeout. A non-positive
// total leaves a stream unbounded, so only silence is bounded.
func StreamBudget(total time.Duration, streaming bool) time.Duration {
	if !streaming {
		return 0
	}
	return total
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

// IdleGuard cancels a generation that stops making progress: the upstream went
// silent for longer than idle, or the whole generation outlived budget (when
// budget is positive). Silence alone cannot bound a stream that trickles bytes
// forever; a budget can, while leaving a long generation free of a wall-clock
// cap. Wrap the response body with Wrap, then Stop the guard once the body is
// closed. A nil cancel disables the guard, a non-positive idle disables the
// silence bound, and a non-positive budget disables the total bound.
type IdleGuard struct {
	idle   time.Duration
	budget time.Duration
	cancel context.CancelFunc

	mu       sync.Mutex
	last     time.Time
	timer    *time.Timer
	total    *time.Timer
	stallErr error
}

func NewIdleGuard(idle, budget time.Duration, cancel context.CancelFunc) *IdleGuard {
	g := &IdleGuard{idle: idle, budget: budget, cancel: cancel, last: time.Now()}
	if cancel == nil {
		return g
	}
	if idle > 0 {
		g.timer = time.AfterFunc(idle, g.onIdle)
	}
	if budget > 0 {
		g.total = time.AfterFunc(budget, g.onBudget)
	}
	return g
}

func (g *IdleGuard) onIdle() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stallErr != nil {
		return
	}
	// Data may have arrived between the fire and this lock; recheck instead of
	// trusting the timer alone.
	if time.Since(g.last) < g.idle {
		g.timer.Reset(g.idle)
		return
	}
	g.stall(fmt.Errorf("%w: no data within %s", ErrUpstreamStall, g.idle))
}

func (g *IdleGuard) onBudget() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stallErr != nil {
		return
	}
	g.stall(fmt.Errorf("%w: generation exceeded %s", ErrUpstreamStall, g.budget))
}

// stall records cause and cancels the generation. Callers hold g.mu.
func (g *IdleGuard) stall(cause error) {
	g.stallErr = cause
	g.cancel()
}

func (g *IdleGuard) touch() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stallErr != nil || g.timer == nil {
		return
	}
	// A fast stream resets the timer many times a second. Skip the runtime
	// reset until the countdown has run past its halfway point; a stall still
	// fires after a full idle of silence, and onIdle rechecks a late timer.
	if since := time.Since(g.last); since < g.idle/2 {
		return
	}
	g.last = time.Now()
	g.timer.Reset(g.idle)
}

// Wrap restarts the idle countdown whenever bytes arrive from r.
func (g *IdleGuard) Wrap(r io.Reader) io.Reader {
	if g.timer == nil && g.total == nil {
		return r
	}
	return &idleReader{reader: r, guard: g}
}

// Stop releases the timers once the response body is no longer read.
func (g *IdleGuard) Stop() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.timer != nil {
		g.timer.Stop()
	}
	if g.total != nil {
		g.total.Stop()
	}
}

// Stalled reports whether the guard canceled the generation.
func (g *IdleGuard) Stalled() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stallErr != nil
}

// Err reports the stall this guard caused, naming the bound that tripped,
// instead of the bare context cancellation the caller observed, so a dead
// upstream is never reported as a cancelled client.
func (g *IdleGuard) Err(err error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err != nil && g.stallErr != nil {
		return g.stallErr
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
		return n, r.guard.Err(err)
	}
	return n, err
}
