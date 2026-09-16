package provider

import (
	"context"
	"net/http"
	"time"
)

// ProviderBusyError reports that the provider's local concurrency limit is
// saturated. It is a gateway-side backpressure signal, not an upstream fault:
// combo routing fails over to the next target without draining this one, and
// direct callers receive 429 with Retry-After instead of 502.
type ProviderBusyError struct {
	Limit int
}

func (e *ProviderBusyError) Error() string {
	return "provider concurrency limit reached"
}

func (e *ProviderBusyError) HTTPStatus() int { return http.StatusTooManyRequests }

// Drainable reports false so a saturated provider is not skipped for the whole
// drain TTL after the burst clears.
func (e *ProviderBusyError) Drainable() bool { return false }

// RetryAfter tells the client when to retry: one full slot turnover is not
// observable, so advertise a short fixed backoff.
func (e *ProviderBusyError) RetryAfter() time.Duration { return time.Second }

// ConcurrencyLimiter bounds in-flight requests to a provider with a semaphore.
type ConcurrencyLimiter struct {
	sem chan struct{}
}

// NewConcurrencyLimiter creates a limiter; limit <= 0 disables it (returns nil).
func NewConcurrencyLimiter(limit int) *ConcurrencyLimiter {
	if limit <= 0 {
		return nil
	}
	return &ConcurrencyLimiter{sem: make(chan struct{}, limit)}
}

// Acquire takes a slot, or reports ProviderBusyError when none is free. It never
// blocks: queueing would add tail latency the combo failover already absorbs.
func (l *ConcurrencyLimiter) Acquire(ctx context.Context) (func(), error) {
	if l == nil {
		return func() {}, nil
	}
	select {
	case l.sem <- struct{}{}:
		return func() { <-l.sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		return nil, &ProviderBusyError{Limit: cap(l.sem)}
	}
}

type limitedProvider struct {
	Provider
	limiter *ConcurrencyLimiter
}

// WithConcurrencyLimit wraps p so at most limit requests run concurrently.
func WithConcurrencyLimit(p Provider, limit int) Provider {
	if limit <= 0 || p == nil {
		return p
	}
	return &limitedProvider{Provider: p, limiter: NewConcurrencyLimiter(limit)}
}

func (lp *limitedProvider) ChatCompletion(ctx context.Context, req ChatRequest, w http.ResponseWriter) error {
	release, err := lp.limiter.Acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	return lp.Provider.ChatCompletion(ctx, req, w)
}
