package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/ac-kurniawan/omnigo/internal/config"
	"github.com/ac-kurniawan/omnigo/internal/provider"
)

// Metrics records gateway outcomes for observability. Implementations must be
// safe for concurrent use. NewRouter normalizes a nil Metrics to a no-op, so
// call sites never guard against nil.
//
// The request context is passed through so an implementation can read the
// authenticated client key from it: the instrumentation middleware installs the
// identity before the handler runs.
type Metrics interface {
	// RecordProviderRequest counts a provider dispatch by outcome.
	RecordProviderRequest(ctx context.Context, provider, model, result string)
	// RecordCombinationAttempt counts a combo routing attempt by outcome.
	RecordCombinationAttempt(ctx context.Context, combo, result string)
	// RecordTokenUsage adds the token counts one completed upstream response
	// reported. combo is empty for a direct provider call.
	RecordTokenUsage(ctx context.Context, providerName, model, account, combo string, usage provider.TokenUsage)
}

type noopMetrics struct{}

func (noopMetrics) RecordProviderRequest(context.Context, string, string, string) {}
func (noopMetrics) RecordCombinationAttempt(context.Context, string, string)      {}
func (noopMetrics) RecordTokenUsage(context.Context, string, string, string, string, provider.TokenUsage) {
}

// Bounded dispatch result labels. Cardinality is fixed by this set, never by
// client input.
const (
	resultSuccess       = "success"
	resultFailure       = "failure"
	resultClientAbort   = "client_abort"
	resultUpstreamStall = "upstream_stall"
	resultBackpressure  = "backpressure"
	resultRateLimited   = "rate_limited"
	resultStreamFailed  = "stream_failed"
	resultUpstreamError = "upstream_error"
	resultUnavailable   = "unavailable"
)

// orNoop normalizes a nil Metrics to a no-op recorder so call sites never guard
// against nil.
func orNoop(m Metrics) Metrics {
	if m == nil {
		return noopMetrics{}
	}
	return m
}

// knownModelLabel returns model only when it is a model the operator actually
// configured for that provider; otherwise "other". Direct requests are already
// refused unless the model is listed, and combo failures can name a model that
// is only in the disabled list, so membership in either list — not the shape
// of the string — is what bounds this label.
func knownModelLabel(cfg *config.Config, providerName, model string) string {
	for _, pc := range cfg.Providers {
		if pc.Name != providerName {
			continue
		}
		for _, m := range pc.Models {
			if m == model {
				return model
			}
		}
		for _, m := range pc.DisabledModels {
			if m == model {
				return model
			}
		}
		break
	}
	return "other"
}

// classifyProviderError maps a provider failure to one of the bounded result
// labels. committed reports that part of the response had already reached the
// client, and clientGone reports that the client aborted the request: neither
// is an upstream fault.
func classifyProviderError(err error, committed, clientGone bool) string {
	if err == nil {
		return resultSuccess
	}
	if clientGone {
		return resultClientAbort
	}
	if errors.Is(err, provider.ErrUpstreamStall) {
		return resultUpstreamStall
	}
	// ProviderBusyError is local concurrency saturation. Upstream 429 errors
	// may also advertise Retry-After, but remain rate-limited outcomes.
	var busy *provider.ProviderBusyError
	if errors.As(err, &busy) {
		return resultBackpressure
	}
	var status interface{ HTTPStatus() int }
	if errors.As(err, &status) && status.HTTPStatus() == http.StatusTooManyRequests {
		return resultRateLimited
	}
	if committed {
		return resultStreamFailed
	}
	return resultUpstreamError
}
