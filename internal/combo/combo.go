package combo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ac-kurniawan/omnigo/internal/provider"
)

type Target struct {
	Provider string
	Model    string
}

type Combo struct {
	Name     string
	Strategy string
	Targets  []Target
	Tracker  *Tracker
	DrainTTL time.Duration
	// Timeout is the wall-clock budget for the whole chain. Each attempt gets
	// whatever remains of it, not an equal slice, so a healthy generation is
	// not cancelled just because more targets sit behind it. The chain stops
	// once the budget is spent. Zero means no combo-level cap.
	Timeout time.Duration
}

type DispatchFunc func(ctx context.Context, t Target) error

type drainReasoner interface {
	DrainReason() string
}

// ErrComboTimeout means the wall-clock budget shared by a combo chain was
// exhausted before any target completed.
var ErrComboTimeout = errors.New("combo timeout exceeded")

var roundRobinCounters sync.Map

func (c Combo) Run(ctx context.Context, dispatch DispatchFunc) (Target, error) {
	targets := c.orderedTargets()
	tracksFailures := (c.Strategy == "fill-first" || c.Strategy == "reliable" || c.Strategy == "round-robin") && c.Tracker != nil

	var failures []string
	var lastErr error
	backpressure := 0
	rateLimited := 0
	attemptStart := time.Now()
	for i, target := range targets {
		if c.Timeout > 0 && time.Since(attemptStart) >= c.Timeout {
			return Target{}, errors.Join(ErrComboTimeout, lastErr)
		}
		err := c.attempt(ctx, attemptStart, target, dispatch)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return Target{}, ctx.Err()
			}
			// The attempt context itself expired, so the budget is spent. That
			// deadline is one this process imposed: the target stays selectable
			// and the chain stops. A DeadlineExceeded an upstream returned on
			// its own leaves the attempt context alive and still fails over.
			if errors.Is(err, context.DeadlineExceeded) && c.Timeout > 0 && time.Since(attemptStart) >= c.Timeout {
				return Target{}, errors.Join(ErrComboTimeout, err)
			}
			failures = append(failures, fmt.Sprintf("%s: %v", targetKey(target), err))
			if isBackpressure(err) {
				backpressure++
			}
			if isUpstreamRateLimit(err) {
				rateLimited++
			}
			if tracksFailures && isDrainable(err) && c.shouldDrain(target, err, healthySiblings(targets, i)) {
				c.Tracker.MarkDrained(target, c.cooldownFor(target, err), failureReason(err))
			}
			continue
		}
		return target, nil
	}
	if c.Timeout > 0 && time.Since(attemptStart) >= c.Timeout {
		lastErr = errors.Join(ErrComboTimeout, lastErr)
	}
	if len(failures) == 0 {
		return Target{}, fmt.Errorf("combo %q has no healthy targets", c.Name)
	}
	// Every target rejected with a 429-class error, whether local saturation or
	// upstream rate limiting. Preserve the typed last error so the API returns
	// 429 + Retry-After rather than erasing it in aggregation.
	if backpressure+rateLimited == len(failures) {
		return Target{}, lastErr
	}
	if c.Strategy == "reliable" || c.Strategy == "round-robin" {
		err := fmt.Errorf("combo %q failed: %s", c.Name, strings.Join(failures, "; "))
		if errors.Is(lastErr, ErrComboTimeout) {
			err = errors.Join(ErrComboTimeout, err)
		}
		return Target{}, err
	}
	return Target{}, lastErr
}

// attempt runs one target. A 502, 503, or 504 is tried once more on the same
// target before the caller decides whether to drain it; 429 and request-caused
// 4xx are not. The combo deadline cancels the retry.
func (c Combo) attempt(ctx context.Context, started time.Time, target Target, dispatch DispatchFunc) error {
	err := c.dispatchOnce(ctx, started, target, dispatch)
	if err == nil || !isTransientGateway(err) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return err
	}
	if c.Timeout > 0 && time.Since(started) >= c.Timeout {
		return err
	}
	return c.dispatchOnce(ctx, started, target, dispatch)
}

func (c Combo) dispatchOnce(parent context.Context, started time.Time, target Target, dispatch DispatchFunc) error {
	ctx, cancel := c.attemptContext(parent, started)
	defer cancel()
	err := dispatch(ctx, target)
	// Only a deadline of the context this combo created is the combo budget.
	// An upstream that returns context.DeadlineExceeded on its own is a
	// failure of that target and must still fail over.
	if ctx.Err() == nil {
		return err
	}
	if err == nil {
		return ctx.Err()
	}
	return errors.Join(err, ctx.Err())
}

// attemptContext gives the attempt whatever remains of the combo budget. The
// parent context is left untouched, so a child deadline is distinguishable
// from a client cancel.
func (c Combo) attemptContext(parent context.Context, started time.Time) (context.Context, context.CancelFunc) {
	if c.Timeout <= 0 {
		return parent, func() {}
	}
	remaining := c.Timeout - time.Since(started)
	if remaining <= 0 {
		ctx, cancel := context.WithCancel(parent)
		cancel()
		return ctx, func() {}
	}
	return context.WithTimeout(parent, remaining)
}

