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
	// Timeout is the total budget for the whole chain. Positive: every target
	// gets an equal slice of the remaining time, so one stalled target cannot
	// consume the budget of the targets behind it. Zero: no combo-level cap.
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
	for _, target := range targets {
		attemptCtx := ctx
		cancel := func() {}
		if c.Timeout > 0 {
			remaining := c.Timeout - time.Since(attemptStart)
			left := len(targets) - len(failures)
			if left <= 0 {
				left = 1
			}
			slice := remaining / time.Duration(left)
			if slice <= 0 {
				return Target{}, ErrComboTimeout
			}
			attemptCtx, cancel = context.WithTimeout(ctx, slice)
		}
		err := dispatch(attemptCtx, target)
		cancel()
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return Target{}, ctx.Err()
			}
			failures = append(failures, fmt.Sprintf("%s: %v", targetKey(target), err))
			if isBackpressure(err) {
				backpressure++
			}
			if isUpstreamRateLimit(err) {
				rateLimited++
			}
			if tracksFailures && isDrainable(err) {
				c.Tracker.MarkDrained(target, failureCooldown(err, c.drainTTL()), failureReason(err))
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
