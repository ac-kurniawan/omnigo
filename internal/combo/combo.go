package combo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
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
}

type DispatchFunc func(ctx context.Context, t Target) error

type drainReasoner interface {
	DrainReason() string
}

var roundRobinCounters sync.Map

func (c Combo) Run(ctx context.Context, dispatch DispatchFunc) (Target, error) {
	targets := c.orderedTargets()
	tracksFailures := (c.Strategy == "fill-first" || c.Strategy == "reliable" || c.Strategy == "round-robin") && c.Tracker != nil

	var failures []string
	var lastErr error
	for _, target := range targets {
		if err := dispatch(ctx, target); err != nil {
			lastErr = err
			failures = append(failures, fmt.Sprintf("%s: %v", targetKey(target), err))
			if tracksFailures {
				c.Tracker.MarkDrained(target, failureCooldown(err, c.drainTTL()), failureReason(err))
			}
			continue
		}
		return target, nil
	}
	if len(failures) == 0 {
		return Target{}, fmt.Errorf("combo %q has no healthy targets", c.Name)
	}
	if c.Strategy == "reliable" || c.Strategy == "round-robin" {
		return Target{}, fmt.Errorf("combo %q failed: %s", c.Name, strings.Join(failures, "; "))
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
