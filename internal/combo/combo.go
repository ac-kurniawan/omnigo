package combo

import (
	"context"
	"fmt"
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

// Run executes targets according to the combo strategy.
// For "fill-first", non-drained targets are tried in order; if all are drained,
// drained targets are tried as fallback. Any failed attempt marks that target as drained.
// For "priority", targets are always executed strictly in configured order without state.
func (c Combo) Run(ctx context.Context, dispatch DispatchFunc) (Target, error) {
	targets := c.Targets
	isFillFirst := c.Strategy == "fill-first" && c.Tracker != nil

	if isFillFirst {
		var healthy []Target
		var drained []Target
		for _, t := range c.Targets {
			if c.Tracker.IsDrained(t) {
				drained = append(drained, t)
			} else {
				healthy = append(healthy, t)
			}
		}
		// Try healthy targets first, fallback to drained targets if all are drained
		targets = append(healthy, drained...)
	}

	var lastErr error
	for _, t := range targets {
		if err := dispatch(ctx, t); err != nil {
			lastErr = err
			if isFillFirst {
				ttl := c.DrainTTL
				if ttl <= 0 {
					ttl = 60 * time.Second
				}
				c.Tracker.MarkDrained(t, ttl)
			}
			continue
		}
		return t, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("combo %q has no targets", c.Name)
	}
	return Target{}, lastErr
}
