package combo

import (
	"context"
	"fmt"
)

type Target struct {
	Provider string
	Model    string
}

type Combo struct {
	Name     string
	Strategy string
	Targets  []Target
}

type DispatchFunc func(ctx context.Context, t Target) error

// Run executes targets in config order, falling back on failure.
// Both priority and fill-first preserve order for MVP; the distinction is
// reserved for quota-fill semantics once quota tracking exists.
func (c Combo) Run(ctx context.Context, dispatch DispatchFunc) (Target, error) {
	var lastErr error
	for _, t := range c.Targets {
		if err := dispatch(ctx, t); err != nil {
			lastErr = err
			continue
		}
		return t, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("combo %q has no targets", c.Name)
	}
	return Target{}, lastErr
}