func (c Combo) orderedTargets() []Target {
	if c.Strategy != "fill-first" && c.Strategy != "reliable" && c.Strategy != "round-robin" {
		return c.Targets
	}

	healthy := make([]Target, 0, len(c.Targets))
	drained := make([]Target, 0, len(c.Targets))
	for _, target := range c.Targets {
		if c.Tracker != nil && c.Tracker.IsDrained(target) {
			drained = append(drained, target)
		} else {
			healthy = append(healthy, target)
		}
	}
	if c.Strategy == "fill-first" {
		return append(healthy, drained...)
	}
	if c.Strategy != "round-robin" || len(healthy) < 2 {
		return healthy
	}

	counter, _ := roundRobinCounters.LoadOrStore(c.Name, &atomic.Uint64{})
	start := int(counter.(*atomic.Uint64).Add(1)-1) % len(healthy)
	rotated := make([]Target, 0, len(healthy))
	rotated = append(rotated, healthy[start:]...)
	return append(rotated, healthy[:start]...)
}

func failureReason(err error) string {
	var reasoner drainReasoner
	if errors.As(err, &reasoner) {
		return reasoner.DrainReason()
	}
	return "upstream failure"
}

func failureCooldown(err error, fallback time.Duration) time.Duration {
	var cooldown interface{ Cooldown() time.Duration }
	if errors.As(err, &cooldown) && cooldown.Cooldown() > 0 {
		return cooldown.Cooldown()
	}
	return fallback
}

func (c Combo) drainTTL() time.Duration {
	if c.DrainTTL > 0 {
		return c.DrainTTL
	}
	return 60 * time.Second
}

func isDrainable(err error) bool {
	var checker interface{ Drainable() bool }
	if errors.As(err, &checker) {
		return checker.Drainable()
	}
	var status interface{ HTTPStatus() int }
	if errors.As(err, &status) {
		code := status.HTTPStatus()
		if code == 400 || code == 404 || code == 422 {
			return false
		}
	}
	return true
}

// isBackpressure reports gateway-side saturation (local concurrency limit)
// rather than an upstream 429 carrying Retry-After.
func isBackpressure(err error) bool {
	var busy *provider.ProviderBusyError
	return errors.As(err, &busy)
}

func isUpstreamRateLimit(err error) bool {
	if isBackpressure(err) {
		return false
	}
	var status interface{ HTTPStatus() int }
	return errors.As(err, &status) && status.HTTPStatus() == http.StatusTooManyRequests
}

// transientWindow is how long repeated 5xx and stalls have to accumulate
// before a target is parked. One blip inside the window does not drain it.
const transientWindow = time.Minute

// transientStrikes is the number of 5xx or stall failures inside
// transientWindow that parks a target. A single one does not.
const transientStrikes = 5

// rateLimitBase is the first cooldown applied to a 429 that carries no
// Retry-After. Each further 429 doubles it, up to rateLimitCap.
const rateLimitBase = 5 * time.Second
const rateLimitCap = 60 * time.Second

var (
	strikesMu sync.Mutex
	strikes   = map[string][]time.Time{}
	rateHits  = map[string]int{}
)

func httpStatusOf(err error) (int, bool) {
	var status interface{ HTTPStatus() int }
	if errors.As(err, &status) {
		return status.HTTPStatus(), true
	}
	return 0, false
}

func isTransientGateway(err error) bool {
	code, ok := httpStatusOf(err)
	return ok && (code == http.StatusBadGateway || code == http.StatusServiceUnavailable || code == http.StatusGatewayTimeout)
}

func isImmediateDrain(err error) bool {
	if isBackpressure(err) {
		return false
	}
	code, ok := httpStatusOf(err)
	if ok && (code == http.StatusUnauthorized || code == http.StatusTooManyRequests) {
		return true
	}
	var quota interface{ Cooldown() time.Duration }
	return errors.As(err, &quota)
}

// healthySiblings counts targets after this one that the chain can still try.
// Draining the last healthy target would hide the upstream error behind
// "no healthy targets", so it is never done.
func healthySiblings(targets []Target, index int) int {
	if index >= len(targets) {
		return 0
	}
	return len(targets) - index - 1
}

func (c Combo) shouldDrain(target Target, err error, siblings int) bool {
	if siblings <= 0 {
		return false
	}
	// 401, 429, quota exhaustion, and a real upstream stall are faults of the
	// target, not one blip. Park it now.
	if isImmediateDrain(err) || errors.Is(err, provider.ErrUpstreamStall) {
		return true
	}
	// Only 5xx is given the benefit of a repeat. Anything else that is still
	// drainable (a refused connection, a transport error) parks immediately,
	// which is what it did before the strike gate existed.
	code, ok := httpStatusOf(err)
	if !ok || code < 500 || code >= 600 {
		return true
	}
	return c.recordTransient(target) >= transientStrikes
}

func (c Combo) recordTransient(target Target) int {
	key := c.Name + "\x00" + targetKey(target)
	now := time.Now()
	cutoff := now.Add(-transientWindow)
	strikesMu.Lock()
	defer strikesMu.Unlock()
	kept := strikes[key][:0]
	for _, at := range strikes[key] {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	kept = append(kept, now)
	strikes[key] = kept
	return len(kept)
}

func (c Combo) cooldownFor(target Target, err error) time.Duration {
	if isUpstreamRateLimit(err) {
		var retry interface{ RetryAfter() time.Duration }
		if errors.As(err, &retry) && retry.RetryAfter() > 0 {
			return retry.RetryAfter()
		}
		return c.rateLimitCooldown(target)
	}
	return failureCooldown(err, c.drainTTL())
}

func (c Combo) rateLimitCooldown(target Target) time.Duration {
	key := c.Name + "\x00" + targetKey(target)
	strikesMu.Lock()
	n := rateHits[key]
	rateHits[key] = n + 1
	strikesMu.Unlock()
	cooldown := rateLimitBase
	for range n {
		if cooldown >= rateLimitCap/2 {
			return rateLimitCap
		}
		cooldown *= 2
	}
	if cooldown > rateLimitCap {
		return rateLimitCap
	}
	return cooldown
}
